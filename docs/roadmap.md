# Roadmap

This document plans the work that was explicitly listed as **out of scope** for the first cut, plus a few follow-ups that emerged during implementation. Items are ordered by my recommended priority (highest first), with effort sizing and the concrete changes each one requires.

## Priority summary

| # | Item | Effort | Value | Status |
|---|---|---|---|---|
| 1 | Snapshot sync / reconcile | S | High | Restore-side landed (live restore) |
| 2 | Snapshot tree visualization | S | Medium | Planned |
| 3 | Dry-run + JSON output | S | Medium | Planned |
| 4 | Retention (`--retain`, `prune`) | M | High | Planned |
| 5 | GitHub Actions CI + releases | S | High (hygiene) | Planned |
| 6 | Unit tests for `proxmox` and `orchestrator` | M | High (hygiene) | Planned |
| 7 | TUI mode | M | Medium | Deferred |
| 8 | vzdump / PBS long-term archive | L | Situational | Deferred |
| 9 | Multi-cluster tenancy (profiles) | S | Low | Docs-only |
| 10 | Snapshot chain inspection | S | Low | Covered by #2 |

Effort: S = ≤ 1 day, M = 2–5 days, L = > 1 week. "Deferred" means useful but not blocking; don't let them drag the core snapshot path.

---

## 1. Snapshot sync / reconcile

**Problem.** `state.yaml` drifts from Proxmox when someone deletes a snapshot via the UI, or when a `snapshot create` partially fails and the operator cleans up by hand. Today the only way to repair state is to hand-edit the file.

**Proposal.** New subcommand `pvesnap snapshot sync [<set>]` that, for every recorded snapshot (or just the ones in `<set>`):

1. Calls `GET /nodes/{node}/{type}/{vmid}/snapshot` for each guest in that entry (this endpoint exists and `proxmox.Client.ListSnapshots` is already wired).
2. Compares the snapshot name to what `state.yaml` records.
3. Updates per-guest `status` to `ok` if found, `missing` if not (new enum value), keeps the state entry either way.
4. Optionally `--prune` flag: removes state entries where no guest still holds the snapshot.

**Files to touch:**
- `internal/state/state.go` — add `StatusMissing` constant.
- `internal/cli/cmd_snapshot.go` — new `runSnapshotSync` function; wire into the `snapshot` switch.
- `internal/orchestrator/orchestrator.go` — add `Sync(ctx, records) []Result` using the existing semaphore.

**Effort:** ~200 LoC, half a day. Low risk because it only reads from Proxmox.

> **Update (2026-06-12):** The restore path no longer depends on `state.yaml` — `snapshot
> restore` and `snapshot list --live` source snapshots live from each guest. A dedicated
> `snapshot sync` command (to rewrite state `status` fields) is still unbuilt, but the
> drift-recovery motivation is largely addressed for restore.

## 2. Snapshot tree visualization

**Problem.** Proxmox snapshots form a parent/child tree — you can branch from an earlier snapshot, and the relationships matter for understanding what rollback does. Today `pvesnap snapshot list` only shows state-recorded snapshots, not the Proxmox-side graph.

**Proposal.** New subcommand `pvesnap snapshot tree <set>` that:

1. For each guest in the set, calls `ListSnapshots` (already implemented).
2. Builds a parent/child graph using the `parent` field that `SnapshotEntry` already decodes.
3. Renders per-guest ASCII trees side by side:

```
e2e-core snapshot tree:

pve1/101 (api)                  pve2/201 (broker)
  current                         current
  ├─ v1-4                         ├─ v1-4
  │  └─ v1-5-rc1                  │  └─ v1-5-rc1
  └─ experimental                 └─ experimental
```

**Files to touch:**
- `internal/proxmox/snapshot.go` — already returns `parent`; no changes needed.
- `internal/cli/cmd_snapshot.go` — new `runSnapshotTree` + small tree-render helper.

**Effort:** ~150 LoC, a day including tidy output. Pairs naturally with #1.

## 3. Dry-run and JSON output

**Problem.** CI pipelines want machine-readable output, and operators want to see what a command *would* do before running it.

**Proposal.** Two global flags:

- `--dry-run` — resolves the set, validates the snapshot name, shows which guests would be targeted, and stops before any write API call. Read-only calls (`ListSnapshots` for a pre-flight check) are OK.
- `--json` — changes every command's output to a single JSON document written to stdout. Keeps human-friendly output as the default.

Example JSON for `snapshot create`:

```json
{
  "command": "snapshot create",
  "set": "e2e-core",
  "name": "v1-5-rc1",
  "results": [
    { "node": "pve1", "type": "lxc", "vmid": 101, "status": "ok" },
    { "node": "pve1", "type": "lxc", "vmid": 102, "status": "failed", "error": "…" }
  ],
  "summary": { "ok": 1, "failed": 1, "total": 2 },
  "exit_code": 1
}
```

**Files to touch:**
- `cmd/pvesnap/main.go` — parse the two global flags, thread through to subcommands.
- `internal/cli/*.go` — each command branches on the flag for output.
- `internal/orchestrator/orchestrator.go` — `Create` / `Restore` / `Delete` already return structured results; small adapter.

**Effort:** ~300 LoC, a day. Must cover every existing subcommand to be useful.

## 4. Retention (`--retain`, `prune`)

**Problem.** Automated nightly/weekly captures need retention or they fill storage. Today the operator has to track and delete manually.

**Proposal.** Two additions:

- `snapshot create --retain N` — after a successful create, delete the oldest entries in that set until at most N remain. "Oldest" means by `created_at` in `state.yaml`. Failed creates do not trigger pruning.
- `pvesnap snapshot prune <set> [--keep N | --older-than DUR | --matching PATTERN]` — explicit pruning with three mutually exclusive criteria.

The retention delete uses the existing `Delete` orchestrator path, so it handles partial failures the same way.

**Files to touch:**
- `internal/cli/cmd_snapshot.go` — new `runSnapshotPrune`; add `--retain` flag to `runSnapshotCreate`.
- `internal/state/state.go` — helper to return snapshots for a set sorted by age.

**Effort:** ~250 LoC, 1–2 days. Needs careful handling of the "keep" set to avoid deleting the one you just created.

## 5. GitHub Actions CI + releases

**Problem.** The repo is public on GitHub but has no CI, no pre-built binaries, and no signed releases.

**Proposal.** Two workflows:

- `.github/workflows/ci.yml` — runs `go vet`, `go test ./...`, `go build` on every push and PR, matrix over `ubuntu-latest` and `macos-latest`, `go-version` 1.22 and 1.24.
- `.github/workflows/release.yml` — triggered by pushing a `v*` tag, uses [goreleaser](https://goreleaser.com/) to build Linux/macOS binaries (amd64 + arm64), generate checksums, attach them to a GitHub Release.

**Files to touch:**
- `.github/workflows/ci.yml`
- `.github/workflows/release.yml`
- `.goreleaser.yaml` — single-binary project, so config is ~30 lines.

**Effort:** Half a day if tests exist; #6 should come first or at the same time.

## 6. Unit tests

**Problem.** Zero tests today. The happy path is thin (HTTP + YAML) but error paths in `proxmox.WaitTask` and `orchestrator.Restore` are worth covering before they regress.

**Proposal.** Table-driven tests with `httptest.NewTLSServer`:

- `internal/proxmox` — fake the Proxmox REST API for each endpoint, exercise auth header, error response decoding, UPID polling with `status=running→stopped`, `exitstatus != OK`, context cancellation.
- `internal/config` — snapshot-name normalization, validation error paths.
- `internal/state` — atomic save survives mid-write crash (simulate with failing `Write` on the temp file).
- `internal/orchestrator` — create-continues-on-failure, restore-cancels-on-first-error.

**Files to touch:**
- `internal/*/..._test.go` — new test files alongside each package.

**Effort:** 2–3 days for meaningful coverage (~60–70% statement). Unlocks #5.

## 7. TUI mode

**Problem.** Operators picking a snapshot from a long list by typing exact names is error-prone.

**Proposal.** New subcommand `pvesnap ui` that opens a [charmbracelet/bubbletea](https://github.com/charmbracelet/bubbletea) TUI with:

- **Screen 1:** list of sets (from config), arrow keys to select.
- **Screen 2:** list of snapshots for that set (from state), with timestamps and failure counts.
- **Screen 3:** action picker — restore / delete / describe.
- **Screen 4:** confirmation with live progress bar per guest.

Adds one dep (bubbletea) and pulls in lipgloss transitively — still well within the "minimal deps" budget.

**Files to touch:**
- `internal/tui/` — new package, ~500 LoC.
- `cmd/pvesnap/main.go` — dispatch for `ui` subcommand.
- `go.mod` — `+github.com/charmbracelet/bubbletea`.

**Effort:** 2–3 days. Deferred because the CLI is already ergonomic once you know your set names.

## 8. vzdump / PBS long-term archive

**Problem.** Snapshots live inside the guest's storage. If the guest is destroyed, its snapshots go too. For disaster recovery and long-term archival ("keep the v1.0 GA environment around for 2 years"), vzdump backups to PBS or an NFS target are the right primitive.

**Proposal.** A parallel command namespace `pvesnap backup create|list|restore` that:

- Uses `POST /nodes/{node}/vzdump` to create backups on a named storage target.
- Uses `GET /nodes/{node}/storage/{storage}/content` to list them.
- Uses `POST /nodes/{node}/qemu` / `POST /nodes/{node}/lxc` with `archive=` parameter to restore.
- Maintains a parallel `backups[]` section in `state.yaml`.

Restore is the hard part: a vzdump restore targets a vmid (possibly a new one), not an existing guest. The tool has to decide whether to restore in-place (stop + destroy + recreate, scary) or to a new vmid (safer, but breaks the assumption that a set's vmids are stable).

**Recommendation.** Ship this as a *separate binary* `pvesnap-backup` sharing the `internal/proxmox` + `internal/config` packages, rather than bolting it onto the snapshot CLI. Keeps the snapshot tool small and the backup tool free to evolve differently (different confirmation semantics, different storage target model).

**Files to touch:**
- `cmd/pvesnap-backup/main.go` — new entrypoint.
- `internal/proxmox/vzdump.go` — new endpoints.
- `internal/backup/` — new orchestrator / state model.

**Effort:** 1–2 weeks. Deferred: do it only when snapshot workflows are solid and someone is actually hitting the "guest destroyed, state lost" pain.

## 9. Multi-cluster tenancy (profiles)

**Problem.** If one operator manages several Proxmox clusters that don't share ACL or network, putting all their nodes into one `config.yaml` is awkward.

**Proposal.** This is mostly a documentation fix, not a code change. `-config <path>` and `PVESNAP_CONFIG` already let you maintain multiple config files:

```bash
# shell aliases
alias pvesnap-prod='pvesnap -config ~/.config/pvesnap/prod.yaml -state ~/.config/pvesnap/prod-state.yaml'
alias pvesnap-lab='pvesnap -config ~/.config/pvesnap/lab.yaml -state ~/.config/pvesnap/lab-state.yaml'
```

If that gets unwieldy, add a minimal `--profile <name>` flag that reads `~/.config/pvesnap/<name>.yaml` / `<name>-state.yaml` conventionally. Avoid building a "profile manager" — operators already have shell aliases.

**Files to touch:** maybe `cmd/pvesnap/main.go` for the sugar; mainly `docs/installation.md` to document the pattern.

**Effort:** A couple hours.

## 10. Snapshot chain inspection

Already covered by item #2 (tree view) — `SnapshotEntry.Parent` gives the chain. No separate command needed.

---

## What we're *not* going to build

- **Web UI.** Too much surface for a snapshot tool. If you want a dashboard, wrap `--json` output into Grafana/Homepage/whatever.
- **Cross-cluster snapshot migration.** Proxmox has this via `qm migrate` / `pct migrate` with snapshot preservation on some storage types; duplicating it here is out of scope.
- **Encrypted config.** The token is already sensitive; if you need secret management, use `sops` or `age` around `config.yaml` — don't bake a crypto dependency in.
- **Windows support.** The binary compiles but the `os.UserConfigDir` paths and systemd examples are Unix-shaped. Cross-compile if someone asks; don't design for it.
