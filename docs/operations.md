# Operational Guide

## LXC storage — the most common gotcha

LXC snapshots only work on storage backends whose LXC drivers support them. If you see a snapshot create fail on an LXC with a message like *"storage does not support snapshots"*, this is why.

| Storage type | LXC snapshot support |
|---|---|
| ZFS (local or over iSCSI) | Yes |
| Btrfs | Yes |
| LVM-thin | Yes |
| Directory + qcow2 rootfs | Yes |
| Directory + raw/subvol | **No** |
| Plain LVM (not thin) | **No** |
| NFS + raw/subvol | **No** |

QEMU VMs have fewer restrictions because qcow2 provides snapshots on almost any backend, but **raw** VM disks on non-qcow2-capable storage still fail.

Use `pvesnap discover` to list your guests, then check their storage in the Proxmox UI or via `pvesm status` before adding them to a set.

## Partial-failure recovery

`snapshot create` leaves `state.yaml` with per-guest `status: ok|failed`. If you see partial failure:

1. Inspect the `error` field in `state.yaml` for the guest.
2. Fix the root cause (storage full, guest locked by another task, guest paused, etc.).
3. Re-run `pvesnap snapshot create <set> <name>` with the **same name** — the tool will re-attempt all guests. Guests that already have that snapshot in Proxmox will fail with *"snapshot already exists"*, which is a benign error.

For a cleaner reset after partial failure:

```bash
pvesnap snapshot delete <set> <name> --yes   # removes what exists
pvesnap snapshot create <set> <name>         # fresh attempt
```

## Stale state reconciliation

`state.yaml` can drift from Proxmox if someone deletes a snapshot from the UI directly. There's no `pvesnap sync` today (see [Roadmap](roadmap.md#1-snapshot-sync--reconcile)); if this happens:

- `snapshot restore` will report "snapshot not found" for the out-of-band-deleted guest.
- `snapshot delete` will report the same; the state entry stays because delete didn't fully succeed.
- To fully clean up, edit `state.yaml` by hand — it's a human-readable file, and you can safely remove the offending `snapshots[]` entry.

As of the live-restore change, `snapshot restore` and `snapshot list --live` query guest
storage directly, so they tolerate a missing or drifted `state.yaml` — restore targets the
snapshots that actually exist on each guest. You no longer have to hand-edit `state.yaml`
to recover from out-of-band snapshot deletion before restoring.

## Scheduling

Since `pvesnap` is a one-shot CLI, wrap it in systemd or cron.

### systemd example — nightly snapshot at 02:00

`/etc/systemd/system/pvesnap-nightly.service`:

```ini
[Unit]
Description=pvesnap nightly capture
After=network-online.target

[Service]
Type=oneshot
User=pvesnap
Environment=PVESNAP_CONFIG=/etc/pvesnap/config.yaml
Environment=PVESNAP_STATE=/var/lib/pvesnap/state.yaml
ExecStart=/usr/local/bin/pvesnap snapshot create e2e-core nightly-%Y%m%d --description "automated nightly"
SuccessExitStatus=0
```

`/etc/systemd/system/pvesnap-nightly.timer`:

```ini
[Unit]
Description=Trigger pvesnap-nightly.service at 02:00 daily

[Timer]
OnCalendar=*-*-* 02:00:00
Persistent=true

[Install]
WantedBy=timers.target
```

Enable with `systemctl enable --now pvesnap-nightly.timer`.

Exit code 1 (partial failure) is treated as success by `SuccessExitStatus=0`; change to `SuccessExitStatus=0 1` if you want systemd to tolerate partials. Exit code 2 or 3 will mark the unit as failed — which is what you want for alerting.

## Performance expectations

On a typical homelab node (SSD + ZFS), per-guest snapshot creation takes:

- LXC: 1–3 seconds (almost instantaneous — ZFS CoW).
- QEMU without `--include-ram`: 1–5 seconds.
- QEMU with `--include-ram`: 10–60 seconds per gigabyte of assigned RAM.

With `parallelism_per_node: 2`, a set of 20 guests spread across 4 nodes (5 each) captures in ~10 seconds when nothing is pathological. The 30-minute `task_timeout` in the default config is loosely sized for the RAM-snapshot worst case.

## Security notes

The API token in `config.yaml` is a secret. A few practical rules:

- `chmod 0600 ~/.config/pvesnap/config.yaml` (or tighter).
- Prefer a dedicated `pvesnap@pve` user with a role limited to `VM.Snapshot`, `VM.Snapshot.Rollback`, `VM.Audit`, `Datastore.Audit`, `Sys.Audit` — avoid `root@pam` in automation.
- Rotate the token by running `pveum user token remove <user> pvesnap` + `pveum user token add <user> pvesnap` and updating `config.yaml`. The old token is revoked immediately.
- If you version `config.yaml` in git, template the `api_token` field and inject it at deploy time. Don't commit raw tokens.

## Monitoring

Because `pvesnap` is a CLI, observability is whatever you wrap it in:

- systemd journal: `journalctl -u pvesnap-nightly.service`.
- Cron: pipe stdout/stderr to a log and alert on non-zero exit.
- CI: the exit code distinguishes usage errors (3), full failure (2), partial failure (1), success (0).

There's no built-in Prometheus endpoint today; the binary exits when the operation completes, so a pull-based metrics model wouldn't fit anyway. If you need metrics, have the wrapper script post exit code + duration to a pushgateway.
