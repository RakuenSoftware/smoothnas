// Package compose drives real `docker compose` (v2) against the
// smoothnas-runtime (LXC2Docker) socket, so a SmoothNAS plugin can BE a
// docker-compose project instead of a bespoke tierd manifest+orchestrator
// (see docs/proposals/pending/plugins-11-via-docker-compose.md).
//
// The orchestration graph (services, depends_on, health-gating, profiles,
// --scale) is compose's, done client-side against a faithful Docker-engine
// (LXC2Docker) — tierd does NOT re-implement it. tierd keeps only the
// smoothnas-specific concerns compose does not own: tiered-volume resolution,
// the cross-project host-port guard, image pins, config/.env, and treating
// `docker compose ps` as the source of truth for container state.
//
// Design decisions locked by the plugins-11 design roundtable (2026-07-01):
//   - Pin compose v2 (the Go plugin) and ASSERT the version before every op:
//     v1 (python) differs in flag + `ps --format json` semantics.
//   - Pin DOCKER_HOST to the smoothnas socket; never inherit an operator's.
//   - Pull is an EXPLICIT step (own status/errors); `up` uses --pull never so
//     a missing pinned image fails loudly rather than compose auto-pulling.
package compose

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// DefaultSocketPath mirrors runtime.DefaultSocketPath — compose targets the
// smoothnas-runtime socket via DOCKER_HOST so it never hits an operator Docker.
const DefaultSocketPath = "/run/smoothnas-runtime/docker.sock"

// MinComposeMajor is the pinned major version. tierd requires compose v2.
const MinComposeMajor = 2

// ErrComposeVersion is returned when the resolved `docker compose` is too old
// (v1 / python) or unparseable — tierd refuses to drive an unpinned CLI.
var ErrComposeVersion = errors.New("compose: requires docker compose v2 (the Go plugin)")

// Runner executes a `docker compose` invocation. Production uses ExecRunner;
// tests inject a fake so the adapter is unit-testable without a live engine.
type Runner interface {
	Run(ctx context.Context, env []string, args ...string) (stdout, stderr []byte, err error)
}

// Adapter is a thin, version-asserted wrapper around `docker compose`, pinned
// to the smoothnas-runtime socket. Safe for concurrent use by multiple
// goroutines (it holds no per-call state).
type Adapter struct {
	socketPath string
	runner     Runner
}

// New builds an Adapter. An empty socketPath defaults to the smoothnas socket;
// a nil runner defaults to ExecRunner (real `docker compose`).
func New(socketPath string, r Runner) *Adapter {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	if r == nil {
		r = ExecRunner{}
	}
	return &Adapter{socketPath: socketPath, runner: r}
}

// Project identifies a compose project: its name (compose -p) and the ordered
// override files that compose them. WorkingDir anchors relative paths; Profiles
// map to compose --profile (the conditional-service mechanism, client-side);
// EnvFile is the rendered operator config (--env-file).
type Project struct {
	Name       string
	Files      []string // -f, in merge order (base first, overrides last)
	WorkingDir string
	Profiles   []string
	EnvFile    string
	SecretEnv  map[string]string // injected into the `up` subprocess env only
}

// baseArgs is the leading `docker compose -p <name> -f ... --profile ...`
// common to every project op.
func (a *Adapter) baseArgs(p Project) []string {
	args := []string{"compose", "-p", p.Name}
	for _, f := range p.Files {
		args = append(args, "-f", f)
	}
	for _, pr := range p.Profiles {
		args = append(args, "--profile", pr)
	}
	if p.EnvFile != "" {
		args = append(args, "--env-file", p.EnvFile)
	}
	return args
}

// env pins DOCKER_HOST to the smoothnas socket. We do NOT inherit an operator's
// DOCKER_HOST (which could point compose at a different engine). PATH etc. are
// inherited so the `docker` binary resolves.
func (a *Adapter) env() []string {
	out := []string{"DOCKER_HOST=unix://" + a.socketPath}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "DOCKER_HOST=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Version parses `docker compose version` and returns the major version,
// erroring (ErrComposeVersion) if it is < MinComposeMajor or unparseable.
// Call once at startup and/or before a project op so a host CLI swap can't
// silently change flag/output semantics.
func (a *Adapter) Version(ctx context.Context) (major int, err error) {
	out, stderr, err := a.runner.Run(ctx, a.env(), "compose", "version", "--format", "json")
	if err != nil {
		return 0, fmt.Errorf("compose version: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &v); err != nil {
		return 0, fmt.Errorf("%w: unparseable version output: %v", ErrComposeVersion, err)
	}
	major, err = parseMajor(v.Version)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrComposeVersion, err)
	}
	if major < MinComposeMajor {
		return major, fmt.Errorf("%w: found v%d", ErrComposeVersion, major)
	}
	return major, nil
}

// parseMajor extracts the leading integer of a "v2.29.7" / "2.29.7" string.
func parseMajor(s string) (int, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	dot := strings.IndexByte(s, '.')
	if dot > 0 {
		s = s[:dot]
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("compose: cannot parse major from %q", s)
	}
	return n, nil
}

// Pull is the EXPLICIT pre-pull step (roundtable: separate pull errors from up
// errors). Callers pull before Up; Up then runs --pull never so a still-missing
// image fails loudly instead of compose silently auto-pulling a fallback.
func (a *Adapter) Pull(ctx context.Context, p Project) error {
	args := append(a.baseArgs(p), "pull")
	if _, stderr, err := a.runner.Run(ctx, a.env(), args...); err != nil {
		return fmt.Errorf("compose pull %s: %w: %s", p.Name, err, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// Up brings the project up detached with --pull never (images must be present;
// call Pull first). Returns the raw compose output for the caller to log.
func (a *Adapter) Up(ctx context.Context, p Project) error {
	args := append(a.baseArgs(p), "up", "-d", "--pull", "never")
	// Secrets are added to THIS subprocess env only (for ${KEY} interpolation),
	// never to a file / the compose project / ps / config.
	env := a.env()
	for k, v := range p.SecretEnv {
		env = append(env, k+"="+v)
	}
	if _, stderr, err := a.runner.Run(ctx, env, args...); err != nil {
		return fmt.Errorf("compose up %s: %w: %s", p.Name, err, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// Logs returns the tail of the project's aggregated compose logs (no follow).
func (a *Adapter) Logs(ctx context.Context, p Project, tail int) ([]byte, error) {
	if tail <= 0 {
		tail = 200
	}
	args := append(a.baseArgs(p), "logs", "--no-color", "--tail", strconv.Itoa(tail))
	out, stderr, err := a.runner.Run(ctx, a.env(), args...)
	if err != nil {
		return nil, fmt.Errorf("compose logs %s: %w: %s", p.Name, err, strings.TrimSpace(string(stderr)))
	}
	return out, nil
}

// Stop stops the project's containers WITHOUT removing them (compose stop) —
// the lifecycle Stop op (vs Down/Teardown which removes on uninstall).
func (a *Adapter) Stop(ctx context.Context, p Project) error {
	args := append(a.baseArgs(p), "stop")
	if _, stderr, err := a.runner.Run(ctx, a.env(), args...); err != nil {
		return fmt.Errorf("compose stop %s: %w: %s", p.Name, err, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// Down stops and removes the project's containers and networks. When
// removeVolumes is true it also drops anonymous volumes (-v); tiered/named
// volumes are managed by tierd's bind-resolution, not compose down.
func (a *Adapter) Down(ctx context.Context, p Project, removeVolumes bool) error {
	args := append(a.baseArgs(p), "down")
	if removeVolumes {
		args = append(args, "-v")
	}
	if _, stderr, err := a.runner.Run(ctx, a.env(), args...); err != nil {
		return fmt.Errorf("compose down %s: %w: %s", p.Name, err, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// PsEntry is one container row from `docker compose ps --format json`. Only the
// fields tierd needs for state-of-truth + the host-port guard are decoded.
type PsEntry struct {
	Name       string      `json:"Name"`
	Service    string      `json:"Service"`
	State      string      `json:"State"`  // "running", "exited", ...
	Health     string      `json:"Health"` // "healthy"/"unhealthy"/"" (starting)
	Publishers []Publisher `json:"Publishers"`
}

// Publisher is a published port mapping (for the cross-project host-port guard).
type Publisher struct {
	URL           string `json:"URL"`
	TargetPort    int    `json:"TargetPort"`
	PublishedPort int    `json:"PublishedPort"`
	Protocol      string `json:"Protocol"`
}

// Ps returns the project's containers as compose sees them. This is tierd's
// SOURCE OF TRUTH for container state (the DB is a cache synced from here).
// compose v2 emits NDJSON (one object per line); we tolerate an array too.
func (a *Adapter) Ps(ctx context.Context, p Project) ([]PsEntry, error) {
	args := append(a.baseArgs(p), "ps", "--all", "--format", "json")
	out, stderr, err := a.runner.Run(ctx, a.env(), args...)
	if err != nil {
		return nil, fmt.Errorf("compose ps %s: %w: %s", p.Name, err, strings.TrimSpace(string(stderr)))
	}
	return parsePs(out)
}

func parsePs(out []byte) ([]PsEntry, error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, nil
	}
	// Array form: `[{...},{...}]`.
	if out[0] == '[' {
		var arr []PsEntry
		if err := json.Unmarshal(out, &arr); err != nil {
			return nil, fmt.Errorf("compose ps: parse array: %w", err)
		}
		return arr, nil
	}
	// NDJSON form: one object per line.
	var entries []PsEntry
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e PsEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("compose ps: parse line: %w", err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("compose ps: scan: %w", err)
	}
	return entries, nil
}

// ExecRunner runs the real `docker compose` binary.
type ExecRunner struct{}

// Run executes `docker <args...>` with the given env, capturing stdout/stderr.
func (ExecRunner) Run(ctx context.Context, env []string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// UpRemoveOrphans is `up` plus --remove-orphans: it reconciles the running set to
// the (regenerated) compose, so scaling DOWN removes the dropped per-instance
// services' containers while their tier-bound _work host dirs (binds) persist.
func (a *Adapter) UpRemoveOrphans(ctx context.Context, p Project) error {
	args := append(a.baseArgs(p), "up", "-d", "--pull", "never", "--remove-orphans")
	env := a.env()
	for k, v := range p.SecretEnv {
		env = append(env, k+"="+v)
	}
	if _, stderr, err := a.runner.Run(ctx, env, args...); err != nil {
		return fmt.Errorf("compose up --remove-orphans %s: %w: %s", p.Name, err, strings.TrimSpace(string(stderr)))
	}
	return nil
}
