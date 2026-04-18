package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/veenone/pvesnap/internal/cli"
	"github.com/veenone/pvesnap/internal/config"
	"github.com/veenone/pvesnap/internal/state"
)

const usage = `pvesnap — orchestrate Proxmox snapshots across sets of guests

Usage:
  pvesnap [global flags] <command> [command flags] [args]

Global flags:
  -config <path>   path to config.yaml (default: $XDG_CONFIG_HOME/pvesnap/config.yaml)
  -state  <path>   path to state.yaml  (default: $XDG_CONFIG_HOME/pvesnap/state.yaml)

Commands:
  discover                              list guests known to each configured node
  set list                              list sets defined in config
  snapshot create <set> <name>          create a named snapshot across a set
  snapshot list [<set>]                 list recorded snapshots
  snapshot restore <set> <name>         roll back a set to a recorded snapshot
  snapshot delete <set> <name>          delete a recorded snapshot

Exit codes:
  0 success  1 partial failure  2 full failure  3 usage/config error
`

func main() {
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet("pvesnap", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	cfgPath := fs.String("config", config.DefaultPath(), "path to config.yaml")
	statePath := fs.String("state", state.DefaultPath(), "path to state.yaml")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 3
	}
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		return 3
	}

	// Help never loads config.
	switch args[0] {
	case "help", "-h", "--help":
		fs.Usage()
		return 0
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	switch args[0] {
	case "discover":
		return cli.RunDiscover(ctx, cfg, os.Stdout, args[1:])
	case "set":
		return cli.RunSet(ctx, cfg, os.Stdout, args[1:])
	case "snapshot":
		return cli.RunSnapshot(ctx, cfg, *statePath, os.Stdout, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		fs.Usage()
		return 3
	}
}
