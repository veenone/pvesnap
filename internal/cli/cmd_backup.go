package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
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
	pos, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return 3
	}
	if len(pos) != 1 {
		fmt.Fprintln(out, "usage: pvesnap backup list <set> [-vmid 100,101]")
		return 3
	}
	setName := pos[0]
	set, ok := cfg.FindSet(setName)
	if !ok {
		fmt.Fprintf(out, "unknown set: %s\n", setName)
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

func runBackupRestore(ctx context.Context, cfg *config.Config, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("backup restore", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip confirmation")
	noStart := fs.Bool("no-start", false, "leave guests stopped after restore (default: restart if it was running)")
	vmidFlag := fs.String("vmid", "", "comma-separated VMIDs to target")
	volid := fs.String("volid", "", "exact backup volid (requires a single -vmid)")
	latest := fs.Bool("latest", false, "restore each guest from its newest backup")
	atStr := fs.String("at", "", "restore each guest from its newest backup at or before this time (RFC3339 or YYYY-MM-DD)")
	pos, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return 3
	}
	if len(pos) != 1 {
		fmt.Fprintln(out, "usage: pvesnap backup restore <set> (-vmid N -volid V | --latest | --at T) [-vmid ...] [--no-start] [--yes]")
		return 3
	}
	setName := pos[0]
	set, ok := cfg.FindSet(setName)
	if !ok {
		fmt.Fprintf(out, "unknown set: %s\n", setName)
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

	sel := 0
	if *volid != "" {
		sel++
	}
	if *latest {
		sel++
	}
	if *atStr != "" {
		sel++
	}
	if sel != 1 {
		fmt.Fprintln(out, "specify exactly one of -volid, --latest, or --at")
		return 3
	}

	orch := orchestrator.New(proxmox.NewClient(cfg), cfg)
	opCtx, cancel := orch.OpContext(ctx)
	defer cancel()

	var targets []orchestrator.BackupTarget
	if *volid != "" {
		if vmidFilter == nil || len(vmidFilter) != 1 {
			fmt.Fprintln(out, "-volid requires exactly one -vmid")
			return 3
		}
		var vid int
		for k := range vmidFilter {
			vid = k
		}
		guest, found := config.Guest{}, false
		for _, g := range set.Guests {
			if g.VMID == vid {
				guest, found = g, true
				break
			}
		}
		if !found {
			fmt.Fprintf(out, "vmid %d not in set %q\n", vid, set.Name)
			return 3
		}
		targets = []orchestrator.BackupTarget{{Guest: guest, VolID: *volid}}
	} else {
		var atUnix int64
		if *atStr != "" {
			at, err := parseAtTime(*atStr)
			if err != nil {
				fmt.Fprintln(out, err)
				return 3
			}
			atUnix = at.Unix()
		}
		results := orch.ListBackups(opCtx, storage, set.Guests)
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(out, "warning: could not list backups for %s/%d: %v\n", r.Guest.Node, r.Guest.VMID, r.Err)
			}
		}
		var skipped []config.Guest
		targets, skipped = selectBackupTargets(results, *latest, atUnix, vmidFilter)
		for _, g := range skipped {
			fmt.Fprintf(out, "note: no matching backup for %s/%d, skipping\n", g.Node, g.VMID)
		}
	}

	if len(targets) == 0 {
		fmt.Fprintf(out, "no backup points selected for set %q\n", set.Name)
		return 2
	}

	// Show exactly what will be overwritten before the (destructive) confirm.
	fmt.Fprintln(out, "will restore (in-place, overwriting disks):")
	for _, tgt := range targets {
		fmt.Fprintf(out, "  %s %s %d  <- %s\n", tgt.Guest.Node, tgt.Guest.Type, tgt.Guest.VMID, tgt.VolID)
	}

	if !*yes {
		fmt.Fprintf(out, "About to RESTORE %d guests in set %q IN-PLACE from PBS backups.\nThis STOPS each guest and OVERWRITES its disks. Continue? [y/N] ", len(targets), set.Name)
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		if strings.ToLower(strings.TrimSpace(line)) != "y" {
			fmt.Fprintln(out, "aborted")
			return 0
		}
	}

	fmt.Fprintf(out, "restoring %d guests from PBS...\n", len(targets))
	results := orch.RestoreBackup(opCtx, targets, *noStart)
	okCount, failCount, cancelled := renderResults(out, results)
	if cancelled > 0 {
		fmt.Fprintf(out, "done: %d ok, %d failed, %d cancelled\n", okCount, failCount, cancelled)
	} else {
		fmt.Fprintf(out, "done: %d ok, %d failed\n", okCount, failCount)
	}
	return exitForCounts(okCount, failCount, cancelled)
}
