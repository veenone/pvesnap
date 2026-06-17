package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
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

// RunBackup dispatches the `backup` subcommands.
func RunBackup(ctx context.Context, cfg *config.Config, out io.Writer, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(out, "usage: pvesnap backup <list|restore> [args]")
		return 3
	}
	switch args[0] {
	case "list":
		return runBackupList(ctx, cfg, out, args[1:])
	case "restore":
		return runBackupRestore(ctx, cfg, out, args[1:])
	default:
		fmt.Fprintf(out, "unknown backup subcommand: %s\n", args[0])
		return 3
	}
}

func runBackupList(ctx context.Context, cfg *config.Config, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("backup list", flag.ContinueOnError)
	vmidFlag := fs.String("vmid", "", "comma-separated VMIDs to list (default: all guests in set)")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	pos := fs.Args()
	if len(pos) != 1 {
		fmt.Fprintln(out, "usage: pvesnap backup list <set> [-vmid 100,101]")
		return 3
	}
	set, ok := cfg.FindSet(pos[0])
	if !ok {
		fmt.Fprintf(out, "unknown set: %s\n", pos[0])
		return 3
	}
	storage := cfg.ResolvePBSStorage(set)
	if storage == "" {
		fmt.Fprintf(out, "no PBS storage configured for set %q (set defaults.pbs_storage)\n", set.Name)
		return 3
	}
	vmidFilter, err := parseVMIDFilter(*vmidFlag)
	if err != nil {
		fmt.Fprintln(out, err)
		return 3
	}

	orch := orchestrator.New(proxmox.NewClient(cfg), cfg)
	opCtx, cancel := orch.OpContext(ctx)
	defer cancel()

	results := orch.ListBackups(opCtx, storage, set.Guests)
	exit := 0
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tTYPE\tVMID\tWHEN\tSIZE\tVERIFIED\tPROT\tVOLID")
	for _, res := range results {
		if vmidFilter != nil && !vmidFilter[res.Guest.VMID] {
			continue
		}
		if res.Err != nil {
			fmt.Fprintf(out, "query %s/%d: %v\n", res.Guest.Node, res.Guest.VMID, res.Err)
			exit = 1
			continue
		}
		bs := res.Backups
		sort.Slice(bs, func(i, j int) bool { return bs[i].CTime > bs[j].CTime })
		for _, b := range bs {
			when := time.Unix(b.CTime, 0).Local().Format("2006-01-02 15:04")
			verified := b.Verification.State
			if verified == "" {
				verified = "-"
			}
			prot := "no"
			if b.Protected != 0 {
				prot = "yes"
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
				res.Guest.Node, res.Guest.Type, res.Guest.VMID, when, humanizeBytes(b.Size), verified, prot, b.VolID)
		}
	}
	_ = tw.Flush()
	return exit
}

// runBackupRestore is a temporary stub; it will be fully implemented in Task 9.
func runBackupRestore(ctx context.Context, cfg *config.Config, out io.Writer, args []string) int {
	fmt.Fprintln(out, "not implemented")
	return 3
}
