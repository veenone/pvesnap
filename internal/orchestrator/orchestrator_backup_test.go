package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/veenone/pvesnap/internal/config"
	"github.com/veenone/pvesnap/internal/proxmox"
)

func testClient(cfg *config.Config) *proxmox.Client { return proxmox.NewClient(cfg) }

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

	orch := testOrch(srv)
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
	orch := New(testClient(cfg), cfg)
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
	res := orch.RestoreBackup(context.Background(), targets, false) // guest was stopped
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
