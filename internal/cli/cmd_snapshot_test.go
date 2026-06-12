package cli

import (
	"errors"
	"testing"

	"github.com/veenone/pvesnap/internal/config"
	"github.com/veenone/pvesnap/internal/orchestrator"
	"github.com/veenone/pvesnap/internal/proxmox"
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
