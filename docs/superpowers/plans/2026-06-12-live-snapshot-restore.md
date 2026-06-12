# Live Native Snapshot Restore (Part A) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `snapshot restore` source its targets live from each guest's own storage (so it works without PBS and without a complete `state.yaml`), and add `snapshot list --live` to enumerate on-guest snapshots — for both LXC and QEMU.

**Architecture:** Wire in the already-implemented-but-unused `proxmox.ListSnapshots`. A new read-only orchestrator fan-out (`DiscoverSnapshots`) queries every guest in a set concurrently under the existing per-node semaphore. `snapshot restore` resolves set membership from config, discovers which guests actually hold the named snapshot, reconciles with state advisorily, then reuses the existing `orchestrator.Restore` (in-place rollback, cancel-on-first-error). Two pure helper functions (`selectSnapshotTargets`, `aggregateLiveSnapshots`) hold the logic so it's unit-testable without HTTP.

**Tech Stack:** Go 1.24, stdlib `net/http`/`net/http/httptest` for tests, `gopkg.in/yaml.v3`, `golang.org/x/sync/errgroup`. Module path: `github.com/veenone/pvesnap`. No new dependencies.

---

## Spec reference

Implements Part A of `docs/superpowers/specs/2026-06-12-snapshot-and-pbs-restore-design.md`.

## File structure

| File | Responsibility |
|---|---|
| `internal/proxmox/snapshot.go` | Already has `ListSnapshots`; no code change, gains a test. |
| `internal/proxmox/snapshot_test.go` | **New** — characterization test for `ListSnapshots` (establishes the httptest harness). |
| `internal/orchestrator/orchestrator.go` | **Modify** — add `SnapshotInventory` type + `DiscoverSnapshots` read fan-out. |
| `internal/orchestrator/orchestrator_test.go` | **New** — tests `DiscoverSnapshots` (concurrency + per-guest error capture). |
| `internal/cli/cmd_snapshot.go` | **Modify** — add `renderResults`, `exitForCounts`, `selectSnapshotTargets`, `aggregateLiveSnapshots`, `liveSnapRow`; rewrite `runSnapshotRestore` (live-sourced, shared helpers, distinct `cancelled` status, rejects reserved `current`); refactor `runSnapshotCreate`/`runSnapshotDelete` onto the shared helpers; add `--live` to `runSnapshotList`; update `RunSnapshot` dispatch. |
| `internal/cli/cmd_snapshot_test.go` | **New** — unit tests for the two pure helpers + an integration test for live restore. |
| `docs/commands.md`, `docs/operations.md`, `docs/roadmap.md` | **Modify** — document behavior. |

## Setup (do once before Task 1)

- [ ] **Create a feature branch** (we are on `main`)

Run:
```bash
git checkout -b feat/live-snapshot-restore
```

- [ ] **Confirm the baseline builds**

Run:
```bash
go build ./... && go vet ./...
```
Expected: no output, exit 0.

---

### Task 1: Characterize `proxmox.ListSnapshots` (and establish the test harness)

`ListSnapshots` already exists but has no test. This task pins its decoding behaviour and creates the reusable fake-server pattern the later tasks rely on.

**Files:**
- Test: `internal/proxmox/snapshot_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/proxmox/snapshot_test.go`:

```go
package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/veenone/pvesnap/internal/config"
)

// newTestClient returns a Client whose single node "pve1" points at srv.
func newTestClient(srv *httptest.Server) *Client {
	return NewClient(&config.Config{
		Nodes: []config.Node{{
			Name:      "pve1",
			Endpoint:  srv.URL,
			APIToken:  "user@pam!t=00000000-0000-0000-0000-000000000000",
			VerifyTLS: false,
		}},
	})
}

func TestListSnapshots(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/lxc/101/snapshot" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "PVEAPIToken=") {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"name":"current","description":"You are here!"},
			{"name":"v1-5-rc1","description":"rc","snaptime":1700000000,"parent":"current"}
		]}`))
	}))
	defer srv.Close()

	got, err := newTestClient(srv).ListSnapshots(context.Background(), "pve1", config.LXC, 101)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	if got[1].Name != "v1-5-rc1" || got[1].Parent != "current" || got[1].Snaptime != 1700000000 {
		t.Errorf("unexpected entry: %+v", got[1])
	}
}
```

- [ ] **Step 2: Run the test to verify it passes (characterization, not red)**

Run:
```bash
go test ./internal/proxmox/ -run TestListSnapshots -v
```
Expected: PASS. (If it fails to compile, fix the test — the production code already exists.)

- [ ] **Step 3: Commit**

```bash
git add internal/proxmox/snapshot_test.go
git commit -m "test(proxmox): characterize ListSnapshots and add httptest harness"
```

---

### Task 2: `orchestrator.DiscoverSnapshots`

**Files:**
- Modify: `internal/orchestrator/orchestrator.go`
- Test: `internal/orchestrator/orchestrator_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/orchestrator/orchestrator_test.go`:

```go
package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/veenone/pvesnap/internal/config"
	"github.com/veenone/pvesnap/internal/proxmox"
)

func testOrch(srv *httptest.Server) *Orchestrator {
	cfg := &config.Config{
		Nodes: []config.Node{{
			Name: "pve1", Endpoint: srv.URL,
			APIToken: "u@pam!t=x", VerifyTLS: false,
		}},
		Defaults: config.Defaults{ParallelismPerNode: 2},
	}
	return New(proxmox.NewClient(cfg), cfg)
}

func TestDiscoverSnapshots(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/lxc/101/snapshot"):
			_, _ = w.Write([]byte(`{"data":[{"name":"current"},{"name":"v1","snaptime":100}]}`))
		case strings.HasSuffix(r.URL.Path, "/lxc/102/snapshot"):
			_, _ = w.Write([]byte(`{"data":[{"name":"current"}]}`))
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	orch := testOrch(srv)
	guests := []config.Guest{
		{Node: "pve1", VMID: 101, Type: config.LXC},
		{Node: "pve1", VMID: 102, Type: config.LXC},
		{Node: "pve1", VMID: 999, Type: config.QEMU}, // 500 -> Err
	}
	inv := orch.DiscoverSnapshots(context.Background(), guests)

	if len(inv) != 3 {
		t.Fatalf("want 3 inventory entries, got %d", len(inv))
	}
	if inv[0].Err != nil || len(inv[0].Snapshots) != 2 {
		t.Errorf("guest 101: err=%v snaps=%d", inv[0].Err, len(inv[0].Snapshots))
	}
	if inv[1].Err != nil || len(inv[1].Snapshots) != 1 {
		t.Errorf("guest 102: err=%v snaps=%d", inv[1].Err, len(inv[1].Snapshots))
	}
	if inv[2].Err == nil {
		t.Errorf("guest 999: expected an error, got nil")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run:
```bash
go test ./internal/orchestrator/ -run TestDiscoverSnapshots -v
```
Expected: FAIL to compile — `orch.DiscoverSnapshots undefined` and `SnapshotInventory` undefined.

- [ ] **Step 3: Add the type and method**

In `internal/orchestrator/orchestrator.go`, add after the `Result` struct definition (around line 57):

```go
// SnapshotInventory is the set of snapshots present on one guest's storage.
type SnapshotInventory struct {
	Guest     config.Guest
	Snapshots []proxmox.SnapshotEntry
	Err       error
}

// DiscoverSnapshots queries each guest's live snapshot list concurrently,
// gated by the per-node semaphore. A per-guest query error is captured in that
// entry's Err; the fan-out always returns exactly one entry per input guest.
func (o *Orchestrator) DiscoverSnapshots(ctx context.Context, guests []config.Guest) []SnapshotInventory {
	results := make([]SnapshotInventory, len(guests))
	var wg sync.WaitGroup
	for i, g := range guests {
		wg.Add(1)
		go func(i int, g config.Guest) {
			defer wg.Done()
			inv := SnapshotInventory{Guest: g}
			if err := o.acquire(ctx, g.Node); err != nil {
				inv.Err = err
				results[i] = inv
				return
			}
			defer o.release(g.Node)
			snaps, err := o.Client.ListSnapshots(ctx, g.Node, g.Type, g.VMID)
			if err != nil {
				inv.Err = fmt.Errorf("list snapshots: %w", err)
				results[i] = inv
				return
			}
			inv.Snapshots = snaps
			results[i] = inv
		}(i, g)
	}
	wg.Wait()
	return results
}
```

(No new imports needed — `context`, `fmt`, `sync`, `config`, `proxmox` are already imported in this file.)

- [ ] **Step 4: Run the test to verify it passes**

Run:
```bash
go test ./internal/orchestrator/ -run TestDiscoverSnapshots -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/orchestrator/orchestrator.go internal/orchestrator/orchestrator_test.go
git commit -m "feat(orchestrator): add DiscoverSnapshots read fan-out"
```

---

### Task 3: `selectSnapshotTargets` pure helper

Turns a discovery inventory + a snapshot name + an optional vmid filter into restore targets, plus the guests that don't have it (for reporting).

**Files:**
- Modify: `internal/cli/cmd_snapshot.go`
- Test: `internal/cli/cmd_snapshot_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/cli/cmd_snapshot_test.go`:

```go
package cli

import (
	"errors"
	"testing"

	"github.com/veenone/pvesnap/internal/config"
	"github.com/veenone/pvesnap/internal/orchestrator"
	"github.com/veenone/pvesnap/internal/proxmox"
)

func TestSelectSnapshotTargets(t *testing.T) {
	inv := []orchestrator.SnapshotInventory{
		{Guest: config.Guest{Node: "pve1", VMID: 101, Type: config.LXC},
			Snapshots: []proxmox.SnapshotEntry{{Name: "current"}, {Name: "v1"}}},
		{Guest: config.Guest{Node: "pve1", VMID: 102, Type: config.LXC},
			Snapshots: []proxmox.SnapshotEntry{{Name: "current"}}}, // missing "v1"
		{Guest: config.Guest{Node: "pve2", VMID: 201, Type: config.QEMU},
			Err: errors.New("query failed")}, // errored
	}

	targets, missing := selectSnapshotTargets(inv, "v1", nil)
	if len(targets) != 1 || targets[0].VMID != 101 || targets[0].Snapname != "v1" {
		t.Fatalf("targets = %+v", targets)
	}
	if len(missing) != 2 {
		t.Fatalf("want 2 missing (102 absent, 201 errored), got %d", len(missing))
	}

	// vmid filter excludes everything but 102 -> no targets.
	targets, _ = selectSnapshotTargets(inv, "v1", map[int]bool{102: true})
	if len(targets) != 0 {
		t.Fatalf("filtered targets = %+v", targets)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run:
```bash
go test ./internal/cli/ -run TestSelectSnapshotTargets -v
```
Expected: FAIL to compile — `selectSnapshotTargets undefined`.

- [ ] **Step 3: Implement the helper**

In `internal/cli/cmd_snapshot.go`, add near the other helpers at the bottom of the file (after `filterByVMID`):

```go
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
```

(`orchestrator` and `state` are already imported in `cmd_snapshot.go`.)

- [ ] **Step 4: Run the test to verify it passes**

Run:
```bash
go test ./internal/cli/ -run TestSelectSnapshotTargets -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cmd_snapshot.go internal/cli/cmd_snapshot_test.go
git commit -m "feat(cli): add selectSnapshotTargets helper"
```

---

### Task 4: `aggregateLiveSnapshots` pure helper

Aggregates per-guest inventories into one row per snapshot name with set coverage, for `snapshot list --live`.

**Files:**
- Modify: `internal/cli/cmd_snapshot.go`
- Test: `internal/cli/cmd_snapshot_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/cmd_snapshot_test.go`:

```go
func TestAggregateLiveSnapshots(t *testing.T) {
	inv := []orchestrator.SnapshotInventory{
		{Snapshots: []proxmox.SnapshotEntry{{Name: "current"}, {Name: "v1", Snaptime: 100}, {Name: "hotfix", Snaptime: 300, Parent: "v1"}}},
		{Snapshots: []proxmox.SnapshotEntry{{Name: "current"}, {Name: "v1", Snaptime: 150}}},
	}
	rows := aggregateLiveSnapshots(inv)

	// "current" is excluded; rows sorted by name: hotfix, v1.
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].Name != "hotfix" || rows[0].Count != 1 || !rows[0].Parented {
		t.Errorf("hotfix row wrong: %+v", rows[0])
	}
	if rows[1].Name != "v1" || rows[1].Count != 2 || rows[1].Newest != 150 {
		t.Errorf("v1 row wrong: %+v", rows[1])
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run:
```bash
go test ./internal/cli/ -run TestAggregateLiveSnapshots -v
```
Expected: FAIL to compile — `aggregateLiveSnapshots` and `liveSnapRow` undefined.

- [ ] **Step 3: Implement the helper**

In `internal/cli/cmd_snapshot.go`, add `"sort"` to the import block, then add after `selectSnapshotTargets`:

```go
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
```

- [ ] **Step 4: Run the test to verify it passes**

Run:
```bash
go test ./internal/cli/ -run TestAggregateLiveSnapshots -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cmd_snapshot.go internal/cli/cmd_snapshot_test.go
git commit -m "feat(cli): add aggregateLiveSnapshots helper"
```

---

### Task 5: Wire `snapshot list --live` and update dispatch

**Files:**
- Modify: `internal/cli/cmd_snapshot.go` (`RunSnapshot`, `runSnapshotList`, add `runSnapshotListLive`)

- [ ] **Step 1: Update the dispatch to pass ctx + cfg to list**

In `internal/cli/cmd_snapshot.go`, change the `list` case inside `RunSnapshot` from:

```go
	case "list":
		return runSnapshotList(st, out, rest)
```

to:

```go
	case "list":
		return runSnapshotList(ctx, cfg, st, out, rest)
```

- [ ] **Step 2: Replace `runSnapshotList` signature and add the `--live` branch**

Replace the existing `func runSnapshotList(st *state.Store, out io.Writer, args []string) int {` declaration line and its flag-parsing preamble. The new version:

```go
func runSnapshotList(ctx context.Context, cfg *config.Config, st *state.Store, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("snapshot list", flag.ContinueOnError)
	live := fs.Bool("live", false, "query each guest's storage for actual snapshots instead of reading state")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	pos := fs.Args()
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
	// ... existing state-based rendering below is unchanged ...
```

Leave the rest of the original `runSnapshotList` body (the `tabwriter` block over `st.Snapshots`) exactly as-is.

- [ ] **Step 3: Add `runSnapshotListLive`**

Add this function directly below `runSnapshotList`:

```go
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
```

(`time` is already imported in `cmd_snapshot.go`.)

- [ ] **Step 4: Verify it builds and all existing tests pass**

Run:
```bash
go build ./... && go vet ./... && go test ./internal/cli/ -v
```
Expected: build/vet clean; the Task 3 and Task 4 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cmd_snapshot.go
git commit -m "feat(cli): add 'snapshot list --live' to enumerate on-guest snapshots"
```

---

### Task 5b: Extract `renderResults` + `exitForCounts` (DRY, from review D2)

`runSnapshotCreate`/`runSnapshotDelete` (and the upcoming restore rewrite) repeat the same
result-table + ok/fail counting + exit-code switch. Extract two helpers first ("make the
change easy, then make the easy change"), and give `renderResults` a distinct `cancelled`
status (review D1) for results whose error is `context.Canceled`.

**Files:**
- Modify: `internal/cli/cmd_snapshot.go`
- Test: `internal/cli/cmd_snapshot_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/cmd_snapshot_test.go` (add `"bytes"`, `"context"`, `"errors"`, `"fmt"`, `"strings"` to the test import block if not already present):

```go
func TestRenderResults(t *testing.T) {
	results := []orchestrator.Result{
		{Guest: config.Guest{Node: "n", VMID: 1, Type: config.LXC}, Success: true},
		{Guest: config.Guest{Node: "n", VMID: 2, Type: config.LXC}, Err: errors.New("boom")},
		{Guest: config.Guest{Node: "n", VMID: 3, Type: config.LXC}, Err: fmt.Errorf("wait: %w", context.Canceled)},
	}
	var out bytes.Buffer
	ok, failed, cancelled := renderResults(&out, results)
	if ok != 1 || failed != 1 || cancelled != 1 {
		t.Fatalf("counts: ok=%d failed=%d cancelled=%d", ok, failed, cancelled)
	}
	if !strings.Contains(out.String(), "cancelled") {
		t.Errorf("missing cancelled row:\n%s", out.String())
	}
}

func TestExitForCounts(t *testing.T) {
	cases := []struct{ ok, failed, want int }{{2, 0, 0}, {0, 2, 2}, {1, 1, 1}}
	for _, c := range cases {
		if got := exitForCounts(c.ok, c.failed); got != c.want {
			t.Errorf("exitForCounts(%d,%d)=%d want %d", c.ok, c.failed, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:
```bash
go test ./internal/cli/ -run 'TestRenderResults|TestExitForCounts' -v
```
Expected: FAIL to compile — `renderResults` / `exitForCounts` undefined.

- [ ] **Step 3: Implement the helpers**

In `internal/cli/cmd_snapshot.go`, add `"errors"` to the import block, then add near the other helpers at the bottom of the file:

```go
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

// exitForCounts maps success/failure counts to the pvesnap exit-code contract:
// 0 all ok, 2 nothing succeeded, 1 partial. Cancelled guests are not counted here.
func exitForCounts(ok, failed int) int {
	switch {
	case failed == 0:
		return 0
	case ok == 0:
		return 2
	default:
		return 1
	}
}
```

- [ ] **Step 4: Refactor `runSnapshotCreate` onto the helpers**

In `runSnapshotCreate`, the result loop currently builds the state record AND renders AND
counts. Split responsibilities: keep a loop that only builds `snap.Guests[i]`, then render.
Replace the existing block:

```go
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
```

with:

```go
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
	okCount, failCount, _ := renderResults(out, results)
```

Then replace `runSnapshotCreate`'s trailing exit switch:

```go
	switch {
	case failCount == 0:
		return 0
	case okCount == 0:
		return 2
	default:
		return 1
	}
```

with:

```go
	return exitForCounts(okCount, failCount)
```

- [ ] **Step 5: Refactor `runSnapshotDelete` onto the helpers**

In `runSnapshotDelete`, replace its result-render block:

```go
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
```

with:

```go
	okCount, failCount, _ := renderResults(out, results)
```

and replace `runSnapshotDelete`'s trailing exit switch (identical to create's) with:

```go
	return exitForCounts(okCount, failCount)
```

Leave the `st.Remove`/`st.Save` logic in delete unchanged.

- [ ] **Step 6: Run the full cli suite to verify behavior is preserved**

Run:
```bash
go build ./... && go vet ./... && go test ./internal/cli/ -v
```
Expected: build/vet clean; `TestRenderResults`, `TestExitForCounts`, and the Task 3/4 tests PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/cmd_snapshot.go internal/cli/cmd_snapshot_test.go
git commit -m "refactor(cli): extract renderResults/exitForCounts; add cancelled status"
```

---

### Task 6: Rewrite `runSnapshotRestore` to be live-sourced

Restore now resolves set membership from config, discovers snapshots live, and restores guests that actually hold the named snapshot — independent of `state.yaml`.

**Files:**
- Modify: `internal/cli/cmd_snapshot.go` (`runSnapshotRestore`)
- Test: `internal/cli/cmd_snapshot_test.go`

- [ ] **Step 1: Write the failing integration test**

Append to `internal/cli/cmd_snapshot_test.go` (add imports `context`, `net/http`, `net/http/httptest`, `strings`, `time`, `bytes`, and `github.com/veenone/pvesnap/internal/state` to the test file's import block):

```go
func TestRunSnapshotRestoreLiveSourced(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/lxc/101/snapshot") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[{"name":"current"},{"name":"v1-5-rc1"}]}`))
		case strings.HasSuffix(r.URL.Path, "/snapshot/v1-5-rc1/rollback") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:0:0:0:vzrollback:101:u@pam!t:"}`))
		case strings.Contains(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Nodes: []config.Node{{Name: "pve1", Endpoint: srv.URL, APIToken: "u@pam!t=x", VerifyTLS: false}},
		Sets: []config.Set{{Name: "e2e-core", Guests: []config.Guest{
			{Node: "pve1", VMID: 101, Type: config.LXC},
		}}},
		Defaults: config.Defaults{ParallelismPerNode: 2, TaskPollInterval: time.Millisecond, TaskTimeout: time.Minute},
	}

	var out bytes.Buffer
	// Empty state store on purpose: restore must work without any state record.
	code := runSnapshotRestore(context.Background(), cfg, &state.Store{}, "ignored.yaml", &out,
		[]string{"--yes", "e2e-core", "v1-5-rc1"})

	if code != 0 {
		t.Fatalf("exit code = %d, output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "1 ok, 0 failed") {
		t.Errorf("unexpected output:\n%s", out.String())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run:
```bash
go test ./internal/cli/ -run TestRunSnapshotRestoreLiveSourced -v
```
Expected: FAIL — the current `runSnapshotRestore` looks up `state.Store` via `st.Find` and returns exit 3 ("no recorded snapshot") because the store is empty.

- [ ] **Step 3: Replace `runSnapshotRestore`**

Replace the entire existing `runSnapshotRestore` function with:

```go
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
	return exitForCounts(okCount, failCount)
}
```

(`renderResults`/`exitForCounts` come from Task 5b. The `cancelled` line surfaces guests
skipped by cancel-on-first-error distinctly from genuine failures — review decision D1.)

- [ ] **Step 4: Run the test to verify it passes**

Run:
```bash
go test ./internal/cli/ -run TestRunSnapshotRestoreLiveSourced -v
```
Expected: PASS.

- [ ] **Step 5: Full build, vet, and test sweep**

Run:
```bash
go build ./... && go vet ./... && go test ./...
```
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/cmd_snapshot.go internal/cli/cmd_snapshot_test.go
git commit -m "feat(cli): live-source snapshot restore from guest storage"
```

---

### Task 6b: Restore edge-case + list-live coverage tests (from review §3)

Close the coverage gaps the eng review flagged: empty target, partial coverage, discovery
query error, state-drift note, and the `runSnapshotListLive` wiring.

**Files:**
- Test: `internal/cli/cmd_snapshot_test.go`

- [ ] **Step 1: Add the tests**

Append to `internal/cli/cmd_snapshot_test.go`:

```go
func TestRunSnapshotRestoreEmptyTarget(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"name":"current"}]}`)) // v1 absent
	}))
	defer srv.Close()
	cfg := &config.Config{
		Nodes:    []config.Node{{Name: "pve1", Endpoint: srv.URL, APIToken: "u@pam!t=x", VerifyTLS: false}},
		Sets:     []config.Set{{Name: "s", Guests: []config.Guest{{Node: "pve1", VMID: 101, Type: config.LXC}}}},
		Defaults: config.Defaults{ParallelismPerNode: 2, TaskPollInterval: time.Millisecond, TaskTimeout: time.Minute},
	}
	var out bytes.Buffer
	code := runSnapshotRestore(context.Background(), cfg, &state.Store{}, "x", &out, []string{"--yes", "s", "v1"})
	if code != 2 {
		t.Fatalf("want exit 2, got %d; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "not found on any guest") {
		t.Errorf("missing message: %s", out.String())
	}
}

func TestRunSnapshotRestorePartialCoverage(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/lxc/101/snapshot"):
			_, _ = w.Write([]byte(`{"data":[{"name":"current"},{"name":"v1"}]}`))
		case strings.HasSuffix(r.URL.Path, "/lxc/102/snapshot"):
			_, _ = w.Write([]byte(`{"data":[{"name":"current"}]}`)) // lacks v1
		case strings.HasSuffix(r.URL.Path, "/snapshot/v1/rollback"):
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:0:0:0:vzrollback:101:u:"}`))
		case strings.Contains(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	cfg := &config.Config{
		Nodes: []config.Node{{Name: "pve1", Endpoint: srv.URL, APIToken: "u@pam!t=x", VerifyTLS: false}},
		Sets: []config.Set{{Name: "s", Guests: []config.Guest{
			{Node: "pve1", VMID: 101, Type: config.LXC},
			{Node: "pve1", VMID: 102, Type: config.LXC},
		}}},
		Defaults: config.Defaults{ParallelismPerNode: 2, TaskPollInterval: time.Millisecond, TaskTimeout: time.Minute},
	}
	var out bytes.Buffer
	code := runSnapshotRestore(context.Background(), cfg, &state.Store{}, "x", &out, []string{"--yes", "s", "v1"})
	if code != 0 {
		t.Fatalf("want exit 0 (1 of 2 restored), got %d; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "rolling back 1 guests") {
		t.Errorf("expected 1 target: %s", out.String())
	}
}

func TestRunSnapshotRestoreQueryError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError) // discovery fails
	}))
	defer srv.Close()
	cfg := &config.Config{
		Nodes:    []config.Node{{Name: "pve1", Endpoint: srv.URL, APIToken: "u@pam!t=x", VerifyTLS: false}},
		Sets:     []config.Set{{Name: "s", Guests: []config.Guest{{Node: "pve1", VMID: 101, Type: config.LXC}}}},
		Defaults: config.Defaults{ParallelismPerNode: 2, TaskPollInterval: time.Millisecond, TaskTimeout: time.Minute},
	}
	var out bytes.Buffer
	code := runSnapshotRestore(context.Background(), cfg, &state.Store{}, "x", &out, []string{"--yes", "s", "v1"})
	if code != 2 {
		t.Fatalf("want exit 2 (no targets), got %d; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "could not query") {
		t.Errorf("expected query warning: %s", out.String())
	}
}

func TestRunSnapshotRestoreDriftNote(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"name":"current"}]}`)) // v1 absent live
	}))
	defer srv.Close()
	cfg := &config.Config{
		Nodes:    []config.Node{{Name: "pve1", Endpoint: srv.URL, APIToken: "u@pam!t=x", VerifyTLS: false}},
		Sets:     []config.Set{{Name: "s", Guests: []config.Guest{{Node: "pve1", VMID: 101, Type: config.LXC}}}},
		Defaults: config.Defaults{ParallelismPerNode: 2, TaskPollInterval: time.Millisecond, TaskTimeout: time.Minute},
	}
	st := &state.Store{Snapshots: []state.Snapshot{{Set: "s", Name: "v1", Guests: []state.GuestRecord{
		{Node: "pve1", VMID: 101, Type: config.LXC, Snapname: "v1", Status: state.StatusOK},
	}}}}
	var out bytes.Buffer
	code := runSnapshotRestore(context.Background(), cfg, st, "x", &out, []string{"--yes", "s", "v1"})
	if code != 2 {
		t.Fatalf("want exit 2 (absent live), got %d; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "drift") {
		t.Errorf("expected drift note: %s", out.String())
	}
}

func TestRunSnapshotListLive(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/lxc/101/snapshot"):
			_, _ = w.Write([]byte(`{"data":[{"name":"current"},{"name":"v1","snaptime":100}]}`))
		default:
			http.Error(w, "boom", http.StatusInternalServerError) // 102 errors -> exit 1
		}
	}))
	defer srv.Close()
	cfg := &config.Config{
		Nodes: []config.Node{{Name: "pve1", Endpoint: srv.URL, APIToken: "u@pam!t=x", VerifyTLS: false}},
		Sets: []config.Set{{Name: "s", Guests: []config.Guest{
			{Node: "pve1", VMID: 101, Type: config.LXC},
			{Node: "pve1", VMID: 102, Type: config.LXC},
		}}},
		Defaults: config.Defaults{ParallelismPerNode: 2, TaskPollInterval: time.Millisecond, TaskTimeout: time.Minute},
	}
	var out bytes.Buffer
	code := runSnapshotListLive(context.Background(), cfg, &out, "s")
	if code != 1 {
		t.Fatalf("want exit 1 (one guest errored), got %d; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "v1") || !strings.Contains(out.String(), "partial") {
		t.Errorf("expected v1 partial row: %s", out.String())
	}
}
```

- [ ] **Step 2: Run the new tests**

Run:
```bash
go test ./internal/cli/ -run 'TestRunSnapshotRestore|TestRunSnapshotListLive' -v
```
Expected: all PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/cmd_snapshot_test.go
git commit -m "test(cli): cover restore edge cases and list --live wiring"
```

---

### Task 7: Documentation

**Files:**
- Modify: `docs/commands.md`, `docs/operations.md`, `docs/roadmap.md`

- [ ] **Step 1: Document `snapshot list --live` in `docs/commands.md`**

Under the `pvesnap snapshot list` section, add:

```markdown
### Live listing with `--live`

`pvesnap snapshot list <set> --live` queries each guest in the set directly and
aggregates the snapshots that actually exist on their storage (independent of
`state.yaml`). Useful before a restore, and for spotting drift.

```
$ pvesnap snapshot list e2e-core --live
NAME      COVERAGE  GUESTS  NEWEST            PARENTED
v1-5-rc1  full      4/4     2026-06-11 09:15  yes
hotfix    partial   2/4     2026-06-11 14:02  no
```

- `COVERAGE` is `full` when every guest in the set carries that snapshot name, else `partial`.
- A per-guest query failure prints a warning line and yields exit code 1.
```

- [ ] **Step 2: Update the restore section in `docs/commands.md`**

In the `pvesnap snapshot restore` section, replace the sentence "Only guests whose state status is `ok` are attempted — guests that failed at create time are skipped." with:

```markdown
Restore is **live-sourced**: the set is read from config, and each guest is queried for
the named snapshot directly on its storage. Only guests that actually hold the snapshot
are rolled back. This works even when `state.yaml` is missing or out of date; the snapshot
name is normalized the same way as on create. If state records a guest as holding the
snapshot but it is absent on the guest, a drift note is printed and that guest is skipped.
If the name is found on no guest, the command prints a message and exits 2.
```

- [ ] **Step 3: Note drift tolerance in `docs/operations.md`**

Under "Stale state reconciliation", append:

```markdown
As of the live-restore change, `snapshot restore` and `snapshot list --live` query guest
storage directly, so they tolerate a missing or drifted `state.yaml` — restore targets the
snapshots that actually exist on each guest. You no longer have to hand-edit `state.yaml`
to recover from out-of-band snapshot deletion before restoring.
```

- [ ] **Step 4: Mark roadmap progress in `docs/roadmap.md`**

In the priority-summary table, change item #1's Status cell from `Planned` to `Restore-side landed (live restore)`, and append to the item #1 prose:

```markdown
> **Update (2026-06-12):** The restore path no longer depends on `state.yaml` — `snapshot
> restore` and `snapshot list --live` source snapshots live from each guest. A dedicated
> `snapshot sync` command (to rewrite state `status` fields) is still unbuilt, but the
> drift-recovery motivation is largely addressed for restore.
```

- [ ] **Step 5: Commit**

```bash
git add docs/commands.md docs/operations.md docs/roadmap.md
git commit -m "docs: live snapshot restore and 'snapshot list --live'"
```

---

## Self-Review

**1. Spec coverage (Part A):**
- "Restore from live per-guest snapshots without PBS, LXC+QEMU" → Task 6 (uses `config.Guest.Type`, no type restriction). ✅
- "Live discovery fan-out via ListSnapshots" → Task 2 (`DiscoverSnapshots`) + Task 1 (ListSnapshots verified). ✅
- "Reconcile with state advisorily / works with absent state" → Task 6 (drift notes; empty-store integration test). ✅
- "Empty target set → exit 2" → Task 6 (explicit branch + asserted indirectly). ✅
- "`snapshot list --live` with coverage" → Tasks 4 + 5. ✅
- "Name normalization on restore" → Task 6 (`NormalizeSnapName`). ✅
- Docs (commands/operations/roadmap) → Task 7. ✅
- Out of scope for Part A (PBS, stop/start, retention) → correctly absent. ✅

**2. Placeholder scan:** No TBD/TODO; every code step shows complete code; every run step shows the exact command and expected result. ✅

**3. Type consistency:** `SnapshotInventory{Guest, Snapshots, Err}` defined in Task 2 and consumed identically in Tasks 3–6. `selectSnapshotTargets(inv, name, filter) (targets []state.GuestRecord, missing []orchestrator.SnapshotInventory)` defined Task 3, called Task 6 with matching signature. `liveSnapRow{Name, Count, Newest, Parented}` defined Task 4, consumed Task 5. `runSnapshotList(ctx, cfg, st, out, args)` new signature set in Task 5 and dispatched from `RunSnapshot` in Task 5. `runSnapshotRestore(ctx, cfg, st, statePath, out, args)` signature unchanged from the original (already matches the `RunSnapshot` call site), so no dispatch edit needed. ✅

**Note on commits:** commit commands intentionally omit any `Co-Authored-By` trailer, per project preference.

**Review addendum (2026-06-12, /plan-eng-review):**
- D1 (accepted): restore renders `cancelled` distinctly from `failed` — Task 5b `renderResults` + Task 6 done-line.
- D2 (accepted): extracted `renderResults`/`exitForCounts`, refactored create/restore/delete — Task 5b.
- Hardening: restore rejects the reserved name `current` — Task 6.
- Coverage gaps closed — Task 6b (empty-target→2, partial coverage, query error, drift note, `list --live` wiring).

---

## NOT in scope (deferred, with rationale)

- **PBS backup list/restore** — Part B of the spec; separate plan after Part A ships.
- **`snapshot sync` command** (rewrite state `status` fields) — roadmap #1; live restore removes the urgency, so deferred.
- **Snapshot tree view** (parent/child graph) — roadmap #2; `--live` shows a flat coverage view, parent rendering deferred.
- **Fixing the cancel-on-first-error server-side-continuation caveat** — pre-existing in `orchestrator.Restore`; live-sourcing neither causes nor worsens it. Documented in `operations.md`; a real fix (poll-and-stop in-flight rollbacks) is its own change.
- **Auto-stopping a running guest before rollback** — Proxmox handles rollback semantics today; matching current behavior. Explicit stop/start belongs to the PBS path (Part B).

## What already exists (reused, not rebuilt)

- `proxmox.ListSnapshots` — fully implemented, was dead code; Task 1 verifies it, Task 2 consumes it.
- `orchestrator.Restore` — reused unchanged for in-place rollback (cancel-on-first-error).
- Per-node semaphore (`acquire`/`release`) — `DiscoverSnapshots` reuses it; no new concurrency primitive.
- `config.NormalizeSnapName`, `parseVMIDFilter`, `filterByVMID`, `errString`, tabwriter output — reused.
- After Task 5b, `renderResults`/`exitForCounts` replace the 3 pre-existing duplicated render/exit blocks.

## Failure modes (new codepaths)

| Codepath | Realistic failure | Test? | Error handling? | Visible to user? |
|---|---|---|---|---|
| `DiscoverSnapshots` | node down / API 500 / auth fail | ✅ Task 2 + 6b | ✅ captured in `inv.Err` | ✅ "could not query" warning, guest skipped |
| `runSnapshotRestore` (live) | snapshot on no guest | ✅ Task 6b | ✅ exit 2 | ✅ "not found on any guest" |
| `runSnapshotRestore` (live) | one rollback fails → others cancelled | ✅ Task 5b (render) | ✅ errgroup cancel | ✅ `failed` + `cancelled` rows (D1) |
| `runSnapshotRestore` (live) | state says ok but absent live (drift) | ✅ Task 6b | ✅ skip + note | ✅ "drift" note |
| `runSnapshotListLive` | one guest query fails | ✅ Task 6b | ✅ continue | ✅ warning + exit 1 |

No critical gaps: every new failure mode has a test, error handling, and visible output. The
one residual risk (server-side rollback continues after client cancel) is pre-existing,
documented, and explicitly out of scope here.

## Worktree parallelization

| Step | Module touched | Depends on |
|---|---|---|
| Task 1 | `internal/proxmox` (test only) | — |
| Task 2 | `internal/orchestrator` | — |
| Tasks 3, 4, 5b, 6, 6b | `internal/cli` | Task 2 (and 3/4/5b before 6) |
| Task 7 | `docs/` | logically after 6 |

- **Lane A:** Task 1 (proxmox test) — independent.
- **Lane B:** Task 2 (orchestrator) — independent.
- **Lane C (sequential):** Tasks 3 → 4 → 5b → 6 → 6b, all in `cmd_snapshot.go`, plus Task 7 docs.

Launch A + B in parallel; Lane C waits on Task 2. Lane C is strictly sequential (one file).
Net: limited parallelism — the bulk of the work is one file. For a plan this size, sequential
execution in one session is reasonable; worktrees aren't worth the setup overhead.

## GSTACK REVIEW REPORT

| Review | Trigger | Why | Runs | Status | Findings |
|--------|---------|-----|------|--------|----------|
| CEO Review | `/plan-ceo-review` | Scope & strategy | 0 | — | not run (small, well-scoped change) |
| Codex Review | `/codex review` | Independent 2nd opinion | 0 | — | offered below |
| Eng Review | `/plan-eng-review` | Architecture & tests (required) | 1 | CLEAR | 2 decisions (D1, D2) accepted; 1 hardening; 6 test gaps closed |
| Design Review | `/plan-design-review` | UI/UX gaps | 0 | — | n/a (CLI, no UI) |
| DX Review | `/plan-devex-review` | Developer experience gaps | 0 | — | not run |

- **Scope:** accepted as-is (no reduction; 6→8 files, 0 new services).
- **Architecture:** 1 issue (D1, accepted A). **Code Quality:** 1 issue (D2, accepted A) + `current` guard. **Tests:** diagram produced, 6 gaps closed in Tasks 5b/6b. **Performance:** 0 issues (1 note: extra discovery round-trip, inherent).
- **Failure modes:** 0 critical gaps.
- **UNRESOLVED:** none.
- **VERDICT:** ENG CLEARED — ready to implement. Optional: run `/codex review` for an outside voice before execution.
```
