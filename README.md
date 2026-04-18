# pvesnap

A small Go CLI for orchestrating Proxmox snapshots across sets of LXC containers and VMs on multiple nodes. Built to jump between product-delivery setups in an end-to-end test environment.

It groups Proxmox guests across one or more nodes into named **sets**, then creates, lists, restores, and deletes a single logical **snapshot** that fans out to every guest in the set in parallel.

## Quick start

```bash
go build -o pvesnap ./cmd/pvesnap
mkdir -p ~/.config/pvesnap
cp examples/config.yaml ~/.config/pvesnap/config.yaml
chmod 0600 ~/.config/pvesnap/config.yaml
# edit api_token values in config.yaml, then:

./pvesnap discover
./pvesnap snapshot create e2e-core baseline --description "pre-test"
./pvesnap snapshot restore e2e-core baseline --yes
```

See `examples/config.yaml` for the full schema.

## Documentation

- [Architecture](docs/architecture.md) — how the pieces fit together, project layout, concurrency model.
- [Installation & Configuration](docs/installation.md) — building the binary, generating a scoped API token, writing `config.yaml`.
- [Command Reference](docs/commands.md) — every subcommand with examples.
- [Operational Guide](docs/operations.md) — LXC storage caveats, partial-failure recovery, scheduling, security notes.
- [Proxmox API Reference](docs/proxmox-api.md) — endpoints and UPID polling pattern.
- [Roadmap](docs/roadmap.md) — planned features: sync, retention, TUI, vzdump/PBS, and what we're deliberately *not* building.

## Design choices

- Native Proxmox per-guest snapshots via the `/snapshot` API — seconds-scale rollback.
- One-shot CLI, no daemon. Use systemd timers or cron for scheduling.
- API-token auth (`PVEAPIToken` header), no session renewal.
- Local YAML state file, human-readable and git-versionable.
- Three deps total: `gopkg.in/yaml.v3`, `golang.org/x/sync/errgroup`, and the Go stdlib.

## License

TBD
