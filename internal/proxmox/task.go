package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

type TaskStatus struct {
	UPID       string `json:"upid"`
	Node       string `json:"node"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus,omitempty"`
}

// TaskStatusOnce fetches the task status once without polling.
func (c *Client) TaskStatusOnce(ctx context.Context, node, upid string) (TaskStatus, error) {
	var out TaskStatus
	path := fmt.Sprintf("/api2/json/nodes/%s/tasks/%s/status", node, upid)
	if err := c.do(ctx, node, http.MethodGet, path, nil, &out); err != nil {
		return out, err
	}
	return out, nil
}

// WaitTask polls the task until it reaches status=stopped or ctx is cancelled.
// On completion, exitstatus == "OK" indicates success; anything else is an error
// with the exitstatus string as the message.
func (c *Client) WaitTask(ctx context.Context, node, upid string, interval time.Duration) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		st, err := c.TaskStatusOnce(ctx, node, upid)
		if err != nil {
			return err
		}
		if st.Status == "stopped" {
			if st.ExitStatus == "OK" {
				return nil
			}
			return fmt.Errorf("task %s failed: %s", upid, st.ExitStatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
