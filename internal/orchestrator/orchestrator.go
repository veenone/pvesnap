package orchestrator

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/veenone/pvesnap/internal/config"
	"github.com/veenone/pvesnap/internal/proxmox"
	"github.com/veenone/pvesnap/internal/state"
	"golang.org/x/sync/errgroup"
)

type Orchestrator struct {
	Client   *proxmox.Client
	Cfg      *config.Config
	Sems     map[string]chan struct{} // per-node concurrency gate
}

func New(client *proxmox.Client, cfg *config.Config) *Orchestrator {
	sems := make(map[string]chan struct{}, len(cfg.Nodes))
	n := cfg.Defaults.ParallelismPerNode
	if n <= 0 {
		n = 2
	}
	for _, node := range cfg.Nodes {
		sems[node.Name] = make(chan struct{}, n)
	}
	return &Orchestrator{Client: client, Cfg: cfg, Sems: sems}
}

// acquire/release the per-node semaphore; ctx is honored during wait.
func (o *Orchestrator) acquire(ctx context.Context, node string) error {
	sem := o.Sems[node]
	if sem == nil {
		return nil
	}
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *Orchestrator) release(node string) {
	if sem := o.Sems[node]; sem != nil {
		<-sem
	}
}

type Result struct {
	Guest   config.Guest
	Success bool
	Err     error
}

// SnapshotInventory is the set of snapshots present on one guest's storage.
type SnapshotInventory struct {
	Guest     config.Guest
	Snapshots []proxmox.SnapshotEntry
	Err       error
}

// DiscoverSnapshots queries each guest's live snapshot list concurrently,
// gated by the per-node semaphore. A per-guest query error is captured in that
// entry's Err; the fan-out always returns exactly one entry per input guest.
func (o *Orchestrator) DiscoverSnapshots(ctx context.Context, guests []config.Guest) []SnapshotInventory {
	results := make([]SnapshotInventory, len(guests))
	var wg sync.WaitGroup
	for i, g := range guests {
		wg.Add(1)
		go func(i int, g config.Guest) {
			defer wg.Done()
			inv := SnapshotInventory{Guest: g}
			if err := o.acquire(ctx, g.Node); err != nil {
				inv.Err = err
				results[i] = inv
				return
			}
			defer o.release(g.Node)
			snaps, err := o.Client.ListSnapshots(ctx, g.Node, g.Type, g.VMID)
			if err != nil {
				inv.Err = fmt.Errorf("list snapshots: %w", err)
				results[i] = inv
				return
			}
			inv.Snapshots = snaps
			results[i] = inv
		}(i, g)
	}
	wg.Wait()
	return results
}

// Create fans out snapshot creation across the set. It runs every guest even
// if some fail — partial snapshots are recoverable. Returns per-guest results.
func (o *Orchestrator) Create(ctx context.Context, set config.Set, snapname, description string, vmstate bool) []Result {
	results := make([]Result, len(set.Guests))
	var wg sync.WaitGroup
	for i, g := range set.Guests {
		wg.Add(1)
		go func(i int, g config.Guest) {
			defer wg.Done()
			r := Result{Guest: g}
			if err := o.acquire(ctx, g.Node); err != nil {
				r.Err = err
				results[i] = r
				return
			}
			defer o.release(g.Node)
			upid, err := o.Client.CreateSnapshot(ctx, g.Node, g.Type, g.VMID, snapname, description, vmstate)
			if err != nil {
				r.Err = fmt.Errorf("create: %w", err)
				results[i] = r
				return
			}
			if err := o.Client.WaitTask(ctx, g.Node, upid, o.Cfg.Defaults.TaskPollInterval); err != nil {
				r.Err = fmt.Errorf("wait: %w", err)
				results[i] = r
				return
			}
			r.Success = true
			results[i] = r
		}(i, g)
	}
	wg.Wait()
	return results
}

// Restore fans out rollback with first-error cancellation. A half-rolled-back
// set is worse than stopping early, so any failure cancels remaining work.
func (o *Orchestrator) Restore(ctx context.Context, records []state.GuestRecord) []Result {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]Result, len(records))
	g, gctx := errgroup.WithContext(ctx)
	for i, rec := range records {
		i, rec := i, rec
		guest := config.Guest{Node: rec.Node, VMID: rec.VMID, Type: rec.Type}
		results[i] = Result{Guest: guest}
		g.Go(func() error {
			if err := o.acquire(gctx, rec.Node); err != nil {
				results[i].Err = err
				return err
			}
			defer o.release(rec.Node)
			upid, err := o.Client.Rollback(gctx, rec.Node, rec.Type, rec.VMID, rec.Snapname)
			if err != nil {
				results[i].Err = fmt.Errorf("rollback: %w", err)
				return results[i].Err
			}
			if err := o.Client.WaitTask(gctx, rec.Node, upid, o.Cfg.Defaults.TaskPollInterval); err != nil {
				results[i].Err = fmt.Errorf("wait: %w", err)
				return results[i].Err
			}
			results[i].Success = true
			return nil
		})
	}
	_ = g.Wait()
	return results
}

// Delete fans out snapshot deletion. Unlike restore, delete keeps going on
// individual failures so the operator can see which guests still have the
// snapshot and clean up manually.
func (o *Orchestrator) Delete(ctx context.Context, records []state.GuestRecord) []Result {
	results := make([]Result, len(records))
	var wg sync.WaitGroup
	for i, rec := range records {
		wg.Add(1)
		go func(i int, rec state.GuestRecord) {
			defer wg.Done()
			guest := config.Guest{Node: rec.Node, VMID: rec.VMID, Type: rec.Type}
			r := Result{Guest: guest}
			if err := o.acquire(ctx, rec.Node); err != nil {
				r.Err = err
				results[i] = r
				return
			}
			defer o.release(rec.Node)
			upid, err := o.Client.DeleteSnapshot(ctx, rec.Node, rec.Type, rec.VMID, rec.Snapname)
			if err != nil {
				r.Err = fmt.Errorf("delete: %w", err)
				results[i] = r
				return
			}
			if err := o.Client.WaitTask(ctx, rec.Node, upid, o.Cfg.Defaults.TaskPollInterval); err != nil {
				r.Err = fmt.Errorf("wait: %w", err)
				results[i] = r
				return
			}
			r.Success = true
			results[i] = r
		}(i, rec)
	}
	wg.Wait()
	return results
}

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

// OpContext returns a context bounded by the configured task timeout.
func (o *Orchestrator) OpContext(parent context.Context) (context.Context, context.CancelFunc) {
	d := o.Cfg.Defaults.TaskTimeout
	if d <= 0 {
		d = 30 * time.Minute
	}
	return context.WithTimeout(parent, d)
}
