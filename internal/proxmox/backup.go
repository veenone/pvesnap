package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/veenone/pvesnap/internal/config"
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

// RestoreBackup restores a guest in-place from a backup volume, overwriting the
// existing VMID (force=1). QEMU uses archive=, LXC uses ostemplate=+restore=1.
// Returns the UPID of the async task.
func (c *Client) RestoreBackup(ctx context.Context, node string, t config.GuestType, vmid int, volid string) (string, error) {
	form := url.Values{}
	form.Set("vmid", strconv.Itoa(vmid))
	form.Set("force", "1")
	var path string
	switch t {
	case config.QEMU:
		form.Set("archive", volid)
		path = fmt.Sprintf("/api2/json/nodes/%s/qemu", node)
	case config.LXC:
		form.Set("ostemplate", volid)
		form.Set("restore", "1")
		path = fmt.Sprintf("/api2/json/nodes/%s/lxc", node)
	default:
		return "", fmt.Errorf("restore: unknown guest type %q", t)
	}
	return c.doString(ctx, node, http.MethodPost, path, form)
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
