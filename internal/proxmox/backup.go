package proxmox

import (
	"context"
	"fmt"
	"net/http"
)

// BackupVerification is the PBS verification sub-object of a content entry.
type BackupVerification struct {
	State string `json:"state"`
}

// BackupPoint is one PBS backup volume for a guest, from the storage content API.
type BackupPoint struct {
	VolID        string             `json:"volid"`
	Format       string             `json:"format"`
	Notes        string             `json:"notes,omitempty"`
	CTime        int64              `json:"ctime"`
	Size         int64              `json:"size"`
	Protected    int                `json:"protected,omitempty"`
	Verification BackupVerification `json:"verification"`
	VMID         int                `json:"vmid,omitempty"`
}

// ListBackups returns the backup volumes for a guest on the given storage, via
// the node storage content API (content=backup).
func (c *Client) ListBackups(ctx context.Context, node, storage string, vmid int) ([]BackupPoint, error) {
	var out []BackupPoint
	path := fmt.Sprintf("/api2/json/nodes/%s/storage/%s/content?content=backup&vmid=%d", node, storage, vmid)
	if err := c.do(ctx, node, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
