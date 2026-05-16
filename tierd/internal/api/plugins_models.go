package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin"
)

const pluginModelInstallTag = "plugin-model-install"

type installModelRequest struct {
	URL      string `json:"url"`
	Filename string `json:"filename,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Start    *bool  `json:"start,omitempty"`
}

type installModelResponse struct {
	JobID string `json:"jobId"`
	Tag   string `json:"tag"`
}

type modelInstallTarget struct {
	HostDir       string
	Destination   string
	ContainerPath string
	Filename      string
}

type pluginModelClientError struct {
	status int
	code   string
	msg    string
}

func (e *pluginModelClientError) Error() string { return e.msg }

func newPluginModelClientError(status int, code, msg string) error {
	return &pluginModelClientError{status: status, code: code, msg: msg}
}

func (h *PluginsHandler) installModel(w http.ResponseWriter, r *http.Request, name string) {
	var req installModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}

	rec, err := h.store.Get(name)
	if err != nil {
		if errors.Is(err, plugin.ErrPluginNotFound) {
			jsonErrorCoded(w, "plugin not found", http.StatusNotFound, "plugins.not_found")
			return
		}
		serverError(w, err)
		return
	}

	target, err := resolveModelInstallTarget(rec, req.URL, req.Filename)
	if err != nil {
		writePluginModelError(w, err)
		return
	}
	if req.SHA256 != "" {
		if _, err := normaliseSHA256(req.SHA256); err != nil {
			writePluginModelError(w, err)
			return
		}
	}
	if h.lifecycle == nil {
		jsonErrorCoded(w, "runtime not configured", http.StatusServiceUnavailable, "plugins.runtime_unavailable")
		return
	}

	shouldStart := true
	if req.Start != nil {
		shouldStart = *req.Start
	}
	jobID := jobs.StartTagged(pluginModelInstallTag)
	jobs.UpdateResult(jobID, map[string]string{
		"plugin":    name,
		"modelPath": target.ContainerPath,
		"filename":  target.Filename,
	})

	go func() {
		result, err := h.runModelInstallJob(context.Background(), jobID, name, req.URL, req.SHA256, target, shouldStart)
		if err != nil {
			jobs.Fail(jobID, err)
			return
		}
		jobs.Complete(jobID, result)
	}()

	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(installModelResponse{
		JobID: jobID,
		Tag:   pluginModelInstallTag,
	})
}

func writePluginModelError(w http.ResponseWriter, err error) {
	var ce *pluginModelClientError
	if errors.As(err, &ce) {
		jsonErrorCoded(w, ce.msg, ce.status, ce.code)
		return
	}
	serverError(w, err)
}

func (h *PluginsHandler) runModelInstallJob(
	ctx context.Context,
	jobID string,
	pluginName string,
	rawURL string,
	rawSHA256 string,
	target modelInstallTarget,
	shouldStart bool,
) (map[string]string, error) {
	jobs.UpdateProgress(jobID, "Downloading model...")
	if err := downloadModel(ctx, rawURL, rawSHA256, target, func(msg string) {
		jobs.UpdateProgress(jobID, msg)
	}); err != nil {
		return nil, err
	}

	jobs.UpdateProgress(jobID, "Registering model...")
	rec, err := h.store.Get(pluginName)
	if err != nil {
		return nil, err
	}
	cfg := configRowsToMap(rec.Config)
	cfg["MODEL_PATH"] = target.ContainerPath
	if err := h.store.ReplaceConfig(pluginName, cfg); err != nil {
		return nil, fmt.Errorf("update MODEL_PATH: %w", err)
	}

	if shouldStart {
		if h.lifecycle == nil {
			return nil, fmt.Errorf("runtime not configured")
		}
		jobs.UpdateProgress(jobID, "Preparing runtime...")
		if err := h.lifecycle.Materialise(ctx, pluginName); err != nil {
			return nil, fmt.Errorf("materialise plugin: %w", err)
		}
		updated, err := h.store.Get(pluginName)
		if err != nil {
			return nil, err
		}
		if updated.Plugin.State != plugin.StateRunning {
			jobs.UpdateProgress(jobID, "Starting plugin...")
			if err := h.lifecycle.Start(ctx, pluginName); err != nil {
				return nil, fmt.Errorf("start plugin: %w", err)
			}
		}
	}

	jobs.UpdateProgress(jobID, "Ready")
	return map[string]string{
		"plugin":    pluginName,
		"modelPath": target.ContainerPath,
		"filename":  target.Filename,
	}, nil
}

func configRowsToMap(rows []plugin.ConfigRow) map[string]string {
	out := make(map[string]string, len(rows)+1)
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out
}

func resolveModelInstallTarget(rec *plugin.PluginRecord, rawURL, rawFilename string) (modelInstallTarget, error) {
	if err := validateModelURL(rawURL); err != nil {
		return modelInstallTarget{}, err
	}

	filename, err := modelFilenameFromRequest(rawURL, rawFilename)
	if err != nil {
		return modelInstallTarget{}, err
	}

	volume, ok := findModelVolume(rec)
	if !ok {
		return modelInstallTarget{}, newPluginModelClientError(
			http.StatusConflict,
			"plugins.models.volume_missing",
			"plugin does not expose a /models volume",
		)
	}
	hostDir := firstModelHostPath(volume)
	if hostDir == "" {
		return modelInstallTarget{}, newPluginModelClientError(
			http.StatusConflict,
			"plugins.models.volume_unresolved",
			"plugin models volume has no resolved host path",
		)
	}

	dest := filepath.Join(hostDir, filename)
	if filepath.Dir(dest) != filepath.Clean(hostDir) {
		return modelInstallTarget{}, newPluginModelClientError(
			http.StatusBadRequest,
			"plugins.models.filename_invalid",
			"model filename is invalid",
		)
	}
	if _, err := os.Stat(dest); err == nil {
		return modelInstallTarget{}, newPluginModelClientError(
			http.StatusConflict,
			"plugins.models.exists",
			"model file already exists",
		)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return modelInstallTarget{}, fmt.Errorf("stat model destination: %w", err)
	}

	return modelInstallTarget{
		HostDir:       hostDir,
		Destination:   dest,
		ContainerPath: path.Join(volume.BindPath, filename),
		Filename:      filename,
	}, nil
}

func validateModelURL(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u == nil {
		return newPluginModelClientError(http.StatusBadRequest, "plugins.models.url_invalid", "model URL is invalid")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return newPluginModelClientError(http.StatusBadRequest, "plugins.models.url_invalid", "model URL must use http or https")
	}
	if u.Host == "" {
		return newPluginModelClientError(http.StatusBadRequest, "plugins.models.url_invalid", "model URL is missing a host")
	}
	return nil
}

func modelFilenameFromRequest(rawURL, rawFilename string) (string, error) {
	filename := strings.TrimSpace(rawFilename)
	if filename == "" {
		u, _ := url.Parse(strings.TrimSpace(rawURL))
		filename = path.Base(u.Path)
		if unescaped, err := url.PathUnescape(filename); err == nil {
			filename = unescaped
		}
	}
	if !safeModelFilename(filename) {
		return "", newPluginModelClientError(
			http.StatusBadRequest,
			"plugins.models.filename_invalid",
			"model filename must be a .gguf filename without path separators",
		)
	}
	return filename, nil
}

func safeModelFilename(filename string) bool {
	if filename == "" || filename == "." || filename == "/" {
		return false
	}
	if strings.Contains(filename, "/") || strings.Contains(filename, `\`) {
		return false
	}
	if filepath.Base(filename) != filename || path.Base(filename) != filename {
		return false
	}
	if strings.Contains(filename, "..") {
		return false
	}
	if !strings.HasSuffix(strings.ToLower(filename), ".gguf") {
		return false
	}
	for _, r := range filename {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func findModelVolume(rec *plugin.PluginRecord) (plugin.VolumeRow, bool) {
	for _, v := range rec.Volumes {
		if v.BindPath == "/models" {
			return v, true
		}
	}
	for _, v := range rec.Volumes {
		if v.Name == "models" {
			return v, true
		}
	}
	return plugin.VolumeRow{}, false
}

func firstModelHostPath(v plugin.VolumeRow) string {
	if p := v.Paths[1]; p != "" {
		return p
	}
	for _, p := range v.Paths {
		if p != "" {
			return p
		}
	}
	return ""
}

func downloadModel(ctx context.Context, rawURL, rawSHA256 string, target modelInstallTarget, progress func(string)) error {
	if err := os.MkdirAll(target.HostDir, 0o750); err != nil {
		return fmt.Errorf("create model directory: %w", err)
	}

	tmp := filepath.Join(target.HostDir, fmt.Sprintf(".%s.%d.download", target.Filename, time.Now().UnixNano()))
	defer os.Remove(tmp) //nolint:errcheck // only exists on failure paths after success rename

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(rawURL), nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download model: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("download model: HTTP %d", resp.StatusCode)
	}

	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("create temporary model file: %w", err)
	}

	hasher := sha256.New()
	written, err := copyWithProgress(out, resp.Body, hasher, resp.ContentLength, progress)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("close model file: %w", closeErr)
	}
	if written == 0 {
		return fmt.Errorf("downloaded model is empty")
	}

	if rawSHA256 != "" {
		want, err := normaliseSHA256(rawSHA256)
		if err != nil {
			return err
		}
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("model checksum mismatch: got sha256:%s", got)
		}
	}

	if _, err := os.Stat(target.Destination); err == nil {
		return fmt.Errorf("model file already exists")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat model destination: %w", err)
	}
	if err := os.Rename(tmp, target.Destination); err != nil {
		return fmt.Errorf("install model file: %w", err)
	}
	return nil
}

func copyWithProgress(dst io.Writer, src io.Reader, hasher hash.Hash, total int64, progress func(string)) (int64, error) {
	writer := io.MultiWriter(dst, hasher)
	buf := make([]byte, 1024*1024)
	var written int64
	lastProgress := time.Now()
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := writer.Write(buf[:n]); err != nil {
				return written, fmt.Errorf("write model file: %w", err)
			}
			written += int64(n)
			if progress != nil && time.Since(lastProgress) >= time.Second {
				progress(formatDownloadProgress(written, total))
				lastProgress = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return written, fmt.Errorf("read model download: %w", readErr)
		}
	}
	if progress != nil {
		progress(formatDownloadProgress(written, total))
	}
	return written, nil
}

func formatDownloadProgress(written, total int64) string {
	if total > 0 {
		return fmt.Sprintf("Downloading model... %.1f / %.1f GiB", gib(written), gib(total))
	}
	return fmt.Sprintf("Downloading model... %.1f GiB", gib(written))
}

func gib(v int64) float64 {
	return float64(v) / (1024 * 1024 * 1024)
}

func normaliseSHA256(raw string) (string, error) {
	s := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(raw), "sha256:"))
	if len(s) != 64 {
		return "", newPluginModelClientError(http.StatusBadRequest, "plugins.models.sha256_invalid", "sha256 must be 64 hex characters")
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", newPluginModelClientError(http.StatusBadRequest, "plugins.models.sha256_invalid", "sha256 must be hex")
	}
	return s, nil
}
