# Proxmox API Reference

## Auth

All requests use the `PVEAPIToken` header:

```
Authorization: PVEAPIToken=USER@REALM!TOKENID=UUID
```

There is no login or CSRF-token dance — tokens are stateless. Requests that fail auth return HTTP 401 with a JSON body.

## Endpoints used

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api2/json/cluster/resources?type=vm` | Enumerate all guests known to the cluster. Returns LXC and QEMU in a single response. |
| `GET` | `/api2/json/nodes/{node}/qemu/{vmid}/snapshot` | List snapshots on a QEMU VM. |
| `GET` | `/api2/json/nodes/{node}/lxc/{vmid}/snapshot` | List snapshots on an LXC container. |
| `POST` | `/api2/json/nodes/{node}/qemu/{vmid}/snapshot` | Create a QEMU snapshot. Body: `snapname`, `description` (optional), `vmstate` (optional, 1 to include RAM). |
| `POST` | `/api2/json/nodes/{node}/lxc/{vmid}/snapshot` | Create an LXC snapshot. Body: `snapname`, `description` (optional). |
| `POST` | `/api2/json/nodes/{node}/qemu/{vmid}/snapshot/{snap}/rollback` | Roll back a QEMU VM to a snapshot. |
| `POST` | `/api2/json/nodes/{node}/lxc/{vmid}/snapshot/{snap}/rollback` | Roll back an LXC to a snapshot. |
| `DELETE` | `/api2/json/nodes/{node}/qemu/{vmid}/snapshot/{snap}` | Delete a QEMU snapshot. |
| `DELETE` | `/api2/json/nodes/{node}/lxc/{vmid}/snapshot/{snap}` | Delete an LXC snapshot. |
| `GET` | `/api2/json/nodes/{node}/tasks/{upid}/status` | Poll the status of an async task. |
| `GET` | `/api2/json/nodes/{node}/storage/{storage}/content?content=backup&vmid={vmid}` | List PBS backup volumes for a guest. |
| `POST` | `/api2/json/nodes/{node}/qemu` (`archive`, `force=1`) | Restore a VM in-place from a backup. Returns a UPID. |
| `POST` | `/api2/json/nodes/{node}/lxc` (`ostemplate`, `restore=1`, `force=1`) | Restore a container in-place. Returns a UPID. |
| `GET` | `/api2/json/nodes/{node}/{type}/{vmid}/status/current` | Guest run state. |
| `POST` | `/api2/json/nodes/{node}/{type}/{vmid}/status/{stop,start}` | Stop/start a guest. Returns a UPID. |

All `POST`/`DELETE` endpoints above return a **UPID** immediately — not the result of the operation. That string identifies an async task you must poll separately.

## Response shape

Success:

```json
{ "data": <value> }
```

The `data` field is either:

- An object or array (e.g. `/cluster/resources` returns an array of guests).
- A bare string (e.g. snapshot create returns the UPID).

Error:

```json
{
  "data": null,
  "errors": { "snapname": "invalid format" },
  "message": "Parameter verification failed."
}
```

`pvesnap`'s client returns an error if either `errors` is non-empty or the HTTP status is ≥ 400.

## UPID polling protocol

A UPID looks like:

```
UPID:pve1:00001A2B:000F4240:655A1234:qmsnapshot:101:root@pam!pvesnap:
```

Fields: `node:pid:pstart:starttime:type:id:user:`. You don't need to parse it — just pass it back whole.

Poll loop:

```
GET /api2/json/nodes/{node}/tasks/{upid}/status
→ { "data": {
      "upid": "...",
      "node": "pve1",
      "type": "qmsnapshot",
      "status": "running" | "stopped",
      "exitstatus": "OK" | "..."      // only present when stopped
  }}
```

Rules the tool follows:

- Sleep `defaults.task_poll_interval` between polls (default 2s).
- Treat any response where `status != "stopped"` as "still running".
- When `status == "stopped"`, the operation is done. Success is `exitstatus == "OK"`; anything else is an error string from Proxmox (e.g. *"TASK ERROR: storage 'local-lvm' does not support snapshots"*) that the tool surfaces verbatim.
- If the parent op context hits `task_timeout` (default 30m), polling is cancelled with `context.DeadlineExceeded`.

Task logs are available at `/api2/json/nodes/{node}/tasks/{upid}/log` but the tool doesn't fetch them today — the `exitstatus` string is usually enough.

## Snapshot name constraints

Enforced by Proxmox, validated client-side by the tool:

```
^[A-Za-z][A-Za-z0-9_-]{1,39}$
```

- 2–40 characters.
- First must be a letter.
- Remaining may be letters, digits, hyphen, underscore.
- Dots are **not** allowed (this rejects version strings like `v1.5`).

`config.NormalizeSnapName` in the tool maps any invalid character to `-` and prefixes a leading letter if needed.

## Relevant Proxmox docs (external)

- [API viewer](https://pve.proxmox.com/pve-docs/api-viewer/) — canonical reference.
- `man pveum` — user/token/ACL management.
- `man pct` and `man qm` — CLI equivalents of the container/VM API endpoints, useful for understanding what's happening server-side.
