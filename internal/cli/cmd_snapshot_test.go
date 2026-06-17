package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/veenone/pvesnap/internal/config"
	"github.com/veenone/pvesnap/internal/orchestrator"
	"github.com/veenone/pvesnap/internal/proxmox"
	"github.com/veenone/pvesnap/internal/state"
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
	cases := []struct{ ok, failed, cancelled, want int }{
		{2, 0, 0, 0},
		{0, 2, 0, 2},
		{1, 1, 0, 1},
		{0, 0, 3, 2}, // all cancelled -> nothing succeeded
		{1, 0, 2, 1}, // some ok, some cancelled -> partial
	}
	for _, c := range cases {
		if got := exitForCounts(c.ok, c.failed, c.cancelled); got != c.want {
			t.Errorf("exitForCounts(%d,%d,%d)=%d want %d", c.ok, c.failed, c.cancelled, got, c.want)
		}
	}
}

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
	code := runSnapshotRestore(context.Background(), cfg, &state.Store{}, "ignored.yaml", &out,
		[]string{"--yes", "e2e-core", "v1-5-rc1"})

	if code != 0 {
		t.Fatalf("exit code = %d, output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "1 ok, 0 failed") {
		t.Errorf("unexpected output:\n%s", out.String())
	}
}

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

func TestParseFlagsAndPositionals(t *testing.T) {
	mk := func() (*flag.FlagSet, *bool, *string) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		yes := fs.Bool("yes", false, "")
		vmid := fs.String("vmid", "", "")
		return fs, yes, vmid
	}
	fs, yes, vmid := mk()
	pos, err := parseFlagsAndPositionals(fs, []string{"set", "name", "--yes", "-vmid", "1"})
	if err != nil || len(pos) != 2 || pos[0] != "set" || pos[1] != "name" || !*yes || *vmid != "1" {
		t.Fatalf("after: pos=%v yes=%v vmid=%q err=%v", pos, *yes, *vmid, err)
	}
	fs, yes, _ = mk()
	pos, _ = parseFlagsAndPositionals(fs, []string{"--yes", "set", "name"})
	if len(pos) != 2 || !*yes {
		t.Fatalf("before: pos=%v yes=%v", pos, *yes)
	}
	fs, yes, _ = mk()
	pos, _ = parseFlagsAndPositionals(fs, []string{"set", "--yes", "name"})
	if len(pos) != 2 || pos[0] != "set" || pos[1] != "name" || !*yes {
		t.Fatalf("interspersed: pos=%v yes=%v", pos, *yes)
	}
	fs, _, _ = mk()
	if _, err := parseFlagsAndPositionals(fs, []string{"--nope"}); err == nil {
		t.Errorf("expected error for unknown flag")
	}
}

func TestRunSnapshotRestoreTrailingYes(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/lxc/101/snapshot") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[{"name":"current"},{"name":"v1-5-rc1"}]}`))
		case strings.HasSuffix(r.URL.Path, "/snapshot/v1-5-rc1/rollback") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:0:0:0:vzrollback:101:u:"}`))
		case strings.Contains(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	cfg := &config.Config{
		Nodes: []config.Node{{Name: "pve1", Endpoint: srv.URL, APIToken: "u@pam!t=x", VerifyTLS: false}},
		Sets:  []config.Set{{Name: "e2e-core", Guests: []config.Guest{{Node: "pve1", VMID: 101, Type: config.LXC}}}},
		Defaults: config.Defaults{ParallelismPerNode: 2, TaskPollInterval: time.Millisecond, TaskTimeout: time.Minute},
	}
	var out bytes.Buffer
	// flag AFTER positionals must be honored (no interactive prompt, exit 0).
	code := runSnapshotRestore(context.Background(), cfg, &state.Store{}, "x", &out, []string{"e2e-core", "v1-5-rc1", "--yes"})
	if code != 0 {
		t.Fatalf("trailing --yes should be parsed; exit=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "1 ok, 0 failed") {
		t.Errorf("unexpected: %s", out.String())
	}
}
