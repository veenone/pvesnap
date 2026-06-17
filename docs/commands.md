# Command Reference

## Global flags

| Flag | Default | Meaning |
|---|---|---|
| `-config <path>` | `$XDG_CONFIG_HOME/pvesnap/config.yaml` | Path to config file. Env var: `PVESNAP_CONFIG`. |
| `-state <path>` | `$XDG_CONFIG_HOME/pvesnap/state.yaml` | Path to state file. Env var: `PVESNAP_STATE`. |

Global flags must appear **before** the subcommand, e.g. `pvesnap -config ./local.yaml snapshot list`.

## `pvesnap discover`

Lists every non-template guest (LXC + QEMU) known to each configured node. Useful when populating `sets[]` in config.

```
$ pvesnap discover
QUERIED  NODE  TYPE  VMID  NAME        STATUS
pve1     pve1  lxc   101   api         running
pve1     pve1  lxc   102   db          running
pve1     pve2  qemu  201   broker      running
pve1     pve2  lxc   202   worker      stopped
```

- `QUERIED` is the node whose API returned the entry.
- `NODE` is the node that actually owns the guest (matters in a cluster).
- Templates are filtered out; duplicates (clustered endpoints seeing the same guest) are deduped.

### Flags

| Flag | Purpose |
|---|---|
| `-node <name>` | Only query this one configured node. |

## `pvesnap set list`

Prints every set defined in config with its guest roster. Does not call Proxmox.

```
$ pvesnap set list
SET       GUESTS  DESCRIPTION
e2e-core  4       Core E2E stack — api, db, broker, worker

e2e-core:
  NODE  TYPE  VMID  ROLE
  pve1  lxc   101   api
  pve1  lxc   102   db
  pve2  qemu  201   broker
  pve2  lxc   202   worker
```

## `pvesnap snapshot create <set> <name>`

Fans out snapshot creation across every guest in `<set>`. Writes one entry to `state.yaml` when done, with per-guest status. The asynchronous Proxmox task for each guest is polled to completion before the guest is marked `ok`.

```
$ pvesnap snapshot create e2e-core v1-5-rc1 --description "v1.5 RC1"
creating snapshot "v1-5-rc1" on set "e2e-core" (4 guests)...
NODE  TYPE  VMID  STATUS  DETAIL
pve1  lxc   101   ok
pve1  lxc   102   ok
pve2  qemu  201   ok
pve2  lxc   202   ok
done: 4 ok, 0 failed
```

### Flags

| Flag | Purpose |
|---|---|
| `-description <text>` | Human-readable description attached to every Proxmox snapshot. |
| `-include-ram` | Set `vmstate=1` on QEMU snapshots (captures memory). Slower and takes more storage. Off by default. Ignored for LXC. |

### Name normalization

Proxmox constrains snapshot names to `[A-Za-z][A-Za-z0-9_-]{1,39}`. `pvesnap` normalizes your input: `v1.5-rc1` becomes `v1-5-rc1`, `2024-q1` becomes `s2024-q1`. It prints the normalized name and records the mapping in state.

## `pvesnap snapshot list [<set>]`

Prints every recorded snapshot. Optional positional `<set>` filters to one set. Reads `state.yaml` only — no API calls.

```
$ pvesnap snapshot list
SET       NAME      CREATED           GUESTS  FAILED  DESCRIPTION
e2e-core  v1-4      2026-04-12 10:21  4       0       v1.4 GA
e2e-core  v1-5-rc1  2026-04-18 09:15  4       0       v1.5 RC1
```

The `FAILED` column is the count of guests whose state entry is not `ok` — i.e. guests where this snapshot does **not** exist or is incomplete.

### Live listing with `--live`

`pvesnap snapshot list <set> --live` queries each guest in the set directly and
aggregates the snapshots that actually exist on their storage (independent of
`state.yaml`). Useful before a restore, and for spotting drift.

```
$ pvesnap snapshot list e2e-core --live
NAME      COVERAGE  GUESTS  NEWEST            PARENTED
v1-5-rc1  full      4/4     2026-06-11 09:15  yes
hotfix    partial   2/4     2026-06-11 14:02  no
```

- `COVERAGE` is `full` when every guest in the set carries that snapshot name, else `partial`.
- A per-guest query failure prints a warning line and yields exit code 1.

## `pvesnap snapshot restore <set> <name>`

Rolls every guest in the set back to the recorded snapshot. Interactive confirmation unless `--yes` is passed. Uses `errgroup` with cancel-on-first-error: if any one guest's rollback fails, remaining rollbacks are cancelled to avoid a half-restored environment.

```
$ pvesnap snapshot restore e2e-core v1-5-rc1 --yes
rolling back 4 guests to "v1-5-rc1"...
NODE  TYPE  VMID  STATUS  DETAIL
pve1  lxc   101   ok
pve1  lxc   102   ok
pve2  qemu  201   ok
pve2  lxc   202   ok
done: 4 ok, 0 failed
```

Restore is **live-sourced**: the set is read from config, and each guest is queried for
the named snapshot directly on its storage. Only guests that actually hold the snapshot
are rolled back. This works even when `state.yaml` is missing or out of date; the snapshot
name is normalized the same way as on create. If state records a guest as holding the
snapshot but it is absent on the guest, a drift note is printed and that guest is skipped.
If the name is found on no guest, the command prints a message and exits 2.

### Selective restore with `--vmid`

Use `--vmid` to restore only specific guests instead of the entire set:

```
# Restore a single guest
$ pvesnap snapshot restore e2e-core v1-5-rc1 --yes --vmid 101
rolling back 1 guests to "v1-5-rc1"...
NODE  TYPE  VMID  STATUS  DETAIL
pve1  lxc   101   ok
done: 1 ok, 0 failed

# Restore multiple specific guests
$ pvesnap snapshot restore e2e-core v1-5-rc1 --yes --vmid 101,201
rolling back 2 guests to "v1-5-rc1"...
NODE  TYPE  VMID  STATUS  DETAIL
pve1  lxc   101   ok
pve2  qemu  201   ok
done: 2 ok, 0 failed
```

## `pvesnap snapshot delete <set> <name>`

Removes the snapshot from every guest. Like create, it continues even if individual guests fail, so you can see exactly what still exists. State entry is removed only on full success; on partial failure the entry remains so you can investigate.

When `--vmid` is used, the state entry is **not** removed even on success, since the snapshot still exists on other guests.

```
$ pvesnap snapshot delete e2e-core v1-4 --yes
NODE  TYPE  VMID  STATUS  DETAIL
pve1  lxc   101   ok
pve1  lxc   102   ok
pve2  qemu  201   ok
pve2  lxc   202   ok
done: 4 ok, 0 failed
```

### Flags (restore and delete)

| Flag | Purpose |
|---|---|
| `--yes` | Skip the interactive confirmation prompt. |
| `--vmid <id,...>` | Comma-separated list of VMIDs to target. When omitted, all guests in the set are affected. |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | All operations succeeded. |
| 1 | Partial failure — inspect per-guest output. |
| 2 | Full failure — no guest succeeded. |
| 3 | Usage or config error — nothing was attempted. |
