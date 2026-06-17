package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
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
		return runSnapshotList(ctx, cfg, st, out, rest)
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
	pos, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return 3
	}
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
	for i, r := range results {
		rec := state.GuestRecord{
			Node: r.Guest.Node, VMID: r.Guest.VMID, Type: r.Guest.Type, Snapname: snapname,
		}
		if r.Success {
			rec.Status = state.StatusOK
		} else {
			rec.Status = state.StatusFailed
			rec.Error = errString(r.Err)
		}
		snap.Guests[i] = rec
	}
	okCount, failCount, cancelled := renderResults(out, results)

	st.Upsert(snap)
	if err := st.Save(statePath); err != nil {
		fmt.Fprintf(out, "warning: failed to persist state: %v\n", err)
	}

	if cancelled > 0 {
		fmt.Fprintf(out, "done: %d ok, %d failed, %d cancelled\n", okCount, failCount, cancelled)
	} else {
		fmt.Fprintf(out, "done: %d ok, %d failed\n", okCount, failCount)
	}
	return exitForCounts(okCount, failCount, cancelled)
}

func runSnapshotList(ctx context.Context, cfg *config.Config, st *state.Store, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("snapshot list", flag.ContinueOnError)
	live := fs.Bool("live", false, "query each guest's storage for actual snapshots instead of reading state")
	pos, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return 3
	}
	if *live {
		if len(pos) != 1 {
			fmt.Fprintln(out, "usage: pvesnap snapshot list <set> --live")
			return 3
		}
		return runSnapshotListLive(ctx, cfg, out, pos[0])
	}
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

func runSnapshotListLive(ctx context.Context, cfg *config.Config, out io.Writer, setName string) int {
	set, ok := cfg.FindSet(setName)
	if !ok {
		fmt.Fprintf(out, "unknown set: %s\n", setName)
		return 3
	}
	client := proxmox.NewClient(cfg)
	orch := orchestrator.New(client, cfg)
	opCtx, cancel := orch.OpContext(ctx)
	defer cancel()

	inv := orch.DiscoverSnapshots(opCtx, set.Guests)
	exit := 0
	for _, item := range inv {
		if item.Err != nil {
			fmt.Fprintf(out, "query %s/%d: %v\n", item.Guest.Node, item.Guest.VMID, item.Err)
			exit = 1
		}
	}

	total := len(set.Guests)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tCOVERAGE\tGUESTS\tNEWEST\tPARENTED")
	for _, r := range aggregateLiveSnapshots(inv) {
		coverage := "partial"
		if r.Count == total {
			coverage = "full"
		}
		newest := ""
		if r.Newest > 0 {
			newest = time.Unix(r.Newest, 0).Local().Format("2006-01-02 15:04")
		}
		parented := "no"
		if r.Parented {
			parented = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%s\t%s\n", r.Name, coverage, r.Count, total, newest, parented)
	}
	_ = tw.Flush()
	return exit
}

func runSnapshotRestore(ctx context.Context, cfg *config.Config, st *state.Store, statePath string, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("snapshot restore", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip confirmation")
	vmidFlag := fs.String("vmid", "", "comma-separated VMIDs to restore (default: all guests in set)")
	pos, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return 3
	}
	if len(pos) != 2 {
		fmt.Fprintln(out, "usage: pvesnap snapshot restore <set> <name> [-vmid 100,101]")
		return 3
	}
	setName, rawName := pos[0], pos[1]
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
	if snapname == "current" {
		fmt.Fprintln(out, `"current" is the live guest state, not a restorable snapshot`)
		return 3
	}
	vmidFilter, err := parseVMIDFilter(*vmidFlag)
	if err != nil {
		fmt.Fprintln(out, err)
		return 3
	}

	client := proxmox.NewClient(cfg)
	orch := orchestrator.New(client, cfg)
	// opCtx covers both discovery and the rollback itself; the timeout clock
	// runs during the interactive prompt when --yes is not set (deliberate).
	opCtx, cancel := orch.OpContext(ctx)
	defer cancel()

	// Source of truth: what snapshots actually exist on each guest right now.
	inv := orch.DiscoverSnapshots(opCtx, set.Guests)
	targets, missing := selectSnapshotTargets(inv, snapname, vmidFilter)

	// Advisory reconciliation against state, plus query-error reporting.
	for _, m := range missing {
		if m.Err != nil {
			fmt.Fprintf(out, "warning: could not query %s/%d: %v\n", m.Guest.Node, m.Guest.VMID, m.Err)
		}
	}
	if recorded, _ := st.Find(setName, snapname); recorded != nil {
		have := map[int]bool{}
		for _, t := range targets {
			have[t.VMID] = true
		}
		for _, g := range recorded.Guests {
			if vmidFilter != nil && !vmidFilter[g.VMID] {
				continue // deliberately excluded by -vmid, not drift
			}
			if g.Status == state.StatusOK && !have[g.VMID] {
				fmt.Fprintf(out, "note: state records %d as having %q but it is not on the guest (drift)\n", g.VMID, snapname)
			}
		}
	}

	if len(targets) == 0 {
		fmt.Fprintf(out, "snapshot %q not found on any guest in set %q\n", snapname, setName)
		return 2
	}

	if !*yes {
		fmt.Fprintf(out, "About to ROLLBACK %d guests in set %q to snapshot %q.\nThis is destructive. Continue? [y/N] ", len(targets), setName, snapname)
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		if strings.ToLower(strings.TrimSpace(line)) != "y" {
			fmt.Fprintln(out, "aborted")
			return 0
		}
	}

	fmt.Fprintf(out, "rolling back %d guests to %q...\n", len(targets), snapname)
	results := orch.Restore(opCtx, targets)
	okCount, failCount, cancelled := renderResults(out, results)
	if cancelled > 0 {
		fmt.Fprintf(out, "done: %d ok, %d failed, %d cancelled\n", okCount, failCount, cancelled)
	} else {
		fmt.Fprintf(out, "done: %d ok, %d failed\n", okCount, failCount)
	}
	_ = statePath // restore does not mutate state
	return exitForCounts(okCount, failCount, cancelled)
}

func runSnapshotDelete(ctx context.Context, cfg *config.Config, st *state.Store, statePath string, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("snapshot delete", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip confirmation")
	vmidFlag := fs.String("vmid", "", "comma-separated VMIDs to delete (default: all guests in set)")
	pos, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return 3
	}
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

	okCount, failCount, cancelled := renderResults(out, results)

	if failCount == 0 && vmidFilter == nil {
		st.Remove(setName, name)
	}
	if err := st.Save(statePath); err != nil {
		fmt.Fprintf(out, "warning: failed to persist state: %v\n", err)
	}

	if cancelled > 0 {
		fmt.Fprintf(out, "done: %d ok, %d failed, %d cancelled\n", okCount, failCount, cancelled)
	} else {
		fmt.Fprintf(out, "done: %d ok, %d failed\n", okCount, failCount)
	}
	return exitForCounts(okCount, failCount, cancelled)
}

// renderResults prints the standard NODE/TYPE/VMID/STATUS/DETAIL table and returns
// counts. A result whose error is context.Canceled is shown as "cancelled" and counted
// separately (neither ok nor failed) — it was skipped by cancel-on-first-error, not run.
func renderResults(out io.Writer, results []orchestrator.Result) (ok, failed, cancelled int) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tTYPE\tVMID\tSTATUS\tDETAIL")
	for _, r := range results {
		switch {
		case r.Success:
			ok++
			fmt.Fprintf(tw, "%s\t%s\t%d\tok\t\n", r.Guest.Node, r.Guest.Type, r.Guest.VMID)
		case errors.Is(r.Err, context.Canceled):
			cancelled++
			fmt.Fprintf(tw, "%s\t%s\t%d\tcancelled\t\n", r.Guest.Node, r.Guest.Type, r.Guest.VMID)
		default:
			failed++
			fmt.Fprintf(tw, "%s\t%s\t%d\tfailed\t%s\n", r.Guest.Node, r.Guest.Type, r.Guest.VMID, errString(r.Err))
		}
	}
	_ = tw.Flush()
	return ok, failed, cancelled
}

// exitForCounts maps result counts to the pvesnap exit-code contract:
// 0 all succeeded, 2 nothing succeeded, 1 partial. Cancelled guests count as
// not-succeeded (a cancelled operation is not a success).
func exitForCounts(ok, failed, cancelled int) int {
	notOK := failed + cancelled
	switch {
	case notOK == 0:
		return 0
	case ok == 0:
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

// parseFlagsAndPositionals parses fs from args, allowing flags and positional
// arguments to appear in any order. Go's flag package otherwise stops parsing at
// the first positional, which silently drops flags written after positionals
// (e.g. `restore <set> <name> --yes`). Returns the positionals in order.
// A "--" terminator stops flag scanning, so any flag-like tokens after it are
// returned as literal positionals (the arity checks then reject them) — fine
// here because set/snapshot names never start with "-".
func parseFlagsAndPositionals(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		// fs.Args() is the suffix from the first non-flag token, so each
		// iteration consumes rest[0]; rest strictly shrinks until empty.
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		rest = rest[1:]
	}
	return positionals, nil
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

// liveSnapRow is one row of `snapshot list --live`: a snapshot name and how
// many guests in the set carry it.
type liveSnapRow struct {
	Name     string
	Count    int   // guests carrying a snapshot of this name
	Newest   int64 // max snaptime across those guests (unix seconds)
	Parented bool  // any carrier has a non-empty parent
}

// aggregateLiveSnapshots collapses per-guest inventories into one row per
// snapshot name, sorted by name. The synthetic "current" entry is excluded.
func aggregateLiveSnapshots(inv []orchestrator.SnapshotInventory) []liveSnapRow {
	type acc struct {
		count    int
		newest   int64
		parented bool
	}
	m := map[string]*acc{}
	for _, item := range inv {
		for _, s := range item.Snapshots {
			if s.Name == "current" {
				continue
			}
			a := m[s.Name]
			if a == nil {
				a = &acc{}
				m[s.Name] = a
			}
			a.count++
			if s.Snaptime > a.newest {
				a.newest = s.Snaptime
			}
			if s.Parent != "" {
				a.parented = true
			}
		}
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]liveSnapRow, 0, len(names))
	for _, name := range names {
		a := m[name]
		rows = append(rows, liveSnapRow{Name: name, Count: a.count, Newest: a.newest, Parented: a.parented})
	}
	return rows
}

// selectSnapshotTargets picks guests whose live inventory contains a snapshot
// named `name`. Guests excluded by `filter` (when non-nil) are ignored entirely.
// It returns restore targets and the inventory entries that lack the snapshot
// (either absent or a failed query, distinguishable via their Err field).
func selectSnapshotTargets(inv []orchestrator.SnapshotInventory, name string, filter map[int]bool) (targets []state.GuestRecord, missing []orchestrator.SnapshotInventory) {
	for _, item := range inv {
		if filter != nil && !filter[item.Guest.VMID] {
			continue
		}
		if item.Err != nil {
			missing = append(missing, item)
			continue
		}
		found := false
		for _, s := range item.Snapshots {
			if s.Name == name {
				found = true
				break
			}
		}
		if found {
			targets = append(targets, state.GuestRecord{
				Node:     item.Guest.Node,
				VMID:     item.Guest.VMID,
				Type:     item.Guest.Type,
				Snapname: name,
			})
		} else {
			missing = append(missing, item)
		}
	}
	return targets, missing
}
