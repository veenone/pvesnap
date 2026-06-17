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
