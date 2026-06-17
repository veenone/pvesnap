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
