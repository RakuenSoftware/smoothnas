package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Event is one entry from the daemon's /events stream. The Docker
// API delivers many event kinds; tierd only inspects container
// events filtered by our managed label.
type Event struct {
	Type   string            `json:"Type"`   // "container" | "image" | "network" | ...
	Action string            `json:"Action"` // "start" | "die" | "oom" | "destroy" | ...
	Actor  EventActor        `json:"Actor"`
	Time   int64             `json:"time"`
	TimeNS int64             `json:"timeNano"`
}

// EventActor is the subject of an event.
type EventActor struct {
	ID         string            `json:"ID"`
	Attributes map[string]string `json:"Attributes"` // includes our io.smoothnas.* labels
}

// SubscribeEvents opens a streaming GET /events filtered to containers
// labelled io.smoothnas.managed=true. Events are delivered on the
// returned channel; closing the channel signals the stream ended
// (either context cancellation or the daemon hung up).
//
// The caller is responsible for re-subscribing if the stream ends
// unexpectedly. The phase 02 lifecycle wraps this in a supervisor
// goroutine that retries with backoff.
func (c *Client) SubscribeEvents(ctx context.Context) (<-chan Event, <-chan error, error) {
	q := url.Values{}
	filters := map[string][]string{
		"label": {PluginManagedLabel + "=true"},
		"type":  {"container"},
	}
	enc, err := json.Marshal(filters)
	if err != nil {
		return nil, nil, fmt.Errorf("encode filters: %w", err)
	}
	q.Set("filters", string(enc))

	resp, err := c.do(ctx, http.MethodGet, "/events", q, nil)
	if err != nil {
		return nil, nil, err
	}

	events := make(chan Event, 16)
	errs := make(chan error, 1)

	go func() {
		defer close(events)
		defer close(errs)
		defer resp.Body.Close()

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 32*1024), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				// Skip malformed lines rather than killing the stream;
				// LXC2Docker is allowed to evolve.
				continue
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			}
		}
		if err := sc.Err(); err != nil {
			errs <- err
		}
	}()

	return events, errs, nil
}

// EventStreamReconnectInterval is the recommended backoff for the
// supervisor that owns the event subscription. Surfaced here as a
// constant so the lifecycle code and tests share one source of truth.
const EventStreamReconnectInterval = 2 * time.Second
