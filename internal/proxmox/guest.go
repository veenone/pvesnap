package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/veenone/pvesnap/internal/config"
)

type guestStatusResp struct {
	Status string `json:"status"`
}

// GuestStatus returns the run state ("running"/"stopped") of a guest.
func (c *Client) GuestStatus(ctx context.Context, node string, t config.GuestType, vmid int) (string, error) {
	var out guestStatusResp
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/status/current", node, t, vmid)
	if err := c.do(ctx, node, http.MethodGet, path, nil, &out); err != nil {
		return "", err
	}
	return out.Status, nil
}

// StopGuest stops a guest; returns the UPID of the async task.
func (c *Client) StopGuest(ctx context.Context, node string, t config.GuestType, vmid int) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/status/stop", node, t, vmid)
	return c.doString(ctx, node, http.MethodPost, path, url.Values{})
}

// StartGuest starts a guest; returns the UPID of the async task.
func (c *Client) StartGuest(ctx context.Context, node string, t config.GuestType, vmid int) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/status/start", node, t, vmid)
	return c.doString(ctx, node, http.MethodPost, path, url.Values{})
}
