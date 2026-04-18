# Installation & Configuration

## Building from source

Requires Go 1.22 or newer.

```bash
git clone git@github.com:veenone/pvesnap.git
cd pvesnap
go mod tidy
go build -o pvesnap ./cmd/pvesnap
sudo install -m 0755 pvesnap /usr/local/bin/
```

Stripped build for smaller binaries:

```bash
go build -ldflags="-s -w" -trimpath -o pvesnap ./cmd/pvesnap
```

## Creating a scoped API token

Do this on any node of the cluster (the token propagates cluster-wide). For a homelab, `--privsep=0` is simplest:

```bash
pveum user token add root@pam pvesnap --privsep=0
```

Copy the returned **full-id** (`root@pam!pvesnap`) and **value** (UUID). The value is shown only once.

For production, prefer a dedicated user with a scoped role:

```bash
pveum role add pvesnap-role -privs "VM.Snapshot VM.Snapshot.Rollback VM.Audit Datastore.Audit Sys.Audit"
pveum user add pvesnap@pve
pveum aclmod / -user pvesnap@pve -role pvesnap-role
pveum user token add pvesnap@pve pvesnap --privsep=1
pveum aclmod / -tokens 'pvesnap@pve!pvesnap' -role pvesnap-role
```

The token string you record in `config.yaml` is `USER@REALM!TOKENID=UUID`, for example `root@pam!pvesnap=00000000-0000-0000-0000-000000000000`.

## `config.yaml`

Default location: `$XDG_CONFIG_HOME/pvesnap/config.yaml` (usually `~/.config/pvesnap/config.yaml`). Override with `-config <path>` or `PVESNAP_CONFIG=<path>`.

```yaml
nodes:
  - name: pve1
    endpoint: https://10.0.0.11:8006
    api_token: "root@pam!pvesnap=00000000-0000-0000-0000-000000000000"
    verify_tls: false        # homelab default; set true where you trust the CA

  - name: pve2
    endpoint: https://10.0.0.12:8006
    api_token: "root@pam!pvesnap=00000000-0000-0000-0000-000000000000"
    verify_tls: false

sets:
  - name: e2e-core
    description: "Core E2E stack — api, db, broker, worker"
    guests:
      - { node: pve1, vmid: 101, type: lxc,  role: api }
      - { node: pve1, vmid: 102, type: lxc,  role: db }
      - { node: pve2, vmid: 201, type: qemu, role: broker }
      - { node: pve2, vmid: 202, type: lxc,  role: worker }

defaults:
  parallelism_per_node: 2    # concurrent ops per node; 2–4 is a safe range
  task_poll_interval: 2s     # UPID status poll cadence
  task_timeout: 30m          # per-operation bound for an entire fan-out
```

### Field reference

**nodes[]** — one entry per Proxmox node you want to talk to.
- `name` — short identifier used in `set.guests[].node` and CLI output.
- `endpoint` — full URL including `https://` and port `8006`.
- `api_token` — the full `USER@REALM!TOKENID=UUID` string.
- `verify_tls` — false accepts self-signed certs; strongly prefer true with a proper CA.

**sets[]** — one entry per named environment.
- `name` — identifier used on the CLI (`snapshot create <name>`).
- `description` — free-form.
- `guests[]` — every guest that must be captured/restored together.
  - `node` must match a `nodes[].name`.
  - `vmid` is the numeric Proxmox ID.
  - `type` is `lxc` or `qemu`.
  - `role` is optional metadata.

**defaults** — all fields have sensible built-in defaults (2 / 2s / 30m); override only if your environment needs it.

## `state.yaml`

Default: `$XDG_CONFIG_HOME/pvesnap/state.yaml`. Override with `-state <path>` or `PVESNAP_STATE=<path>`. The tool creates it on first snapshot and writes atomically. You can safely version it in git if you want snapshots of your snapshot registry.

## File permissions

`config.yaml` contains API tokens that grant write access to your Proxmox cluster. Lock it down:

```bash
chmod 0600 ~/.config/pvesnap/config.yaml
```

`state.yaml` is written with mode `0600` automatically.
