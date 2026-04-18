# Architecture

## Topology

```
┌────────────────┐   HTTPS + PVEAPIToken   ┌─────────────────┐
│  pvesnap CLI   │────────────────────────▶│ Proxmox node A  │
│  (Go binary)   │────────────────────────▶│ Proxmox node B  │
│                │────────────────────────▶│ Proxmox node C  │
└──────┬─────────┘                          └─────────────────┘
       │ reads/writes
       ▼
 $XDG_CONFIG_HOME/pvesnap/
   config.yaml   (nodes, tokens, sets)
   state.yaml    (saved setups → per-guest snapshot records)
```

The binary opens one HTTPS session per configured node. Discovery can hit any single node's `/cluster/resources` (all clustered nodes return the full view); create/rollback/delete calls target the specific node that owns each guest.

## Project layout

```
pvesnap/
├── go.mod
├── cmd/pvesnap/main.go           # entrypoint: global flags + subcommand dispatch
├── internal/
│   ├── config/config.go          # YAML load, validate, snapshot-name normalizer
│   ├── state/state.go            # YAML load + atomic save (temp file + rename)
│   ├── proxmox/
│   │   ├── client.go             # http.Client wrapper, PVEAPIToken auth, TLS
│   │   ├── resources.go          # GET /cluster/resources — guest discovery
│   │   ├── snapshot.go           # create / list / rollback / delete endpoints
│   │   └── task.go               # UPID polling (WaitTask)
│   ├── orchestrator/
│   │   └── orchestrator.go       # fan-out across a set, per-node semaphore
│   └── cli/
│       ├── cmd_discover.go
│       ├── cmd_set.go
│       └── cmd_snapshot.go
└── examples/
    └── config.yaml
```

## Dependency budget

Three modules, all pure-Go:

- `gopkg.in/yaml.v3` — config and state parsing.
- `golang.org/x/sync/errgroup` — fan-out with first-error cancellation for restore.
- stdlib: `net/http`, `crypto/tls`, `encoding/json`, `flag`, `context`, `os/signal`, `text/tabwriter`.

No Cobra, no Viper, no ORM, no SQLite. Subcommand dispatch is a short switch in `main.go` using stdlib `flag.FlagSet` per command.

## Concurrency model

Every write operation in Proxmox returns a **UPID** (Unique Process IDentifier) immediately and runs asynchronously. `pvesnap` fans out the initial POST/DELETE for every guest in a set, then polls `/nodes/<node>/tasks/<upid>/status` in each goroutine until the task is `stopped` with `exitstatus=OK`.

A **per-node semaphore** (`chan struct{}` sized by `defaults.parallelism_per_node`, default 2) caps how many concurrent API calls hit a single node. Proxmox serializes per-guest operations anyway, so higher concurrency gains little and risks overwhelming the node.

### Create vs restore — deliberate asymmetry

| Operation | Strategy | Why |
|---|---|---|
| `snapshot create` | `sync.WaitGroup`, run every guest even if some fail | A partial snapshot is recoverable — you can retry failed guests, or delete the partial set and try again. Aborting halfway leaves ambiguous state. |
| `snapshot restore` | `errgroup` with cancel-on-first-error | A half-rolled-back environment (some guests at v1.5, some at v1.4) is actively dangerous — tests can pass or fail for wrong reasons. Better to stop and surface the error. |
| `snapshot delete` | `sync.WaitGroup`, run every guest even if some fail | Same logic as create — you want visibility into which guests still hold the snapshot. |

That asymmetry is the most important thing to understand about `orchestrator.go`.

## State semantics

`state.yaml` is the source of truth for "which named snapshots exist and which guests they span". Proxmox itself only knows about individual per-guest snapshots labeled with the same name — there's no cluster-wide grouping concept on the server side. If `state.yaml` is lost, the snapshots still exist in Proxmox; you just lose the grouping metadata and would have to reconstruct it from snapshot-name conventions.

Writes are atomic (temp file + `rename(2)`), so an interrupted save can't corrupt the file.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | All operations succeeded. |
| 1 | Partial failure — some guests succeeded, some failed. Check per-guest output. |
| 2 | Full failure — no guest succeeded. |
| 3 | Usage or config error — nothing was attempted. |

These matter if you drive `pvesnap` from a CI job or systemd timer.
