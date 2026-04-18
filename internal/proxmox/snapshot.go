package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/veenone/pvesnap/internal/config"
)

func guestBasePath(node string, t config.GuestType, vmid int) string {
	return fmt.Sprintf("/api2/json/nodes/%s/%s/%d", node, t, vmid)
}

type SnapshotEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Snaptime    int64  `json:"snaptime,omitempty"`
	Parent      string `json:"parent,omitempty"`
}

// ListSnapshots returns snapshots for a given guest.
func (c *Client) ListSnapshots(ctx context.Context, node string, t config.GuestType, vmid int) ([]SnapshotEntry, error) {
	var out []SnapshotEntry
	path := guestBasePath(node, t, vmid) + "/snapshot"
	if err := c.do(ctx, node, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateSnapshot creates a snapshot and returns the UPID of the async task.
func (c *Client) CreateSnapshot(ctx context.Context, node string, t config.GuestType, vmid int, snapname, description string, vmstate bool) (string, error) {
	form := url.Values{}
	form.Set("snapname", snapname)
	if description != "" {
		form.Set("description", description)
	}
	if t == config.QEMU && vmstate {
		form.Set("vmstate", "1")
	}
	path := guestBasePath(node, t, vmid) + "/snapshot"
	return c.doString(ctx, node, http.MethodPost, path, form)
}

// Rollback returns the UPID of the async rollback task.
func (c *Client) Rollback(ctx context.Context, node string, t config.GuestType, vmid int, snapname string) (string, error) {
	path := fmt.Sprintf("%s/snapshot/%s/rollback", guestBasePath(node, t, vmid), snapname)
	return c.doString(ctx, node, http.MethodPost, path, url.Values{})
}

// DeleteSnapshot returns the UPID of the async delete task.
func (c *Client) DeleteSnapshot(ctx context.Context, node string, t config.GuestType, vmid int, snapname string) (string, error) {
	path := fmt.Sprintf("%s/snapshot/%s", guestBasePath(node, t, vmid), snapname)
	return c.doString(ctx, node, http.MethodDelete, path, nil)
}
