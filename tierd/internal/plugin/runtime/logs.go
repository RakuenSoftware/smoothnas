package runtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// LogsOptions controls a /containers/{id}/logs request.
type LogsOptions struct {
	Follow     bool
	Stdout     bool
	Stderr     bool
	Timestamps bool
	Since      int64  // unix seconds; 0 = no lower bound
	Tail       string // "all" | "100" | "" (default: "all")
}

// StreamLogs opens GET /containers/{id}/logs and returns the response
// body. The caller is responsible for closing the returned io.ReadCloser.
//
// LXC2Docker emits the same multiplexed stream Docker proper does
// (8-byte headers prefixing each chunk identifying stdout vs stderr).
// tierd's UI layer is expected to do the demultiplexing if it needs
// per-stream tagging; raw passthrough is fine for SSE log views.
func (c *Client) StreamLogs(ctx context.Context, id string, opts LogsOptions) (io.ReadCloser, error) {
	q := url.Values{}
	if opts.Follow {
		q.Set("follow", "1")
	}
	if opts.Stdout {
		q.Set("stdout", "1")
	}
	if opts.Stderr {
		q.Set("stderr", "1")
	}
	if opts.Timestamps {
		q.Set("timestamps", "1")
	}
	if opts.Since > 0 {
		q.Set("since", strconv.FormatInt(opts.Since, 10))
	}
	tail := opts.Tail
	if tail == "" {
		tail = "all"
	}
	q.Set("tail", tail)

	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/logs", q, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("logs status %d: %s", resp.StatusCode, string(body))
	}
	return resp.Body, nil
}
