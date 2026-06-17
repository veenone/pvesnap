# Restore Sources: Live Snapshots & PBS Backups — Design

**Status:** Draft for review
**Date:** 2026-06-12
**Author:** brainstorming session (pvesnap)
**Supersedes:** `2026-06-12-pbs-backup-list-restore-design.md`

## Summary

Give pvesnap two complementary ways to enumerate and restore guests to an earlier point,
so the workflow is the same whether or not PBS is deployed:

- **Part A — Live native snapshots (no PBS).** Make `snapshot restore` source the snapshot
  list **live** from each guest's own storage (`/nodes/.../snapshot`), reconciling with
  `state.yaml` rather than depending on it. Add `snapshot list --live` to enumerate what
  actually exists on each guest. This is the default, no-prerequisite path and works even
  when `state.yaml` is missing, incomplete, or snapshots were made out-of-band.
- **Part B — PBS backups (centralized).** Add `backup list` and `backup restore` that
  consume PBS backup points via the node storage API. In-place restore. PBS owns creation
  and retention.

Together these land the "consume/restore" halves of roadmap #1 (reconcile) and #8 (PBS),
integrated into the existing binary.

## Goals

1. **Restore from live per-guest snapshots without PBS** — `snapshot restore <set> <name>`
   works off snapshots present on each guest's storage, for both LXC and QEMU.
2. **See what restore points exist** — `snapshot list --live` (on-guest snapshots) and
   `backup list` (PBS backup points).
3. **Restore from a specific PBS backup point** — single-guest precise restore by volid.
4. **Centralized backup storage** — satisfied by PBS (a datastore registered as a PVE
   storage); no new storage layer built.

## Non-goals

- Creating/scheduling snapshots beyond the existing `snapshot create`; creating PBS backups
  (`vzdump`) — PBS / its scheduler does this.
- Retention/pruning of snapshots or backups (PBS owns backup lifecycle; snapshot retention
  is roadmap #4).
- Deleting PBS backups from pvesnap.
- PBS restore-to-a-new-VMID — in-place only.
- A separate `pvesnap-backup` binary; a direct PBS API credential path.

## Restore sources at a glance

| | Live native snapshots (Part A) | PBS backups (Part B) |
|---|---|---|
| Storage | Each guest's own storage (ZFS/LVM-thin/…) | PBS datastore (central, node-shared) |
| Needs PBS? | **No** | Yes |
| Identified by | Shared **name** across the set | Per-guest **volid** + timestamp (no shared name) |
| Source of truth | Live `/snapshot` query (state advisory) | Live storage content query |
| Restore mechanism | `rollback` (in-place, as today) | `qmrestore`/`pct restore` with `force=1` (in-place, stop→restore→start) |
| Command | `snapshot list --live`, `snapshot restore` (enhanced) | `backup list`, `backup restore` (new) |

---

## Part A — Live-sourced native snapshot restore (non-PBS)

### Behavior change: `snapshot restore <set> <name>`

Today this reads `state.yaml`, finds the recorded snapshot, and restores only guests whose
recorded `status == ok`. New behavior makes **the guest's storage the source of truth**:

1. Resolve the **set from config** (set membership no longer comes from state).
2. **Live discovery fan-out:** for each guest in the set (honoring `-vmid`), call the
   already-implemented `proxmox.ListSnapshots(node, type, vmid)` and check whether a
   snapshot named `<name>` exists.
3. Build restore targets = guests that actually have the snapshot on storage.
4. **Reconcile with state (advisory, non-fatal):**
   - Guest has it live but not in state → include it; note "not in state (drift)".
   - State says `ok` but it's gone live → skip with "not found on guest"; don't error the run.
   - `state.yaml` may be entirely absent — restore still works.
5. Confirm (unless `--yes`), then `orchestrator.Restore(targets)` — unchanged in-place
   rollback with `errgroup` cancel-on-first-error.
6. Restore does not mutate state (unchanged).

**Empty target set** (name present on zero guests) → message "snapshot `<name>` not found on
any guest in set `<set>`" and exit 2.

This also resolves review finding #2: a guest wrongly flipped to `failed` in state by a
re-run of `snapshot create` is still restorable, because restore checks live storage, not
the state status. It delivers the restore-side of roadmap #1 (reconcile) without a separate
`sync` command.

### New: `snapshot list --live [<set>]`

Default `snapshot list` (state-based) is unchanged. With `--live`, query each guest in the
set(s) and aggregate on-guest snapshots by name, showing set coverage:

```
$ pvesnap snapshot list e2e-core --live
NAME      COVERAGE  GUESTS  NEWEST            PARENTED
v1-5-rc1  full      4/4     2026-06-11 09:15  yes
hotfix    partial   2/4     2026-06-11 14:02  no
```

- `COVERAGE` = full when every guest in the set has a snapshot of that name, else partial.
- Helps the operator pick a `<name>` that exists across the set before restoring.
- Reuses `SnapshotEntry` (incl. `parent`); full tree rendering stays roadmap #2.

### Orchestrator additions

- `DiscoverSnapshots(ctx, guests []config.Guest) []SnapshotInventory` — read-only fan-out
  over the per-node semaphore; each result carries the guest and its `[]SnapshotEntry` (or
  a query error). Continue-on-error.
- A small CLI-side helper turns the inventory + a target name into `[]state.GuestRecord`
  for the existing `Restore`. No change to `Restore` itself.

### Files (Part A)

| File | Change |
|---|---|
| `internal/cli/cmd_snapshot.go` | Rework `runSnapshotRestore` to live-source targets; add `--live` branch to `runSnapshotList` |
| `internal/orchestrator/orchestrator.go` | Add `DiscoverSnapshots` read fan-out (+ `SnapshotInventory`) |
| `internal/proxmox/snapshot.go` | No new endpoint — wire in the existing unused `ListSnapshots` |
| `docs/commands.md` | Document live restore + `snapshot list --live` |
| `docs/operations.md` | Note restore is now state-drift tolerant |
| `docs/roadmap.md` | Mark #1 restore-side as landed |

---

## Part B — PBS backup list & restore (centralized)

### Access path

PBS datastore registered as a PVE storage → reachable through the node API with existing
tokens. List: `GET /nodes/{node}/storage/{storage}/content?content=backup&vmid={vmid}`.
Restore: `POST /nodes/{node}/qemu` (`archive=<volid>`) or `/lxc` (`ostemplate=<volid>`,
`restore=1`) with `force=1` → UPID. A direct PBS API path is left as a future seam
(`BackupLister` interface) but not built.

### Config

- `Defaults.PBSStorage` (`yaml:"pbs_storage"`) — PBS storage id; optional per-set override
  `Set.PBSStorage`. `backup` commands require a resolved non-empty value (else exit 3).
  Load-time validation stays lenient so snapshot-only users are unaffected.

```yaml
defaults:
  parallelism_per_node: 2
  task_poll_interval: 2s
  task_timeout: 30m
  pbs_storage: pbs-main
```

### Backups vs snapshots

PBS backups are **live-queried, never persisted to `state.yaml`** (PBS is the source of
truth) and have **no shared name** across guests — they're per-guest timestamped points.
Precise restore therefore targets one guest + one volid; set-wide restore needs a
time selector.

### `pvesnap backup list <set> [-vmid 100,101]`

```
NODE  TYPE  VMID  WHEN              SIZE     VERIFIED  PROT  VOLID
pve1  lxc   101   2026-06-11 02:14  1.2 GiB  ok        no    pbs-main:backup/ct/101/2026-06-11T02:14:03Z
pve2  qemu  201   2026-06-11 02:15  8.4 GiB  ok        no    pbs-main:backup/vm/201/2026-06-11T02:15:22Z
```

Newest-first; per-guest query failure → that row shows an error, run exits 1 (partial).

### `pvesnap backup restore <set> ...`

- **Precise:** `-vmid <id> -volid <volid> [--yes] [--no-start]` — one guest, one point.
- **Set-wide:** `[--latest | --at <RFC3339|date>] [-vmid ...] [--yes] [--no-start]` —
  resolve each targeted guest's point (newest, or nearest at-or-before `--at`) and restore
  together. Exactly one of `{-volid, --latest, --at}` required; `--latest`/`--at` mutually
  exclusive.

**In-place flow per guest:** status check → if running, `StopGuest` + `WaitTask` →
`RestoreBackup` (`force=1`) + `WaitTask` → if not `--no-start`, `StartGuest` + `WaitTask`.
Destructive → confirmation prompt unless `--yes`.

### Orchestrator / client (Part B)

- `proxmox/backup.go` — `BackupPoint` (`volid, ctime, size, format, notes, verification.state,
  protected`), `ListBackups`, `RestoreBackup` (→ UPID).
- `proxmox/guest.go` — `GuestStatus`, `StopGuest`, `StartGuest` (→ UPID).
- `orchestrator` — `ListBackups` (continue-on-error read fan-out) and `RestoreBackup`
  (cancel-on-first-error, runs the stop→restore→start sequence per guest).
- `cli/cmd_backup.go` — `RunBackup` dispatch (`list`/`restore`); reuses `parseVMIDFilter`,
  `filterByVMID`, `errString`, `tabwriter`.
- `cmd/pvesnap/main.go` — dispatch `backup`; usage text.

### Files (Part B)

`internal/config/config.go`, `internal/proxmox/backup.go` (new),
`internal/proxmox/guest.go` (new), `internal/orchestrator/orchestrator.go`,
`internal/cli/cmd_backup.go` (new), `cmd/pvesnap/main.go`, `examples/config.yaml`,
`docs/{commands,operations,proxmox-api,roadmap}.md`, tests.

---

## Shared concerns

### Failure semantics & exit codes

Unchanged contract: `0` success, `1` partial, `2` full failure, `3` usage/config error.
Live `snapshot restore` and `backup restore` both report cancelled-by-first-error guests
distinctly from genuine failures (an improvement over today's restore output). The existing
"already-issued server-side task continues past cancel" caveat is documented in
`operations.md` for both.

### Testing

`httptest.NewTLSServer` faking the node API:

- `proxmox` — `ListSnapshots` decode incl. `parent`; backup content decode (verification,
  protected); restore/stop/start UPID; status; error decoding.
- `config` — `pbs_storage` default vs per-set override; missing-storage error.
- `cli` — live restore target selection (present/absent/drift); empty-target exit 2;
  `--live` coverage aggregation; backup flag validation (mutually exclusive selectors);
  `--at` boundary picks; vmid filtering.
- `orchestrator` — `DiscoverSnapshots` / `ListBackups` continue-on-error; `RestoreBackup`
  cancel-on-first-error and stop→restore→start ordering.

## Sequencing

1. **Part A first** — no external prerequisites (uses existing snapshot/rollback endpoints
   and the already-built `ListSnapshots`); immediately useful and de-risks state drift.
2. **Part B second** — needs a PBS datastore registered on the nodes to exercise end-to-end.

Each part is an independently shippable plan; they can be split into two implementation
plans if preferred.

## Open interpretations to confirm

1. **"the profile" = the existing `set`** (backup/snapshot commands key off sets with
   `-vmid` filtering) — *not* the roadmap-#9 multi-cluster profile notion.
2. **PBS backup points are per-guest** → precise restore is `-vmid` + `-volid`; set-wide
   needs `--latest`/`--at`. Native snapshots keep the shared-name model.
3. **Restart-after-restore (PBS path) defaults ON** (`--no-start` to opt out). Native
   snapshot rollback keeps current behavior (no explicit stop/start; Proxmox handles it).
4. **"manage" available backups = rich listing** (time/size/verified/protected/volid) +
   restore; deleting/pruning backups is intentionally out of scope (PBS owns it). Flag if
   you intended delete.
```
