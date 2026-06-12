# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`pvesnap` is a small Go CLI that orchestrates Proxmox snapshots across **sets** of guests (LXC containers and QEMU VMs) spanning one or more nodes. A "snapshot" here is a *logical* group: it fans a single named snapshot out to every guest in a set in parallel, over the Proxmox HTTPS API. Built for jumping between states in an end-to-end test environment — seconds-scale rollback.

## Commands

```bash
go build -o pvesnap ./cmd/pvesnap   # build the binary
go vet ./...                        # vet
go test ./...                       # run tests (NOTE: no tests exist yet)
go test ./internal/config -run TestNormalizeSnapName   # run a single test, once tests exist
```

There is currently no test suite, no linter config, and no CI. The dependency budget is deliberately tiny — `gopkg.in/yaml.v3`, `golang.org/x/sync/errgroup`, and the Go stdlib. Subcommand dispatch is a hand-rolled `switch` over `flag.FlagSet`; **do not introduce Cobra/Viper or other frameworks** without a strong reason.

Running it locally requires a `config.yaml` (see `examples/config.yaml`) pointed at a real Proxmox cluster; both `config.yaml` and `state.yaml` are gitignored. Config/state paths resolve from `-config`/`-state` flags, then `$PVESNAP_CONFIG`/`$PVESNAP_STATE`, then `$XDG_CONFIG_HOME/pvesnap/`.

## Architecture

Request flow: `cmd/pvesnap/main.go` (global flags, signal-aware context, subcommand dispatch) → `internal/cli/*` (per-command flag parsing + output) → `internal/orchestrator` (fan-out) → `internal/proxmox` (HTTP client). `internal/config` and `internal/state` are the two persistence layers.

**Every Proxmox write returns a UPID immediately and runs async.** The pattern throughout `internal/proxmox/snapshot.go` is: POST/DELETE returns a UPID string → `client.WaitTask` polls `/nodes/<node>/tasks/<upid>/status` until `status=stopped`, then checks `exitstatus=="OK"`. Any new write operation must follow this UPID-then-poll pattern.

**Per-node concurrency gate.** `orchestrator.New` builds one buffered channel per node sized by `defaults.parallelism_per_node` (default 2). Every fan-out goroutine must `acquire(ctx, node)` / `defer release(node)` around its API calls. Proxmox serializes per-guest ops anyway, so this cap is about not overwhelming a node, not throughput.

**The create/restore/delete asymmetry is the most important design decision** — see `internal/orchestrator/orchestrator.go`:
- `Create` and `Delete` use `sync.WaitGroup` and **run every guest even if some fail** — partial state is recoverable and you want visibility into which guests succeeded.
- `Restore` uses `errgroup` with **cancel-on-first-error** — a half-rolled-back environment (some guests at v1.5, some at v1.4) is actively dangerous, so any failure aborts the rest.

Preserve this distinction when touching the orchestrator.

**State vs. Proxmox reality.** `state.yaml` is the *only* place the set→snapshot grouping lives; Proxmox itself only knows about individual per-guest snapshots that happen to share a name. Losing `state.yaml` loses the grouping metadata, not the snapshots. Writes go through `Store.Save`, which is atomic (temp file + `rename`) and chmods to `0600` — keep it that way. Restore never mutates state; delete only removes the state entry when **all** guests succeeded *and* no `-vmid` filter was applied (a partial/filtered delete leaves the record so you can see what remains).

**Cluster discovery.** `discover` hits `/cluster/resources?type=vm`; on a real cluster any single node returns the full view, so results are deduped by `node/vmid`. Templates and non-qemu/lxc types are filtered out.

## Conventions

- **Exit codes are a contract** (CI/systemd depend on them): `0` success, `1` partial failure, `2` full failure, `3` usage/config error. Every command's final `switch` maps ok/fail counts to these — match it in new commands.
- Snapshot names must satisfy Proxmox's `^[A-Za-z][A-Za-z0-9_-]{1,39}$`. User input is run through `config.NormalizeSnapName` (substitutes invalid chars with `-`, prefixes `s` if it doesn't start with a letter, truncates to 40). Always normalize user-supplied names before sending them to the API.
- Tabular output uses `text/tabwriter` with a `NODE TYPE VMID STATUS DETAIL`-style header — follow the existing column style.
- TLS verification is opt-in per node via `verify_tls` (defaults to skip-verify, since homelab Proxmox usually has self-signed certs).
- `cmd_set.go` has a `ctxUnused = context.TODO` shim to satisfy the import; the `set` command doesn't need a context.

## Docs

`docs/` is the canonical reference and worth reading before non-trivial changes: `architecture.md` (this file's source of truth, with diagrams), `commands.md`, `installation.md`, `operations.md` (LXC storage-backend snapshot caveats, partial-failure recovery, scheduling, token scoping), `proxmox-api.md`, and `roadmap.md` (planned sync/retention/TUI and explicitly-out-of-scope features — check it before building something that may be deliberately excluded).

## gstack

This project uses [gstack](https://github.com/garrytan/gstack) skills. They are not vendored in the repo — install gstack locally (`git clone --single-branch --depth 1 https://github.com/garrytan/gstack.git ~/.claude/skills/gstack && cd ~/.claude/skills/gstack && ./setup`) so the skills are available.

**Browsing rule:** Use the `/browse` skill from gstack for **all** web browsing (navigation, QA, screenshots, dogfooding, scraping page state). **Never** use `mcp__claude-in-chrome__*` tools — route all browser interaction through `/browse` instead.

**Available gstack skills:**

`/office-hours`, `/plan-ceo-review`, `/plan-eng-review`, `/plan-design-review`, `/design-consultation`, `/design-shotgun`, `/design-html`, `/review`, `/ship`, `/land-and-deploy`, `/canary`, `/benchmark`, `/browse`, `/connect-chrome`, `/qa`, `/qa-only`, `/design-review`, `/setup-browser-cookies`, `/setup-deploy`, `/setup-gbrain`, `/retro`, `/investigate`, `/document-release`, `/document-generate`, `/codex`, `/cso`, `/autoplan`, `/plan-devex-review`, `/devex-review`, `/careful`, `/freeze`, `/guard`, `/unfreeze`, `/gstack-upgrade`, `/learn`
