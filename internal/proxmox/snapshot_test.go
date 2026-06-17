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
