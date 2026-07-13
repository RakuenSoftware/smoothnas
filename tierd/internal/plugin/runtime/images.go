package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// PullImage issues POST /images/create against the runtime daemon.
// The daemon streams JSON status events line by line until the pull
// completes; this method drains the stream and returns the final
// resolved image ref (with @sha256:... when available).
//
// LXC2Docker accepts both registry refs (`ghcr.io/...:tag`) and the
// distro shorthand (`ubuntu:jammy`); the latter resolves to an LXC
// download template and is how lxc-distro plugins pull their base.
//
// onProgress, if non-nil, is called for every status event so the UI
// can show "Pulling layer X" progress. Pass nil if you don't care.
func (c *Client) PullImage(ctx context.Context, ref string, onProgress func(PullEvent)) (string, error) {
	q := url.Values{}
	// Docker splits ref into fromImage + tag in the query string.
	from, tag := splitRef(ref)
	q.Set("fromImage", from)
	if tag != "" {
		q.Set("tag", tag)
	}
	resp, err := c.do(ctx, http.MethodPost, "/images/create", q, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// The body is a stream of JSON objects, one per line, terminating
	// with either an error event or just EOF on success.
	sc := bufio.NewScanner(resp.Body)
	// Pulls can produce very long lines (e.g. progressDetail blobs).
	// Bump the buffer cap so we don't truncate.
	sc.Buffer(make([]byte, 64*1024), 1<<20)

	var resolved string
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev PullEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// LXC2Docker may emit non-JSON debug lines; surface them
			// to the progress callback as Status if anyone's listening
			// but don't fail the pull.
			if onProgress != nil {
				onProgress(PullEvent{Status: string(line)})
			}
			continue
		}
		if ev.Error != "" {
			return "", fmt.Errorf("pull %s: %s", ref, ev.Error)
		}
		// Docker emits a final "Status: Digest: sha256:..." event when
		// the image manifest is digested (registry pulls). That's the
		// resolved digest we want to record.
		if d := extractDigest(ev.Status); d != "" {
			resolved = from + "@" + d
		}
		if onProgress != nil {
			onProgress(ev)
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("pull %s: stream: %w", ref, err)
	}

	// LXC2Docker may not surface a digest event for distro-template
	// pulls (they're built on the fly, not pulled from a registry).
	// In that case return the original ref unchanged so callers can
	// still record something useful in plugins.image_ref.
	if resolved == "" {
		resolved = ref
	}
	return resolved, nil
}

// PullEvent is one line of POST /images/create's streamed response.
type PullEvent struct {
	Status         string         `json:"status,omitempty"`
	Error          string         `json:"error,omitempty"`
	ID             string         `json:"id,omitempty"`
	Progress       string         `json:"progress,omitempty"`
	ProgressDetail map[string]any `json:"progressDetail,omitempty"`
}

// ImageSummary is one entry from GET /images/json — the fields tierd's
// image garbage-collector needs. Containers is the daemon's own count of
// containers referencing the image (computed over every container it knows,
// managed or not), so Containers==0 is an authoritative "nothing uses this".
type ImageSummary struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
	Created     int64    `json:"Created"` // unix seconds
	Size        int64    `json:"Size"`
	Containers  int      `json:"Containers"`
}

// ListImages returns every image the runtime daemon knows about
// (GET /images/json). Used by the orphaned-image sweep to reclaim
// templates left behind by plugin uninstall and in-place image updates.
func (c *Client) ListImages(ctx context.Context) ([]ImageSummary, error) {
	var out []ImageSummary
	if err := c.getJSON(ctx, "/images/json", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RemoveImage issues DELETE /images/{name}. Returns nil on 404 so
// uninstall can be retried after partial failure.
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	q := url.Values{}
	q.Set("force", "1")
	resp, err := c.do(ctx, http.MethodDelete, "/images/"+url.PathEscape(ref), q, nil)
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	return nil
}

// splitRef separates a Docker ref into its fromImage and tag parts
// the way the Engine API expects them. Examples:
//   "ubuntu"                     → ("ubuntu", "latest")
//   "ubuntu:22.04"               → ("ubuntu", "22.04")
//   "ghcr.io/foo/bar:1.2.3"      → ("ghcr.io/foo/bar", "1.2.3")
//   "ghcr.io/foo/bar@sha256:abc" → ("ghcr.io/foo/bar@sha256:abc", "")
func splitRef(ref string) (from, tag string) {
	// Digest pins are kept whole; the daemon parses them itself.
	if strings.Contains(ref, "@") {
		return ref, ""
	}
	// A colon in the path portion (port number) confuses naive
	// strings.LastIndex if the tag is also present, but Docker
	// treats the *last* colon as the tag separator only when it
	// follows a slash (or there's no slash at all).
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		// Don't split a port number out of e.g. "registry:5000/foo".
		if !strings.Contains(ref[i:], "/") {
			return ref[:i], ref[i+1:]
		}
	}
	return ref, "latest"
}

// extractDigest pulls "sha256:..." out of an event status string like
// "Digest: sha256:abc..." or returns "" if not present.
func extractDigest(status string) string {
	const prefix = "Digest: "
	i := strings.Index(status, prefix)
	if i < 0 {
		return ""
	}
	rest := status[i+len(prefix):]
	// Stop at the first whitespace so we don't capture trailing
	// content (Docker sometimes appends "\nStatus: ...").
	if j := strings.IndexAny(rest, " \t\r\n"); j >= 0 {
		rest = rest[:j]
	}
	if !strings.HasPrefix(rest, "sha256:") {
		return ""
	}
	return rest
}
