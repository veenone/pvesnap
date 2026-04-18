package proxmox

import (
	"context"
	"net/http"
)

type ClusterResource struct {
	Type     string `json:"type"`
	Node     string `json:"node"`
	VMID     int    `json:"vmid,omitempty"`
	Name     string `json:"name,omitempty"`
	Status   string `json:"status,omitempty"`
	Uptime   int64  `json:"uptime,omitempty"`
	MaxMem   int64  `json:"maxmem,omitempty"`
	MaxDisk  int64  `json:"maxdisk,omitempty"`
	Template int    `json:"template,omitempty"`
}

// ClusterResources returns all VMs and LXCs known to the Proxmox node's
// cluster view. For standalone nodes this still returns that node's guests.
func (c *Client) ClusterResources(ctx context.Context, node string) ([]ClusterResource, error) {
	var out []ClusterResource
	if err := c.do(ctx, node, http.MethodGet, "/api2/json/cluster/resources?type=vm", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
