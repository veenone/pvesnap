# pvesnap

A small Go CLI for orchestrating Proxmox snapshots across sets of LXC containers and VMs on multiple nodes. Built to jump between product-delivery setups in an E2E test environment.

## Documentation

Full documentation lives in the **pvesnap** collection on Outline:
http://outline.myhome.lan/collection/pvesnap-ER3D4MgIcF

- Overview
- Architecture
- Installation & Configuration
- Command Reference
- Operational Guide
- Proxmox API Reference

## Quick start

```bash
go build -o pvesnap ./cmd/pvesnap
cp examples/config.yaml ~/.config/pvesnap/config.yaml
# edit api_token values, then:
./pvesnap discover
./pvesnap snapshot create e2e-core baseline --description "pre-test"
./pvesnap snapshot restore e2e-core baseline --yes
```

See `examples/config.yaml` for the full schema.
