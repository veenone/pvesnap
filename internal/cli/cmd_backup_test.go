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
