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
