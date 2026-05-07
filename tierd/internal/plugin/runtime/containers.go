package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// CreateContainer issues POST /containers/create?name=<name>. The
// returned ID is the daemon's identifier; tierd records it in
// plugin_instances.container_id.
func (c *Client) CreateContainer(ctx context.Context, name string, req CreateContainerRequest) (CreateContainerResponse, error) {
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	var out CreateContainerResponse
	if err := c.postJSON(ctx, "/containers/create", q, req, &out); err != nil {
		return CreateContainerResponse{}, err
	}
	return out, nil
}

// StartContainer issues POST /containers/{id}/start. Idempotent:
// 304 (already started) is treated as success because lifecycle
// retries shouldn't fail.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil)
	if err != nil {
		// Daemons return 304 for "already running"; some surface 409
		// instead. Either is fine for our caller.
		if IsConflict(err) {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	return nil
}

// StopContainer issues POST /containers/{id}/stop?t=<seconds>. The
// daemon SIGTERMs the container, waits up to t seconds, then SIGKILLs.
// Idempotent: 304 (already stopped) is success.
func (c *Client) StopContainer(ctx context.Context, id string, timeoutSeconds int) error {
	q := url.Values{}
	q.Set("t", strconv.Itoa(timeoutSeconds))
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+id+"/stop", q, nil)
	if err != nil {
		if IsConflict(err) {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	return nil
}

// RestartContainer issues POST /containers/{id}/restart?t=<seconds>.
func (c *Client) RestartContainer(ctx context.Context, id string, timeoutSeconds int) error {
	q := url.Values{}
	q.Set("t", strconv.Itoa(timeoutSeconds))
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+id+"/restart", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// RemoveContainer issues DELETE /containers/{id}?v=1&force=<force>.
// v=1 removes anonymous volumes (we don't use any); force triggers
// SIGKILL if the container is still running.
//
// Returns nil if the container is already gone (404) so uninstall
// can be retried after a partial failure.
func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	q := url.Values{}
	q.Set("v", "1")
	if force {
		q.Set("force", "1")
	}
	resp, err := c.do(ctx, http.MethodDelete, "/containers/"+id, q, nil)
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	return nil
}

// InspectContainer returns full container state for the given ID.
// Surfaces IsNotFound so callers can detect "container missing on
// startup" during reconciliation.
func (c *Client) InspectContainer(ctx context.Context, id string) (ContainerInspect, error) {
	var out ContainerInspect
	if err := c.getJSON(ctx, "/containers/"+id+"/json", nil, &out); err != nil {
		return ContainerInspect{}, err
	}
	return out, nil
}

// WaitContainer blocks until the container exits, then returns its
// exit code. Used by the lxc-distro setup flow to know the apt /
// setup script finished. Honours the request context for cancellation.
func (c *Client) WaitContainer(ctx context.Context, id string) (int, error) {
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+id+"/wait", nil, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct {
		StatusCode int    `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decode wait response: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return out.StatusCode, fmt.Errorf("wait container: %s", out.Error.Message)
	}
	return out.StatusCode, nil
}

// CommitContainer issues POST /commit to capture a container's
// current rootfs as an image. Used by the lxc-distro setup flow to
// snapshot the post-setup state as a reusable template.
func (c *Client) CommitContainer(ctx context.Context, containerID, repo, tag string) (string, error) {
	q := url.Values{}
	q.Set("container", containerID)
	if repo != "" {
		q.Set("repo", repo)
	}
	if tag != "" {
		q.Set("tag", tag)
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := c.postJSON(ctx, "/commit", q, struct{}{}, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ListManagedContainers returns every container labelled
// io.smoothnas.managed=true, regardless of state. The reconciler
// uses this at startup to enumerate what the daemon thinks tierd
// owns.
func (c *Client) ListManagedContainers(ctx context.Context) ([]ContainerSummary, error) {
	q := url.Values{}
	q.Set("all", "1")
	// Docker's `filters` query param is a URL-encoded JSON object.
	filters := map[string][]string{
		"label": {PluginManagedLabel + "=true"},
	}
	enc, err := json.Marshal(filters)
	if err != nil {
		return nil, fmt.Errorf("encode filters: %w", err)
	}
	q.Set("filters", string(enc))

	var out []ContainerSummary
	if err := c.getJSON(ctx, "/containers/json", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}
