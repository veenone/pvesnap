package cli

import (
	"fmt"
	"time"

	"github.com/veenone/pvesnap/internal/config"
	"github.com/veenone/pvesnap/internal/orchestrator"
	"github.com/veenone/pvesnap/internal/proxmox"
)

// parseAtTime parses an --at value as RFC3339 or a plain YYYY-MM-DD date (local).
func parseAtTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid --at time %q (use RFC3339 or YYYY-MM-DD)", s)
}

// pickBackup chooses one backup for a guest: latest = newest by ctime;
// otherwise the newest with ctime <= atUnix. ok=false if none qualifies.
func pickBackup(backups []proxmox.BackupPoint, latest bool, atUnix int64) (proxmox.BackupPoint, bool) {
	var best proxmox.BackupPoint
	found := false
	for _, b := range backups {
		if !latest && b.CTime > atUnix {
			continue
		}
		if !found || b.CTime > best.CTime {
			best = b
			found = true
		}
	}
	return best, found
}

// selectBackupTargets resolves one backup per guest under the selection mode.
// Guests excluded by filter are ignored; errored queries are skipped here (the
// caller reports them); guests with no matching backup are returned in skipped.
func selectBackupTargets(results []orchestrator.BackupListResult, latest bool, atUnix int64, filter map[int]bool) (targets []orchestrator.BackupTarget, skipped []config.Guest) {
	for _, res := range results {
		if filter != nil && !filter[res.Guest.VMID] {
			continue
		}
		if res.Err != nil {
			continue
		}
		b, ok := pickBackup(res.Backups, latest, atUnix)
		if !ok {
			skipped = append(skipped, res.Guest)
			continue
		}
		targets = append(targets, orchestrator.BackupTarget{Guest: res.Guest, VolID: b.VolID})
	}
	return targets, skipped
}

// humanizeBytes formats a byte count as a human-readable IEC string.
func humanizeBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
