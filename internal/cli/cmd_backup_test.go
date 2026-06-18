package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	if got, ok := pickBackup(bs, true, 0); !ok || got.VolID != "b" {
		t.Errorf("latest: %+v ok=%v", got, ok)
	}
	if got, ok := pickBackup(bs, false, 250); !ok || got.VolID != "c" {
		t.Errorf("at-250: %+v ok=%v", got, ok)
	}
	if _, ok := pickBackup(bs, false, 50); ok {
		t.Errorf("at-50: expected none")
	}
}

func TestSelectBackupTargets(t *testing.T) {
	results := []orchestrator.BackupListResult{
		{Guest: config.Guest{VMID: 101}, Backups: []proxmox.BackupPoint{{VolID: "x", CTime: 100}}},
		{Guest: config.Guest{VMID: 102}, Backups: nil},
		{Guest: config.Guest{VMID: 103}, Err: errAny()},
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

func TestRunBackupList(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/content") && r.URL.Query().Get("vmid") == "101":
			_, _ = w.Write([]byte(`{"data":[{"volid":"pbs-main:backup/ct/101/x","format":"pbs-ct","ctime":1700000100,"size":1288490188,"verification":{"state":"ok"}}]}`))
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
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
	if code != 1 {
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

func TestRunBackupListVmidFilter(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("vmid") == "101" {
			_, _ = w.Write([]byte(`{"data":[{"volid":"pbs-main:backup/ct/101/x","format":"pbs-ct","ctime":1700000100,"size":1024,"verification":{"state":"ok"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	cfg := &config.Config{
		Nodes:    []config.Node{{Name: "pve1", Endpoint: srv.URL, APIToken: "u@pam!t=x", VerifyTLS: false}},
		Sets:     []config.Set{{Name: "s", Guests: []config.Guest{{Node: "pve1", VMID: 101, Type: config.LXC}, {Node: "pve1", VMID: 102, Type: config.LXC}}}},
		Defaults: config.Defaults{ParallelismPerNode: 2, TaskPollInterval: time.Millisecond, TaskTimeout: time.Minute, PBSStorage: "pbs-main"},
	}
	var out bytes.Buffer
	// -vmid AFTER the set name must parse (regression guard for flag ordering).
	code := RunBackup(context.Background(), cfg, &out, []string{"list", "s", "-vmid", "101"})
	if code != 0 {
		t.Fatalf("want exit 0 (filtered to 101), got %d; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "pbs-main:backup/ct/101/x") {
		t.Errorf("expected 101's backup row: %s", out.String())
	}
}

func backupRestoreServer() *httptest.Server {
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
	srv := backupRestoreServer()
	defer srv.Close()
	var out bytes.Buffer
	code := RunBackup(context.Background(), backupCfg(srv), &out, []string{"restore", "s", "--latest", "--yes"})
	if code != 0 {
		t.Fatalf("want exit 0, got %d; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "1 ok, 0 failed") {
		t.Errorf("unexpected: %s", out.String())
	}
	if !strings.Contains(out.String(), "will restore") || !strings.Contains(out.String(), "pbs:backup/ct/101/new") {
		t.Errorf("expected resolved-target preview with newest volid: %s", out.String())
	}
}

func TestRunBackupRestorePrecise(t *testing.T) {
	srv := backupRestoreServer()
	defer srv.Close()
	var out bytes.Buffer
	code := RunBackup(context.Background(), backupCfg(srv), &out,
		[]string{"restore", "s", "-vmid", "101", "-volid", "pbs:backup/ct/101/old", "--yes"})
	if code != 0 {
		t.Fatalf("want exit 0, got %d; out=%s", code, out.String())
	}
}

func TestRunBackupRestoreSelectorValidation(t *testing.T) {
	srv := backupRestoreServer()
	defer srv.Close()
	var out bytes.Buffer
	if code := RunBackup(context.Background(), backupCfg(srv), &out, []string{"restore", "s", "--yes"}); code != 3 {
		t.Errorf("no selector: want 3, got %d", code)
	}
	out.Reset()
	if code := RunBackup(context.Background(), backupCfg(srv), &out, []string{"restore", "s", "--latest", "--at", "2026-06-11", "--yes"}); code != 3 {
		t.Errorf("two selectors: want 3, got %d", code)
	}
	out.Reset()
	if code := RunBackup(context.Background(), backupCfg(srv), &out, []string{"restore", "s", "-volid", "x", "--yes"}); code != 3 {
		t.Errorf("volid without vmid: want 3, got %d", code)
	}
}
