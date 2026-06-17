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
