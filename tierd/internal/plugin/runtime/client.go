// Package runtime is a small Docker Engine API client that tierd uses
// to talk to LXC2Docker over a private unix socket. It implements only
// the endpoints the plugin lifecycle needs (containers, images, events,
// logs, ping, info) and deliberately avoids the official Docker SDK,
// which pulls in a much larger dependency surface than tierd wants.
//
// LXC2Docker accepts both version-prefixed (`/v1.43/...`) and bare
// paths; this client uses bare paths everywhere because the daemon
// is in our release channel and we control its API version.
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultSocketPath is where the smoothnas-runtime systemd unit
// puts its unix socket. Phase 02 of the plugin proposal pins this
// path so tierd does not collide with an operator-installed Docker.
const DefaultSocketPath = "/run/smoothnas-runtime/docker.sock"

// defaultResponseHeaderTimeout guards the normal client against a daemon that
// accepts a request but never replies. Container create bypasses it via the
// slow client (see Client.httpSlow). A package var so tests can shorten it.
var defaultResponseHeaderTimeout = 60 * time.Second

// Client is a Docker Engine API client over a unix socket. Safe for
// concurrent use by multiple goroutines.
type Client struct {
	socketPath string
	http       *http.Client
	// httpSlow has no ResponseHeaderTimeout: container create clones a
	// possibly multi-GB image rootfs before the daemon writes response
	// headers, which routinely exceeds the default 60s. Bounded instead by the
	// caller's context deadline (see CreateContainer).
	httpSlow *http.Client
}

// NewClient constructs a client pointed at the given unix socket.
// Pass DefaultSocketPath in production; tests pass an httptest
// socket path.
func NewClient(socketPath string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}
	transport := func(headerTimeout time.Duration) *http.Transport {
		return &http.Transport{
			DialContext: dial,
			// LXC2Docker terminates idle connections aggressively; keep
			// this small so we don't pile up half-dead conns during
			// reconnection storms.
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			DisableCompression:    true,
			ResponseHeaderTimeout: headerTimeout,
		}
	}
	return &Client{
		socketPath: socketPath,
		// No global timeout — long-running streams (logs, events) need to stay
		// open. Per-call contexts handle cancellation; ResponseHeaderTimeout
		// guards against a daemon that accepts but never replies.
		http:     &http.Client{Transport: transport(defaultResponseHeaderTimeout)},
		httpSlow: &http.Client{Transport: transport(0)},
	}
}

// SocketPath returns the unix socket the client targets. Useful for
// error messages and tests.
func (c *Client) SocketPath() string { return c.socketPath }

// Ping verifies the daemon is up and responsive. Returns nil on
// success; any other return value is a signal the daemon is not
// available.
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/_ping", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("runtime ping: status %d", resp.StatusCode)
	}
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// WaitForReady polls Ping until it succeeds or the context expires.
// Used at tierd startup to handle the race where smoothnas-runtime
// has not finished initialising. The proposal allows up to 30 s.
func (c *Client) WaitForReady(ctx context.Context) error {
	const interval = 200 * time.Millisecond
	t := time.NewTicker(interval)
	defer t.Stop()
	// Try once immediately so we don't pay the first interval when
	// the daemon is already up.
	if err := c.Ping(ctx); err == nil {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("runtime: not ready after %v: %w", interval, ctx.Err())
		case <-t.C:
			if err := c.Ping(ctx); err == nil {
				return nil
			}
		}
	}
}

// Info returns a parsed /info response. Used at tierd startup so the
// operator can see the runtime daemon version + storage driver in
// SmoothNAS logs.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	if err := c.getJSON(ctx, "/info", nil, &out); err != nil {
		return Info{}, err
	}
	return out, nil
}

// Info is the subset of Docker's /info response tierd cares about.
// Adding fields here is cheap; new fields just default to zero when
// the daemon doesn't supply them.
type Info struct {
	ServerVersion   string `json:"ServerVersion"`
	OperatingSystem string `json:"OperatingSystem"`
	KernelVersion   string `json:"KernelVersion"`
	NCPU            int    `json:"NCPU"`
	MemTotal        int64  `json:"MemTotal"`
	Driver          string `json:"Driver"`
	Containers      int    `json:"Containers"`
	Images          int    `json:"Images"`
	Name            string `json:"Name"`
}

// APIError is the typed error returned for non-2xx responses with a
// JSON body. The Docker API convention is `{"message":"..."}`.
type APIError struct {
	StatusCode int
	Message    string
	Method     string
	Path       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("runtime %s %s: %d %s", e.Method, e.Path, e.StatusCode, e.Message)
}

// IsNotFound reports whether err is a 404 from the runtime daemon.
// Lifecycle code uses this to handle "container/image already gone"
// idempotently.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound
}

// IsConflict reports whether err is a 409 (typically "container is
// already started" or similar). Lifecycle code uses this to make
// start/stop idempotent.
func IsConflict(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.StatusCode == http.StatusConflict
}

// --- internal helpers ---

// do performs a request against the daemon. The caller is responsible
// for closing resp.Body. Non-2xx responses produce an *APIError.
//
// query, when non-nil, is appended as a URL query string. body, when
// non-nil, is JSON-encoded and sent with Content-Type: application/json.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	return c.doVia(ctx, c.http, method, path, query, body)
}

// doVia is do() with an explicit http client, so slow operations (container
// create) can use httpSlow (no ResponseHeaderTimeout).
func (c *Client) doVia(ctx context.Context, cl *http.Client, method, path string, query url.Values, body any) (*http.Response, error) {
	full := "http://unix" + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}

	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode body: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, full, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runtime %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 400 {
		// Read the body so we can include the daemon's error message.
		defer resp.Body.Close()
		msg := readErrorBody(resp.Body)
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Message:    msg,
			Method:     method,
			Path:       path,
		}
	}
	return resp, nil
}

// getJSON does a GET, JSON-decodes the response, and closes the body.
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	resp, err := c.do(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// postJSON does a POST with a JSON body, JSON-decodes the response,
// and closes the body. out may be nil if the caller doesn't care
// about the response body.
func (c *Client) postJSON(ctx context.Context, path string, query url.Values, body, out any) error {
	return c.postJSONVia(ctx, c.http, path, query, body, out)
}

// postJSONVia is postJSON with an explicit http client (see doVia).
func (c *Client) postJSONVia(ctx context.Context, cl *http.Client, path string, query url.Values, body, out any) error {
	resp, err := c.doVia(ctx, cl, http.MethodPost, path, query, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// readErrorBody pulls a Docker-style error message out of an HTTP
// error response. Daemon convention: `{"message":"..."}`. Falls back
// to the raw body or "(empty body)" on parse failures.
func readErrorBody(r io.Reader) string {
	const max = 4096
	buf, _ := io.ReadAll(io.LimitReader(r, max))
	if len(buf) == 0 {
		return "(empty body)"
	}
	var wrap struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(buf, &wrap); err == nil && wrap.Message != "" {
		return wrap.Message
	}
	return strings.TrimSpace(string(buf))
}
