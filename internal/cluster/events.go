package cluster

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Event is one decoded SSE frame from a follower's /events stream: the named
// event and its JSON-encoded payload (matching internal/sse.Broker.ServeAll).
type Event struct {
	Name string
	Data string
}

// Subscribe opens the follower's multiplexed SSE stream (/events) and returns a
// channel of decoded events. It fails over across the node's endpoints in
// preference order to find a reachable one, then streams until ctx is cancelled
// or the connection drops, at which point the channel is closed. Reconnection is
// the caller's responsibility (the Roster health loop drives it in P3).
func (c *Client) Subscribe(ctx context.Context) (<-chan Event, error) {
	eps := c.snapshotEndpoints()
	if len(eps) == 0 {
		return nil, fmt.Errorf("cluster: node %q has no usable endpoints", c.node.Name)
	}
	start := c.activeIndex(len(eps))
	var errs []error
	for i := range eps {
		idx := (start + i) % len(eps)
		body, err := c.openEvents(ctx, eps[idx])
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", eps[idx], err))
			continue
		}
		c.setActive(idx)
		ch := make(chan Event, eventChanBuf)
		go c.pumpEvents(body, ch)
		return ch, nil
	}
	return nil, fmt.Errorf("cluster: no reachable /events endpoint for node %q: %w", c.node.Name, errors.Join(errs...))
}

const eventChanBuf = 64

func (c *Client) openEvents(ctx context.Context, base string) (io.ReadCloser, error) {
	u := strings.TrimRight(base, "/") + "/events"
	if c.node.Token != "" {
		u += "?token=" + url.QueryEscape(c.node.Token)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.node.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.node.Token)
	}
	resp, err := c.sseHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("cluster: /events returned %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func (c *Client) pumpEvents(body io.ReadCloser, ch chan<- Event) {
	defer close(ch)
	defer func() { _ = body.Close() }()

	reader := bufio.NewReader(body)
	var name string
	var data strings.Builder
	flush := func() {
		if data.Len() == 0 && name == "" {
			return
		}
		evName := name
		if evName == "" {
			evName = "message"
		}
		ch <- Event{Name: evName, Data: data.String()}
		name = ""
		data.Reset()
	}
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				flush()
			case strings.HasPrefix(trimmed, ":"):
			case strings.HasPrefix(trimmed, "event:"):
				name = strings.TrimSpace(trimmed[len("event:"):])
			case strings.HasPrefix(trimmed, "data:"):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimSpace(trimmed[len("data:"):]))
			}
		}
		if err != nil {
			return
		}
	}
}
