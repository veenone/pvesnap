package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/veenone/pvesnap/internal/config"
	"github.com/veenone/pvesnap/internal/orchestrator"
	"github.com/veenone/pvesnap/internal/proxmox"
	"github.com/veenone/pvesnap/internal/state"
)

func RunSnapshot(ctx context.Context, cfg *config.Config, statePath string, out io.Writer, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(out, "usage: pvesnap snapshot <create|list|restore|delete> [args]")
		return 3
	}
	sub := args[0]
	rest := args[1:]
	st, err := state.Load(statePath)
	if err != nil {
		fmt.Fprintln(out, err)
		return 3
	}
	switch sub {
	case "create":
		return runSnapshotCreate(ctx, cfg, st, statePath, out, rest)
	case "list":
		return runSnapshotList(st, out, rest)
	case "restore":
		return runSnapshotRestore(ctx, cfg, st, statePath, out, rest)
	case "delete":
		return runSnapshotDelete(ctx, cfg, st, statePath, out, rest)
	default:
		fmt.Fprintf(out, "unknown snapshot subcommand: %s\n", sub)
		return 3
	}
}

func runSnapshotCreate(ctx context.Context, cfg *config.Config, st *state.Store, statePath string, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("snapshot create", flag.ContinueOnError)
	desc := fs.String("description", "", "snapshot description recorded on every guest")
	includeRAM := fs.Bool("include-ram", false, "include VM RAM state (vmstate=1); VMs only, larger snapshots")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	pos := fs.Args()
	if len(pos) != 2 {
		fmt.Fprintln(out, "usage: pvesnap snapshot create <set> <name>")
		return 3
	}
	setName := pos[0]
	rawName := pos[1]
	set, ok := cfg.FindSet(setName)
	if !ok {
		fmt.Fprintf(out, "unknown set: %s\n", setName)
		return 3
	}
	snapname, err := config.NormalizeSnapName(rawName)
	if err != nil {
		fmt.Fprintln(out, err)
		return 3
	}
	if snapname != rawName {
		fmt.Fprintf(out, "normalized snapshot name: %s → %s\n", rawName, snapname)
	}

	client := proxmox.NewClient(cfg)
	orch := orchestrator.New(client, cfg)
	opCtx, cancel := orch.OpContext(ctx)
	defer cancel()

	fmt.Fprintf(out, "creating snapshot %q on set %q (%d guests)...\n", snapname, set.Name, len(set.Guests))
	results := orch.Create(opCtx, set, snapname, *desc, *includeRAM)

	snap := state.Snapshot{
		Set:         set.Name,
		Name:        snapname,
		Description: *desc,
		CreatedAt:   time.Now().UTC(),
		Guests:      make([]state.GuestRecord, len(results)),
	}
	okCount, failCount := 0, 0
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tTYPE\tVMID\tSTATUS\tDETAIL")
	for i, r := range results {
		rec := state.GuestRecord{
			Node: r.Guest.Node, VMID: r.Guest.VMID, Type: r.Guest.Type, Snapname: snapname,
		}
		if r.Success {
			rec.Status = state.StatusOK
			okCount++
			fmt.Fprintf(tw, "%s\t%s\t%d\tok\t\n", r.Guest.Node, r.Guest.Type, r.Guest.VMID)
		} else {
			rec.Status = state.StatusFailed
			rec.Error = errString(r.Err)
			failCount++
			fmt.Fprintf(tw, "%s\t%s\t%d\tfailed\t%s\n", r.Guest.Node, r.Guest.Type, r.Guest.VMID, rec.Error)
		}
		snap.Guests[i] = rec
	}
	_ = tw.Flush()

	st.Upsert(snap)
	if err := st.Save(statePath); err != nil {
		fmt.Fprintf(out, "warning: failed to persist state: %v\n", err)
	}

	fmt.Fprintf(out, "done: %d ok, %d failed\n", okCount, failCount)
	switch {
	case failCount == 0:
		return 0
	case okCount == 0:
		return 2
	default:
		return 1
	}
}

func runSnapshotList(st *state.Store, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("snapshot list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 3
	}
	pos := fs.Args()
	filter := ""
	if len(pos) == 1 {
		filter = pos[0]
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SET\tNAME\tCREATED\tGUESTS\tFAILED\tDESCRIPTION")
	for _, s := range st.Snapshots {
		if filter != "" && s.Set != filter {
			continue
		}
		failed := 0
		for _, g := range s.Guests {
			if g.Status != state.StatusOK {
				failed++
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n",
			s.Set, s.Name, s.CreatedAt.Local().Format("2006-01-02 15:04"),
			len(s.Guests), failed, s.Description)
	}
	_ = tw.Flush()
	return 0
}

func runSnapshotRestore(ctx context.Context, cfg *config.Config, st *state.Store, statePath string, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("snapshot restore", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip confirmation")
	vmidFlag := fs.String("vmid", "", "comma-separated VMIDs to restore (default: all guests in set)")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	pos := fs.Args()
	if len(pos) != 2 {
		fmt.Fprintln(out, "usage: pvesnap snapshot restore <set> <name> [-vmid 100,101]")
		return 3
	}
	setName, name := pos[0], pos[1]
	snap, _ := st.Find(setName, name)
	if snap == nil {
		fmt.Fprintf(out, "no recorded snapshot %s/%s — check 'pvesnap snapshot list'\n", setName, name)
		return 3
	}
	vmidFilter, err := parseVMIDFilter(*vmidFlag)
	if err != nil {
		fmt.Fprintln(out, err)
		return 3
	}

	// Only attempt rollback on guests that have status=ok in state.
	var targets []state.GuestRecord
	for _, g := range snap.Guests {
		if g.Status == state.StatusOK {
			targets = append(targets, g)
		}
	}
	targets = filterByVMID(targets, vmidFilter)
	if len(targets) == 0 {
		fmt.Fprintln(out, "no healthy guests to roll back in this snapshot")
		return 2
	}

	if !*yes {
		fmt.Fprintf(out, "About to ROLLBACK %d guests in set %q to snapshot %q.\nThis is destructive. Continue? [y/N] ", len(targets), setName, name)
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		if strings.ToLower(strings.TrimSpace(line)) != "y" {
			fmt.Fprintln(out, "aborted")
			return 0
		}
	}
	client := proxmox.NewClient(cfg)
	orch := orchestrator.New(client, cfg)
	opCtx, cancel := orch.OpContext(ctx)
	defer cancel()

	fmt.Fprintf(out, "rolling back %d guests to %q...\n", len(targets), name)
	results := orch.Restore(opCtx, targets)
	okCount, failCount := 0, 0
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tTYPE\tVMID\tSTATUS\tDETAIL")
	for _, r := range results {
		if r.Success {
			okCount++
			fmt.Fprintf(tw, "%s\t%s\t%d\tok\t\n", r.Guest.Node, r.Guest.Type, r.Guest.VMID)
		} else {
			failCount++
			fmt.Fprintf(tw, "%s\t%s\t%d\tfailed\t%s\n", r.Guest.Node, r.Guest.Type, r.Guest.VMID, errString(r.Err))
		}
	}
	_ = tw.Flush()
	fmt.Fprintf(out, "done: %d ok, %d failed\n", okCount, failCount)
	_ = statePath // not modified on restore
	switch {
	case failCount == 0:
		return 0
	case okCount == 0:
		return 2
	default:
		return 1
	}
}

func runSnapshotDelete(ctx context.Context, cfg *config.Config, st *state.Store, statePath string, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("snapshot delete", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip confirmation")
	vmidFlag := fs.String("vmid", "", "comma-separated VMIDs to delete (default: all guests in set)")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	pos := fs.Args()
	if len(pos) != 2 {
		fmt.Fprintln(out, "usage: pvesnap snapshot delete <set> <name> [-vmid 100,101]")
		return 3
	}
	setName, name := pos[0], pos[1]
	snap, _ := st.Find(setName, name)
	if snap == nil {
		fmt.Fprintf(out, "no recorded snapshot %s/%s\n", setName, name)
		return 3
	}
	vmidFilter, err := parseVMIDFilter(*vmidFlag)
	if err != nil {
		fmt.Fprintln(out, err)
		return 3
	}

	// Delete only where the snapshot was recorded as present.
	var targets []state.GuestRecord
	for _, g := range snap.Guests {
		if g.Status == state.StatusOK {
			targets = append(targets, g)
		}
	}
	targets = filterByVMID(targets, vmidFilter)

	if !*yes {
		fmt.Fprintf(out, "Delete snapshot %q from %d guests in set %q? [y/N] ", name, len(targets), setName)
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		if strings.ToLower(strings.TrimSpace(line)) != "y" {
			fmt.Fprintln(out, "aborted")
			return 0
		}
	}
	client := proxmox.NewClient(cfg)
	orch := orchestrator.New(client, cfg)
	opCtx, cancel := orch.OpContext(ctx)
	defer cancel()

	results := orch.Delete(opCtx, targets)

	okCount, failCount := 0, 0
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tTYPE\tVMID\tSTATUS\tDETAIL")
	for _, r := range results {
		if r.Success {
			okCount++
			fmt.Fprintf(tw, "%s\t%s\t%d\tok\t\n", r.Guest.Node, r.Guest.Type, r.Guest.VMID)
		} else {
			failCount++
			fmt.Fprintf(tw, "%s\t%s\t%d\tfailed\t%s\n", r.Guest.Node, r.Guest.Type, r.Guest.VMID, errString(r.Err))
		}
	}
	_ = tw.Flush()

	if failCount == 0 && vmidFilter == nil {
		st.Remove(setName, name)
	}
	if err := st.Save(statePath); err != nil {
		fmt.Fprintf(out, "warning: failed to persist state: %v\n", err)
	}

	fmt.Fprintf(out, "done: %d ok, %d failed\n", okCount, failCount)
	switch {
	case failCount == 0:
		return 0
	case okCount == 0:
		return 2
	default:
		return 1
	}
}

// parseVMIDFilter parses a comma-separated list of VMIDs into a lookup map.
// Returns nil map if the input is empty (no filtering).
func parseVMIDFilter(raw string) (map[int]bool, error) {
	if raw == "" {
		return nil, nil
	}
	m := make(map[int]bool)
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("invalid VMID %q: %w", s, err)
		}
		m[id] = true
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

// filterByVMID returns only guests whose VMID is in the filter map.
// If filter is nil, all guests are returned unchanged.
func filterByVMID(guests []state.GuestRecord, filter map[int]bool) []state.GuestRecord {
	if filter == nil {
		return guests
	}
	var out []state.GuestRecord
	for _, g := range guests {
		if filter[g.VMID] {
			out = append(out, g)
		}
	}
	return out
}

func errString(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}
