# PBS Backup List & Restore (Part B) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `pvesnap backup list` and `pvesnap backup restore` — enumerate PBS backup points per set and restore guests in-place from a chosen point — consuming backups via the Proxmox node storage API.

**Architecture:** PBS is registered as a PVE storage, so backups are reachable through the existing node API and tokens. Listing is a live read fan-out (no `state.yaml`). Restore is in-place (overwrite same VMID) with a per-guest stop → restore → start sequence, run under `errgroup` cancel-on-first-error like `snapshot restore`. Backup points are per-guest timestamped volumes (no shared name), so precise restore targets one `-vmid` + `-volid`, and set-wide restore uses a `--latest`/`--at` selector.

**Tech Stack:** Go 1.24+, stdlib `net/http`/`net/http/httptest` for tests, `gopkg.in/yaml.v3`, `golang.org/x/sync/errgroup`. Module: `github.com/veenone/pvesnap`. No new dependencies.

---

## Spec reference

Implements Part B of `docs/superpowers/specs/2026-06-12-snapshot-and-pbs-restore-design.md`.

## Dependency & base branch

Part B reuses these from Part A (`feat/live-snapshot-restore`):
- `renderResults(out, results) (ok, failed, cancelled int)` and `exitForCounts(ok, failed, cancelled int) int` in `internal/cli/cmd_snapshot.go`.
- The `newTestClient(srv)` helper in `internal/proxmox/snapshot_test.go` (package `proxmox`).
- The `orchestrator.Result` struct and per-node semaphore (`acquire`/`release`).

**Base this branch on Part A.** At the Setup step, branch from `feat/live-snapshot-restore`. If Part A has already merged to `main`, branch from `main` instead.

## File structure

| File | Responsibility |
|---|---|
| `internal/config/config.go` | **Modify** — add `Defaults.PBSStorage`, `Set.PBSStorage`, `ResolvePBSStorage`. |
| `internal/config/config_test.go` | **New** — test `ResolvePBSStorage`. |
| `internal/proxmox/backup.go` | **New** — `BackupPoint`, `ListBackups`, `RestoreBackup`. |
| `internal/proxmox/backup_test.go` | **New** — tests for the above (httptest). |
| `internal/proxmox/guest.go` | **New** — `GuestStatus`, `StopGuest`, `StartGuest`. |
| `internal/proxmox/guest_test.go` | **New** — tests for the above. |
| `internal/orchestrator/orchestrator.go` | **Modify** — add `BackupListResult`, `ListBackups`, `BackupTarget`, `RestoreBackup`. |
| `internal/orchestrator/orchestrator_backup_test.go` | **New** — tests for both. |
| `internal/cli/cmd_backup.go` | **New** — `RunBackup` (`list`/`restore`), `runBackupList`, `runBackupRestore`, helpers `parseAtTime`, `pickBackup`, `selectBackupTargets`, `humanizeBytes`. |
| `internal/cli/cmd_backup_test.go` | **New** — tests for the helpers + list/restore integration. |
| `cmd/pvesnap/main.go` | **Modify** — dispatch `backup`; usage text. |
| `examples/config.yaml` | **Modify** — `pbs_storage` example. |
| `docs/{commands,operations,proxmox-api,roadmap}.md` | **Modify** — document the commands and endpoints. |

## Setup (do once before Task 1)

- [ ] **Create the feature branch based on Part A**

Run (Part A unmerged):
```bash
git checkout feat/live-snapshot-restore && git checkout -b feat/pbs-backup-restore
```
If Part A already merged to main: `git checkout main && git pull && git checkout -b feat/pbs-backup-restore`.

- [ ] **Confirm baseline builds**

Run:
```bash
go build ./... && go vet ./... && go test ./...
```
Expected: all PASS (Part A tests included).

---

### Task 1: Config — `pbs_storage` with per-set override

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/config/config_test.go`:

```go
package config

import "testing"

func TestResolvePBSStorage(t *testing.T) {
	c := &Config{Defaults: Defaults{PBSStorage: "pbs-main"}}
	if got := c.ResolvePBSStorage(Set{}); got != "pbs-main" {
		t.Errorf("default: got %q, want pbs-main", got)
	}
	if got := c.ResolvePBSStorage(Set{PBSStorage: "pbs-set"}); got != "pbs-set" {
		t.Errorf("override: got %q, want pbs-set", got)
	}
	empty := &Config{}
	if got := empty.ResolvePBSStorage(Set{}); got != "" {
		t.Errorf("unset: got %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/config/ -run TestResolvePBSStorage -v`
Expected: compile failure — `PBSStorage` field and `ResolvePBSStorage` undefined.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add a field to `Defaults`:
```go
	PBSStorage         string        `yaml:"pbs_storage"`
```
add a field to `Set` (after `Description`):
```go
	PBSStorage  string  `yaml:"pbs_storage,omitempty"`
```
and add this method (near `FindSet`):
```go
// ResolvePBSStorage returns the PBS storage id for a set: the set's override if
// set, otherwise the global default. Empty string means "not configured".
func (c *Config) ResolvePBSStorage(s Set) string {
	if s.PBSStorage != "" {
		return s.PBSStorage
	}
	return c.Defaults.PBSStorage
}
```
Do not change `applyDefaults`/`validate` — `pbs_storage` is optional so snapshot-only configs keep working.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/config/ -run TestResolvePBSStorage -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add pbs_storage with per-set override"
```

---

### Task 2: proxmox `ListBackups`

**Files:**
- Create: `internal/proxmox/backup.go`
- Test: `internal/proxmox/backup_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/proxmox/backup_test.go`:

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

func TestListBackups(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/storage/pbs-main/content") {
			http.Error(w, "bad path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("content") != "backup" || r.URL.Query().Get("vmid") != "101" {
			http.Error(w, "bad query "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"data":[
			{"volid":"pbs-main:backup/ct/101/2026-06-11T02:14:03Z","format":"pbs-ct","ctime":1700000100,"size":1288490188,"protected":1,"verification":{"state":"ok"},"vmid":101},
			{"volid":"pbs-main:backup/ct/101/2026-06-10T02:14:01Z","format":"pbs-ct","ctime":1700000000,"size":1188490188,"verification":{"state":"ok"},"vmid":101}
		]}`))
	}))
	defer srv.Close()

	got, err := newTestClient(srv).ListBackups(context.Background(), "pve1", "pbs-main", 101)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	if got[0].VolID == "" || got[0].Protected != 1 || got[0].Verification.State != "ok" || got[0].CTime != 1700000100 {
		t.Errorf("decode: %+v", got[0])
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/proxmox/ -run TestListBackups -v`
Expected: compile failure — `ListBackups`/`BackupPoint` undefined.

- [ ] **Step 3: Implement**

Create `internal/proxmox/backup.go`:

```go
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
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/proxmox/ -run TestListBackups -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/proxmox/backup.go internal/proxmox/backup_test.go
git commit -m "feat(proxmox): add ListBackups via storage content API"
```

---

### Task 3: proxmox `RestoreBackup`

**Files:**
- Modify: `internal/proxmox/backup.go`
- Test: `internal/proxmox/backup_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/proxmox/backup_test.go` (add `"github.com/veenone/pvesnap/internal/config"` is already imported):

```go
func TestRestoreBackup(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case strings.HasSuffix(r.URL.Path, "/qemu") && r.Method == http.MethodPost:
			if r.PostForm.Get("archive") == "" || r.PostForm.Get("force") != "1" || r.PostForm.Get("vmid") != "201" {
				http.Error(w, "bad qemu params", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:0:0:0:qmrestore:201:u:"}`))
		case strings.HasSuffix(r.URL.Path, "/lxc") && r.Method == http.MethodPost:
			if r.PostForm.Get("ostemplate") == "" || r.PostForm.Get("restore") != "1" || r.PostForm.Get("force") != "1" {
				http.Error(w, "bad lxc params", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:0:0:0:vzrestore:101:u:"}`))
		default:
			http.Error(w, "bad "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	cl := newTestClient(srv)

	upid, err := cl.RestoreBackup(context.Background(), "pve1", config.QEMU, 201, "pbs-main:backup/vm/201/x")
	if err != nil || upid == "" {
		t.Fatalf("qemu restore: err=%v upid=%q", err, upid)
	}
	upid, err = cl.RestoreBackup(context.Background(), "pve1", config.LXC, 101, "pbs-main:backup/ct/101/x")
	if err != nil || upid == "" {
		t.Fatalf("lxc restore: err=%v upid=%q", err, upid)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/proxmox/ -run TestRestoreBackup -v`
Expected: compile failure — `RestoreBackup` undefined.

- [ ] **Step 3: Implement**

In `internal/proxmox/backup.go`, add imports `"net/url"`, `"strconv"`, and `"github.com/veenone/pvesnap/internal/config"` to the import block, then add:

```go
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
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/proxmox/ -run TestRestoreBackup -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/proxmox/backup.go internal/proxmox/backup_test.go
git commit -m "feat(proxmox): add RestoreBackup (in-place qemu/lxc restore)"
```

---

### Task 4: proxmox guest power control

**Files:**
- Create: `internal/proxmox/guest.go`
- Test: `internal/proxmox/guest_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/proxmox/guest_test.go`:

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

func TestGuestPowerControl(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status/current"):
			_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
		case strings.HasSuffix(r.URL.Path, "/status/stop") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:0:0:0:vzstop:101:u:"}`))
		case strings.HasSuffix(r.URL.Path, "/status/start") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:0:0:0:vzstart:101:u:"}`))
		default:
			http.Error(w, "bad "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	cl := newTestClient(srv)

	status, err := cl.GuestStatus(context.Background(), "pve1", config.LXC, 101)
	if err != nil || status != "running" {
		t.Fatalf("GuestStatus: status=%q err=%v", status, err)
	}
	if upid, err := cl.StopGuest(context.Background(), "pve1", config.LXC, 101); err != nil || upid == "" {
		t.Fatalf("StopGuest: upid=%q err=%v", upid, err)
	}
	if upid, err := cl.StartGuest(context.Background(), "pve1", config.LXC, 101); err != nil || upid == "" {
		t.Fatalf("StartGuest: upid=%q err=%v", upid, err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/proxmox/ -run TestGuestPowerControl -v`
Expected: compile failure — `GuestStatus`/`StopGuest`/`StartGuest` undefined.

- [ ] **Step 3: Implement**

Create `internal/proxmox/guest.go`:

```go
package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/veenone/pvesnap/internal/config"
)

type guestStatusResp struct {
	Status string `json:"status"`
}

// GuestStatus returns the run state ("running"/"stopped") of a guest.
func (c *Client) GuestStatus(ctx context.Context, node string, t config.GuestType, vmid int) (string, error) {
	var out guestStatusResp
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/status/current", node, t, vmid)
	if err := c.do(ctx, node, http.MethodGet, path, nil, &out); err != nil {
		return "", err
	}
	return out.Status, nil
}

// StopGuest stops a guest; returns the UPID of the async task.
func (c *Client) StopGuest(ctx context.Context, node string, t config.GuestType, vmid int) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/status/stop", node, t, vmid)
	return c.doString(ctx, node, http.MethodPost, path, url.Values{})
}

// StartGuest starts a guest; returns the UPID of the async task.
func (c *Client) StartGuest(ctx context.Context, node string, t config.GuestType, vmid int) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/status/start", node, t, vmid)
	return c.doString(ctx, node, http.MethodPost, path, url.Values{})
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/proxmox/ -run TestGuestPowerControl -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/proxmox/guest.go internal/proxmox/guest_test.go
git commit -m "feat(proxmox): add guest status/stop/start"
```

---

### Task 5: orchestrator `ListBackups` fan-out

**Files:**
- Modify: `internal/orchestrator/orchestrator.go`
- Test: `internal/orchestrator/orchestrator_backup_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/orchestrator/orchestrator_backup_test.go`:

```go
package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/veenone/pvesnap/internal/config"
)

func TestListBackups(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/content") && r.URL.Query().Get("vmid") == "101":
			_, _ = w.Write([]byte(`{"data":[{"volid":"pbs:backup/ct/101/x","ctime":100,"verification":{"state":"ok"}}]}`))
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	orch := testOrch(srv) // defined in orchestrator_test.go (Part A)
	guests := []config.Guest{
		{Node: "pve1", VMID: 101, Type: config.LXC},
		{Node: "pve1", VMID: 999, Type: config.QEMU}, // 500 -> Err
	}
	res := orch.ListBackups(context.Background(), "pbs", guests)
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	if res[0].Err != nil || len(res[0].Backups) != 1 {
		t.Errorf("guest 101: err=%v backups=%d", res[0].Err, len(res[0].Backups))
	}
	if res[1].Err == nil {
		t.Errorf("guest 999: expected error")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/orchestrator/ -run TestListBackups -v`
Expected: compile failure — `ListBackups`/`BackupListResult` undefined.

- [ ] **Step 3: Implement**

In `internal/orchestrator/orchestrator.go`, add after `SnapshotInventory`:

```go
// BackupListResult is the PBS backup points present for one guest.
type BackupListResult struct {
	Guest   config.Guest
	Backups []proxmox.BackupPoint
	Err     error
}

// ListBackups queries each guest's PBS backup points concurrently, gated by the
// per-node semaphore. Continues on per-guest error (captured in Err).
func (o *Orchestrator) ListBackups(ctx context.Context, storage string, guests []config.Guest) []BackupListResult {
	results := make([]BackupListResult, len(guests))
	var wg sync.WaitGroup
	for i, g := range guests {
		wg.Add(1)
		go func(i int, g config.Guest) {
			defer wg.Done()
			res := BackupListResult{Guest: g}
			if err := o.acquire(ctx, g.Node); err != nil {
				res.Err = err
				results[i] = res
				return
			}
			defer o.release(g.Node)
			b, err := o.Client.ListBackups(ctx, g.Node, storage, g.VMID)
			if err != nil {
				res.Err = fmt.Errorf("list backups: %w", err)
			} else {
				res.Backups = b
			}
			results[i] = res
		}(i, g)
	}
	wg.Wait()
	return results
}
```

(No new imports — `context`, `fmt`, `sync`, `config`, `proxmox` are already imported.)

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/orchestrator/ -run TestListBackups -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/orchestrator/orchestrator.go internal/orchestrator/orchestrator_backup_test.go
git commit -m "feat(orchestrator): add ListBackups read fan-out"
```

---

### Task 6: orchestrator `RestoreBackup` (stop → restore → start)

**Files:**
- Modify: `internal/orchestrator/orchestrator.go`
- Test: `internal/orchestrator/orchestrator_backup_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/orchestrator/orchestrator_backup_test.go` (add `"time"` to its import block):

```go
func TestRestoreBackup(t *testing.T) {
	var started bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status/current"):
			_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
		case strings.HasSuffix(r.URL.Path, "/status/stop"):
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:0:0:0:vzstop:101:u:"}`))
		case strings.HasSuffix(r.URL.Path, "/status/start"):
			started = true
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:0:0:0:vzstart:101:u:"}`))
		case strings.HasSuffix(r.URL.Path, "/lxc") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:0:0:0:vzrestore:101:u:"}`))
		case strings.Contains(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		default:
			http.Error(w, "bad "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Nodes:    []config.Node{{Name: "pve1", Endpoint: srv.URL, APIToken: "u@pam!t=x", VerifyTLS: false}},
		Defaults: config.Defaults{ParallelismPerNode: 2, TaskPollInterval: time.Millisecond, TaskTimeout: time.Minute},
	}
	orch := New(testClient(cfg), cfg) // see helper note below
	targets := []BackupTarget{{Guest: config.Guest{Node: "pve1", VMID: 101, Type: config.LXC}, VolID: "pbs:backup/ct/101/x"}}
	res := orch.RestoreBackup(context.Background(), targets, false) // noStart=false
	if len(res) != 1 || !res[0].Success {
		t.Fatalf("restore result: %+v", res)
	}
	if !started {
		t.Errorf("expected running guest to be restarted")
	}
}

func TestRestoreBackupPreservesStopped(t *testing.T) {
	var started bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status/current"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped"}}`))
		case strings.HasSuffix(r.URL.Path, "/status/start"):
			started = true
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:0:0:0:vzstart:101:u:"}`))
		case strings.HasSuffix(r.URL.Path, "/lxc") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:0:0:0:vzrestore:101:u:"}`))
		case strings.Contains(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		default:
			http.Error(w, "bad "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	cfg := &config.Config{
		Nodes:    []config.Node{{Name: "pve1", Endpoint: srv.URL, APIToken: "u@pam!t=x", VerifyTLS: false}},
		Defaults: config.Defaults{ParallelismPerNode: 2, TaskPollInterval: time.Millisecond, TaskTimeout: time.Minute},
	}
	orch := New(testClient(cfg), cfg)
	targets := []BackupTarget{{Guest: config.Guest{Node: "pve1", VMID: 101, Type: config.LXC}, VolID: "v"}}
	res := orch.RestoreBackup(context.Background(), targets, false) // noStart=false, but guest was stopped
	if len(res) != 1 || !res[0].Success {
		t.Fatalf("result: %+v", res)
	}
	if started {
		t.Errorf("a guest that was stopped before restore must not be started")
	}
}

func TestRestoreBackupFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status/current"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped"}}`))
		case strings.HasSuffix(r.URL.Path, "/lxc") && r.Method == http.MethodPost:
			http.Error(w, "restore boom", http.StatusInternalServerError)
		default:
			http.Error(w, "bad "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	cfg := &config.Config{
		Nodes:    []config.Node{{Name: "pve1", Endpoint: srv.URL, APIToken: "u@pam!t=x", VerifyTLS: false}},
		Defaults: config.Defaults{ParallelismPerNode: 2, TaskPollInterval: time.Millisecond, TaskTimeout: time.Minute},
	}
	orch := New(testClient(cfg), cfg)
	targets := []BackupTarget{{Guest: config.Guest{Node: "pve1", VMID: 101, Type: config.LXC}, VolID: "v"}}
	res := orch.RestoreBackup(context.Background(), targets, false)
	if len(res) != 1 || res[0].Success || res[0].Err == nil {
		t.Fatalf("expected failure result, got %+v", res)
	}
}
```

Note: `testOrch(srv)` (Part A) builds its own cfg with default poll interval; this test needs a fast `TaskPollInterval`, so it builds cfg directly. Add a tiny package-local helper at the top of `orchestrator_backup_test.go` to construct the client:
```go
func testClient(cfg *config.Config) *proxmox.Client { return proxmox.NewClient(cfg) }
```
and add `"github.com/veenone/pvesnap/internal/proxmox"` to the import block.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/orchestrator/ -run TestRestoreBackup -v`
Expected: compile failure — `BackupTarget`/`RestoreBackup` undefined.

- [ ] **Step 3: Implement**

In `internal/orchestrator/orchestrator.go`, add after `ListBackups`:

```go
// BackupTarget is one guest to restore from a specific backup volume.
type BackupTarget struct {
	Guest config.Guest
	VolID string
}

// RestoreBackup restores each target in-place from its backup volume, under
// errgroup cancel-on-first-error (a half-restored set is dangerous). Per guest:
// stop if running -> restore (force) -> wait -> restart only if it was running
// before and noStart is false (preserves prior power state). As with snapshot
// restore, an already-issued server-side task continues past a client-side
// cancel; this bounds, not eliminates, partial restores.
func (o *Orchestrator) RestoreBackup(ctx context.Context, targets []BackupTarget, noStart bool) []Result {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]Result, len(targets))
	g, gctx := errgroup.WithContext(ctx)
	for i, tgt := range targets {
		i, tgt := i, tgt
		results[i] = Result{Guest: tgt.Guest}
		g.Go(func() error {
			if err := o.acquire(gctx, tgt.Guest.Node); err != nil {
				results[i].Err = err
				return err
			}
			defer o.release(tgt.Guest.Node)
			node, gt, vmid := tgt.Guest.Node, tgt.Guest.Type, tgt.Guest.VMID

			status, err := o.Client.GuestStatus(gctx, node, gt, vmid)
			if err != nil {
				results[i].Err = fmt.Errorf("status: %w", err)
				return results[i].Err
			}
			wasRunning := status == "running"
			if wasRunning {
				upid, err := o.Client.StopGuest(gctx, node, gt, vmid)
				if err != nil {
					results[i].Err = fmt.Errorf("stop: %w", err)
					return results[i].Err
				}
				if err := o.Client.WaitTask(gctx, node, upid, o.Cfg.Defaults.TaskPollInterval); err != nil {
					results[i].Err = fmt.Errorf("stop wait: %w", err)
					return results[i].Err
				}
			}

			upid, err := o.Client.RestoreBackup(gctx, node, gt, vmid, tgt.VolID)
			if err != nil {
				results[i].Err = fmt.Errorf("restore: %w", err)
				return results[i].Err
			}
			if err := o.Client.WaitTask(gctx, node, upid, o.Cfg.Defaults.TaskPollInterval); err != nil {
				results[i].Err = fmt.Errorf("restore wait: %w", err)
				return results[i].Err
			}

			// Restore the guest's prior power state: restart only if it was
			// running before, unless the caller forced --no-start.
			if wasRunning && !noStart {
				upid, err := o.Client.StartGuest(gctx, node, gt, vmid)
				if err != nil {
					results[i].Err = fmt.Errorf("start: %w", err)
					return results[i].Err
				}
				if err := o.Client.WaitTask(gctx, node, upid, o.Cfg.Defaults.TaskPollInterval); err != nil {
					results[i].Err = fmt.Errorf("start wait: %w", err)
					return results[i].Err
				}
			}
			results[i].Success = true
			return nil
		})
	}
	_ = g.Wait()
	return results
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/orchestrator/ -run TestRestoreBackup -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/orchestrator/orchestrator.go internal/orchestrator/orchestrator_backup_test.go
git commit -m "feat(orchestrator): add RestoreBackup (stop->restore->start)"
```

---

### Task 7: CLI pure helpers — `parseAtTime`, `pickBackup`, `selectBackupTargets`, `humanizeBytes`

**Files:**
- Create: `internal/cli/cmd_backup.go`
- Test: `internal/cli/cmd_backup_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/cli/cmd_backup_test.go`:

```go
package cli

import (
	"testing"

	"github.com/veenone/pvesnap/internal/config"
	"github.com/veenone/pvesnap/internal/orchestrator"
	"github.com/veenone/pvesnap/internal/proxmox"
)

func TestPickBackup(t *testing.T) {
	bs := []proxmox.BackupPoint{
		{VolID: "a", CTime: 100},
		{VolID: "b", CTime: 300},
		{VolID: "c", CTime: 200},
	}
	// latest -> newest ctime
	if got, ok := pickBackup(bs, true, 0); !ok || got.VolID != "b" {
		t.Errorf("latest: %+v ok=%v", got, ok)
	}
	// at <= 250 -> newest at or before = "c" (200)
	if got, ok := pickBackup(bs, false, 250); !ok || got.VolID != "c" {
		t.Errorf("at-250: %+v ok=%v", got, ok)
	}
	// at <= 50 -> none
	if _, ok := pickBackup(bs, false, 50); ok {
		t.Errorf("at-50: expected none")
	}
}

func TestSelectBackupTargets(t *testing.T) {
	results := []orchestrator.BackupListResult{
		{Guest: config.Guest{VMID: 101}, Backups: []proxmox.BackupPoint{{VolID: "x", CTime: 100}}},
		{Guest: config.Guest{VMID: 102}, Backups: nil},                 // no backups -> skipped
		{Guest: config.Guest{VMID: 103}, Err: errAny()},                // errored -> skipped silently here
	}
	targets, skipped := selectBackupTargets(results, true, 0, nil)
	if len(targets) != 1 || targets[0].VolID != "x" {
		t.Fatalf("targets=%+v", targets)
	}
	if len(skipped) != 1 || skipped[0].VMID != 102 {
		t.Fatalf("skipped=%+v", skipped)
	}
}

func TestParseAtTime(t *testing.T) {
	if _, err := parseAtTime("2026-06-11"); err != nil {
		t.Errorf("date: %v", err)
	}
	if _, err := parseAtTime("2026-06-11T02:14:03Z"); err != nil {
		t.Errorf("rfc3339: %v", err)
	}
	if _, err := parseAtTime("nonsense"); err == nil {
		t.Errorf("expected error for bad time")
	}
}

func TestHumanizeBytes(t *testing.T) {
	cases := map[int64]string{512: "512 B", 1536: "1.5 KiB", 1288490188: "1.2 GiB"}
	for in, want := range cases {
		if got := humanizeBytes(in); got != want {
			t.Errorf("humanizeBytes(%d)=%q want %q", in, got, want)
		}
	}
}

func errAny() error { return &simpleErr{"boom"} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/cli/ -run 'TestPickBackup|TestSelectBackupTargets|TestParseAtTime|TestHumanizeBytes' -v`
Expected: compile failure — helpers undefined.

- [ ] **Step 3: Implement**

Create `internal/cli/cmd_backup.go` with just the helpers for now (the commands come in Tasks 8–9):

```go
package cli

import (
	"fmt"
	"time"

	"github.com/veenone/pvesnap/internal/orchestrator"
	"github.com/veenone/pvesnap/internal/proxmox"
	"github.com/veenone/pvesnap/internal/config"
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
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/cli/ -run 'TestPickBackup|TestSelectBackupTargets|TestParseAtTime|TestHumanizeBytes' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cmd_backup.go internal/cli/cmd_backup_test.go
git commit -m "feat(cli): add backup selection + format helpers"
```

---

### Task 8: CLI `backup list` + dispatch wiring

**Files:**
- Modify: `internal/cli/cmd_backup.go`
- Modify: `cmd/pvesnap/main.go`
- Test: `internal/cli/cmd_backup_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/cmd_backup_test.go` (add to its import block: `"bytes"`, `"context"`, `"net/http"`, `"net/http/httptest"`, `"strings"`, `"time"`):

```go
func TestRunBackupList(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/content") && r.URL.Query().Get("vmid") == "101":
			_, _ = w.Write([]byte(`{"data":[{"volid":"pbs-main:backup/ct/101/x","format":"pbs-ct","ctime":1700000100,"size":1288490188,"verification":{"state":"ok"}}]}`))
		default:
			http.Error(w, "boom", http.StatusInternalServerError) // 102 errors -> exit 1
		}
	}))
	defer srv.Close()
	cfg := &config.Config{
		Nodes:    []config.Node{{Name: "pve1", Endpoint: srv.URL, APIToken: "u@pam!t=x", VerifyTLS: false}},
		Sets:     []config.Set{{Name: "s", Guests: []config.Guest{{Node: "pve1", VMID: 101, Type: config.LXC}, {Node: "pve1", VMID: 102, Type: config.LXC}}}},
		Defaults: config.Defaults{ParallelismPerNode: 2, TaskPollInterval: time.Millisecond, TaskTimeout: time.Minute, PBSStorage: "pbs-main"},
	}
	var out bytes.Buffer
	code := RunBackup(context.Background(), cfg, &out, []string{"list", "s"})
	if code != 1 { // one guest errored
		t.Fatalf("want exit 1, got %d; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "1.2 GiB") || !strings.Contains(out.String(), "pbs-main:backup/ct/101/x") {
		t.Errorf("missing backup row: %s", out.String())
	}
}

func TestRunBackupListNoStorage(t *testing.T) {
	cfg := &config.Config{
		Nodes: []config.Node{{Name: "pve1", Endpoint: "https://x", APIToken: "t", VerifyTLS: false}},
		Sets:  []config.Set{{Name: "s", Guests: []config.Guest{{Node: "pve1", VMID: 101, Type: config.LXC}}}},
	}
	var out bytes.Buffer
	if code := RunBackup(context.Background(), cfg, &out, []string{"list", "s"}); code != 3 {
		t.Fatalf("want exit 3 (no pbs_storage), got %d; out=%s", code, out.String())
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/cli/ -run TestRunBackupList -v`
Expected: compile failure — `RunBackup`/`runBackupList` undefined.

- [ ] **Step 3: Implement `RunBackup` + `runBackupList`**

In `internal/cli/cmd_backup.go`, add `"context"`, `"flag"`, `"io"`, `"sort"`, `"text/tabwriter"` to the import block, then add:

```go
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
```

- [ ] **Step 4: Wire dispatch in `cmd/pvesnap/main.go`**

Add a `backup` case to the command switch in `run()`:
```go
	case "backup":
		return cli.RunBackup(ctx, cfg, os.Stdout, args[1:])
```
and add to the usage `Commands:` block, after the snapshot lines:
```
  backup list <set> [-vmid ...]         list PBS backup points for a set
  backup restore <set> ...              restore guests in-place from PBS backups
```

- [ ] **Step 5: Run to verify it passes**

Run: `go build ./... && go test ./internal/cli/ -run TestRunBackupList -v`
Expected: build clean; both list tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/cmd_backup.go cmd/pvesnap/main.go
git commit -m "feat(cli): add 'backup list' and wire backup dispatch"
```

---

### Task 9: CLI `backup restore`

**Files:**
- Modify: `internal/cli/cmd_backup.go`
- Test: `internal/cli/cmd_backup_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/cmd_backup_test.go`:

```go
func backupRestoreServer(t *testing.T) *httptest.Server {
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/content"):
			_, _ = w.Write([]byte(`{"data":[{"volid":"pbs:backup/ct/101/new","ctime":200},{"volid":"pbs:backup/ct/101/old","ctime":100}]}`))
		case strings.HasSuffix(r.URL.Path, "/status/current"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped"}}`))
		case strings.HasSuffix(r.URL.Path, "/lxc") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:0:0:0:vzrestore:101:u:"}`))
		case strings.HasSuffix(r.URL.Path, "/status/start"):
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:0:0:0:vzstart:101:u:"}`))
		case strings.Contains(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		default:
			http.Error(w, "bad "+r.URL.Path, http.StatusNotFound)
		}
	}))
}

func backupCfg(srv *httptest.Server) *config.Config {
	return &config.Config{
		Nodes:    []config.Node{{Name: "pve1", Endpoint: srv.URL, APIToken: "u@pam!t=x", VerifyTLS: false}},
		Sets:     []config.Set{{Name: "s", Guests: []config.Guest{{Node: "pve1", VMID: 101, Type: config.LXC}}}},
		Defaults: config.Defaults{ParallelismPerNode: 2, TaskPollInterval: time.Millisecond, TaskTimeout: time.Minute, PBSStorage: "pbs-main"},
	}
}

func TestRunBackupRestoreLatest(t *testing.T) {
	srv := backupRestoreServer(t)
	defer srv.Close()
	var out bytes.Buffer
	code := RunBackup(context.Background(), backupCfg(srv), &out, []string{"restore", "s", "--latest", "--yes"})
	if code != 0 {
		t.Fatalf("want exit 0, got %d; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "1 ok, 0 failed") {
		t.Errorf("unexpected: %s", out.String())
	}
}

func TestRunBackupRestorePrecise(t *testing.T) {
	srv := backupRestoreServer(t)
	defer srv.Close()
	var out bytes.Buffer
	code := RunBackup(context.Background(), backupCfg(srv), &out,
		[]string{"restore", "s", "-vmid", "101", "-volid", "pbs:backup/ct/101/old", "--yes"})
	if code != 0 {
		t.Fatalf("want exit 0, got %d; out=%s", code, out.String())
	}
}

func TestRunBackupRestoreSelectorValidation(t *testing.T) {
	srv := backupRestoreServer(t)
	defer srv.Close()
	var out bytes.Buffer
	// no selector
	if code := RunBackup(context.Background(), backupCfg(srv), &out, []string{"restore", "s", "--yes"}); code != 3 {
		t.Errorf("no selector: want 3, got %d", code)
	}
	out.Reset()
	// two selectors
	if code := RunBackup(context.Background(), backupCfg(srv), &out, []string{"restore", "s", "--latest", "--at", "2026-06-11", "--yes"}); code != 3 {
		t.Errorf("two selectors: want 3, got %d", code)
	}
	out.Reset()
	// -volid without single -vmid
	if code := RunBackup(context.Background(), backupCfg(srv), &out, []string{"restore", "s", "-volid", "x", "--yes"}); code != 3 {
		t.Errorf("volid without vmid: want 3, got %d", code)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/cli/ -run TestRunBackupRestore -v`
Expected: compile failure — `runBackupRestore` undefined.

- [ ] **Step 3: Implement `runBackupRestore`**

In `internal/cli/cmd_backup.go`, add `"bufio"`, `"os"`, `"strings"` to the import block, then add:

```go
func runBackupRestore(ctx context.Context, cfg *config.Config, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("backup restore", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip confirmation")
	noStart := fs.Bool("no-start", false, "leave guests stopped after restore (default: restart)")
	vmidFlag := fs.String("vmid", "", "comma-separated VMIDs to target")
	volid := fs.String("volid", "", "exact backup volid (requires a single -vmid)")
	latest := fs.Bool("latest", false, "restore each guest from its newest backup")
	atStr := fs.String("at", "", "restore each guest from its newest backup at or before this time (RFC3339 or YYYY-MM-DD)")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	pos := fs.Args()
	if len(pos) != 1 {
		fmt.Fprintln(out, "usage: pvesnap backup restore <set> (-vmid N -volid V | --latest | --at T) [-vmid ...] [--no-start] [--yes]")
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

	// Exactly one selector.
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
```

(`renderResults`, `exitForCounts`, `parseVMIDFilter` come from `cmd_snapshot.go` in the same package — Part A.)

- [ ] **Step 4: Run to verify it passes**

Run: `go build ./... && go test ./internal/cli/ -run TestRunBackupRestore -v`
Expected: build clean; all restore tests PASS.

- [ ] **Step 5: Full sweep**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/cmd_backup.go internal/cli/cmd_backup_test.go
git commit -m "feat(cli): add 'backup restore' (precise + --latest/--at, in-place)"
```

---

### Task 10: Docs + example config

**Files:**
- Modify: `examples/config.yaml`, `docs/commands.md`, `docs/operations.md`, `docs/proxmox-api.md`, `docs/roadmap.md`

- [ ] **Step 1: `examples/config.yaml` — add `pbs_storage`**

Under the `defaults:` block, add:
```yaml
  pbs_storage: pbs-main      # PBS datastore registered on the nodes (for `backup` commands)
```

- [ ] **Step 2: `docs/commands.md` — document the backup commands**

Add a new section after the snapshot commands:

````markdown
## `pvesnap backup list <set> [-vmid 100,101]`

Lists PBS backup points for each guest in the set, newest first. Read-only; queries the
node storage content API for `defaults.pbs_storage` (or the set's `pbs_storage` override).

```
$ pvesnap backup list e2e-core
NODE  TYPE  VMID  WHEN              SIZE     VERIFIED  PROT  VOLID
pve1  lxc   101   2026-06-11 02:14  1.2 GiB  ok        no    pbs-main:backup/ct/101/2026-06-11T02:14:03Z
```

A per-guest query failure prints a line and yields exit code 1.

## `pvesnap backup restore <set> ...`

Restores guests **in-place** (stop → restore over the same VMID with `force=1` → restart)
from PBS backups. Destructive; prompts unless `--yes`. PBS backups are per-guest, so:

- **Precise:** `backup restore <set> -vmid 101 -volid <volid>` — one guest from one backup.
- **Set-wide:** `backup restore <set> --latest` or `--at 2026-06-11` — each targeted guest's
  newest backup (or newest at/before the time). Exactly one of `-volid`, `--latest`, `--at`.

| Flag | Purpose |
|---|---|
| `--yes` | Skip confirmation. |
| `--no-start` | Leave guests stopped after restore (default: restart). |
| `-vmid <id,...>` | Restrict to these VMIDs. Required (single) with `-volid`. |
| `-volid <volid>` | Exact backup volume (precise mode). |
| `--latest` / `--at <T>` | Set-wide selectors. `T` is RFC3339 or `YYYY-MM-DD`. |
````

- [ ] **Step 3: `docs/operations.md` — PBS prerequisites + safety**

Add a section:
```markdown
## PBS backups (`backup` commands)

`backup list`/`backup restore` require a PBS datastore registered as a PVE storage; set
its storage id in `defaults.pbs_storage` (or per set). pvesnap reaches it through the node
API with the existing tokens — no separate PBS credentials. Backups are created and pruned
by PBS, not pvesnap.

`backup restore` is **in-place and destructive**: it stops the guest and overwrites its
disks. Like `snapshot restore`, set-wide restore uses cancel-on-first-error; an
already-issued server-side restore continues past a client cancel, so a half-restored set
is bounded, not impossible.
```

- [ ] **Step 4: `docs/proxmox-api.md` — add endpoints**

Add to the endpoints table:
```markdown
| `GET` | `/api2/json/nodes/{node}/storage/{storage}/content?content=backup&vmid={vmid}` | List PBS backup volumes for a guest. |
| `POST` | `/api2/json/nodes/{node}/qemu` (`archive`, `force=1`) | Restore a VM in-place from a backup. Returns a UPID. |
| `POST` | `/api2/json/nodes/{node}/lxc` (`ostemplate`, `restore=1`, `force=1`) | Restore a container in-place. Returns a UPID. |
| `GET` | `/api2/json/nodes/{node}/{type}/{vmid}/status/current` | Guest run state. |
| `POST` | `/api2/json/nodes/{node}/{type}/{vmid}/status/{stop,start}` | Stop/start a guest. Returns a UPID. |
```

- [ ] **Step 5: `docs/roadmap.md` — mark item #8 consume-half landed**

In the priority-summary table, change item #8's Status to `Consume (list/restore) landed`, and append to the item #8 prose:
```markdown
> **Update (2026-06-17):** The consume half shipped as integrated `backup list`/`backup
> restore` (PBS via the node storage API, in-place restore). Backup *creation* (vzdump) and
> retention remain out of scope — PBS owns them.
```

- [ ] **Step 6: Commit**

```bash
git add examples/config.yaml docs/commands.md docs/operations.md docs/proxmox-api.md docs/roadmap.md
git commit -m "docs: document PBS backup list/restore"
```

---

## Self-Review

**1. Spec coverage (Part B):**
- PBS access via node storage API → Tasks 2 (list) + 3 (restore). ✅
- `pbs_storage` config + per-set override + required-for-backup → Task 1 + Tasks 8/9 (empty → exit 3). ✅
- `backup list` with metadata (when/size/verified/protected/volid), newest-first, partial→exit 1 → Task 8. ✅
- `backup restore` precise (`-vmid`+`-volid`) and set-wide (`--latest`/`--at`), exactly-one-selector → Task 9. ✅
- In-place stop→restore→start, `--no-start`, cancel-on-first-error → Task 6 + Task 9. ✅
- Live-queried, never persisted to state → no state writes anywhere in backup code. ✅
- Reuse `renderResults`/`exitForCounts` → Task 9. ✅
- Docs/examples/endpoints/roadmap → Task 10. ✅
- Out of scope (create/vzdump, retention, delete, restore-to-new-vmid, direct PBS API) → correctly absent. ✅

**2. Placeholder scan:** No TBD/TODO; every code step shows complete code; every run step shows command + expected result.

**3. Type consistency:** `BackupPoint{VolID,Format,Notes,CTime,Size,Protected,Verification{State},VMID}` defined Task 2, consumed Tasks 5/7/8. `BackupListResult{Guest,Backups,Err}` Task 5 → Tasks 7/8/9. `BackupTarget{Guest,VolID}` Task 6 → Tasks 7/9. `RestoreBackup(ctx, targets, restart)` Task 6 → Task 9 (`!*noStart`). `ListBackups(ctx, storage, guests)` Task 5 → Tasks 8/9. `pickBackup(backups, latest, atUnix)`, `selectBackupTargets(results, latest, atUnix, filter)`, `parseAtTime`, `humanizeBytes` defined Task 7, used Tasks 8/9. `RunBackup`/`runBackupList`/`runBackupRestore` consistent across Tasks 8/9 and `main.go` dispatch.

**Note on commits:** commit commands intentionally omit any `Co-Authored-By` trailer, per project preference.

**Known real-world refinement (flag at execution, not a plan defect):** `RestoreBackup` sends the minimal documented params (`archive`/`ostemplate` + `force=1`). Some storage layouts require an explicit target `storage=` on restore; if live testing hits "no storage specified", add a `storage` param sourced from config. Out of scope for the httptest-backed plan (review D3: defer).

## Review addendum (2026-06-17, /plan-eng-review)

- **D1 (accepted):** restore preserves prior power state — `RestoreBackup(ctx, targets, noStart)` restarts a guest only if it was running before; a pre-stopped guest stays stopped. Tasks 6 (logic + `TestRestoreBackupPreservesStopped`) and 9 (`*noStart`).
- **D2 (accepted):** `runBackupRestore` prints the resolved targets (node/type/vmid ← volid) before the confirmation prompt, so an in-place overwrite is never confirmed blind. Task 9.
- **D3 (defer):** no explicit restore `storage=` param now; the failure mode is a loud task error, add reactively if a live PBS needs it.
- **Test gaps closed:** Task 6 adds `TestRestoreBackupPreservesStopped` (stopped guest not restarted) and `TestRestoreBackupFailure` (restore error → failed result).

## GSTACK REVIEW REPORT

| Review | Trigger | Why | Runs | Status | Findings |
|--------|---------|-----|------|--------|----------|
| CEO Review | `/plan-ceo-review` | Scope & strategy | 0 | — | not run |
| Codex Review | `/codex review` | Independent 2nd opinion | 0 | — | classifier outage; skipped |
| Eng Review | `/plan-eng-review` | Architecture & tests (required) | 1 | CLEAR | 3 decisions (D1/D2 accepted, D3 defer); 2 test gaps closed |
| Design Review | `/plan-design-review` | UI/UX gaps | 0 | — | n/a (CLI) |
| DX Review | `/plan-devex-review` | Developer experience gaps | 0 | — | not run |

- **Note:** the Skill-tool classifier was temporarily unavailable, so this review was conducted directly (read-only analysis + AskUserQuestion), not via the Skill wrapper. No `gstack-review-log` entry was written.
- **Scope:** accepted as-is (new `backup` namespace; file count justified, no overbuild).
- **Architecture:** 3 decisions (D1 restart policy, D2 restore preview, D3 restore storage). **Code Quality:** no blocking issues. **Tests:** 2 gaps closed. **Performance:** 0 issues.
- **Failure modes:** restore error path now tested; cancel-on-first-error server-side caveat documented (inherited from snapshot restore).
- **UNRESOLVED:** none.
- **VERDICT:** ENG CLEARED — ready to implement (base on Part A branch).
```
