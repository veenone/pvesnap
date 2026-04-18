package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/veenone/pvesnap/internal/config"
)

func RunSet(ctx context.Context, cfg *config.Config, out io.Writer, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(out, "usage: pvesnap set <list>")
		return 3
	}
	switch args[0] {
	case "list":
		return runSetList(cfg, out, args[1:])
	default:
		fmt.Fprintf(out, "unknown set subcommand: %s\n", args[0])
		return 3
	}
}

func runSetList(cfg *config.Config, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("set list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if len(cfg.Sets) == 0 {
		fmt.Fprintln(out, "no sets defined in config")
		return 0
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SET\tGUESTS\tDESCRIPTION")
	for _, s := range cfg.Sets {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", s.Name, len(s.Guests), s.Description)
	}
	_ = tw.Flush()
	fmt.Fprintln(out)
	for _, s := range cfg.Sets {
		fmt.Fprintf(out, "%s:\n", s.Name)
		gtw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(gtw, "  NODE\tTYPE\tVMID\tROLE")
		for _, g := range s.Guests {
			fmt.Fprintf(gtw, "  %s\t%s\t%d\t%s\n", g.Node, g.Type, g.VMID, g.Role)
		}
		_ = gtw.Flush()
	}
	_ = ctxUnused
	return 0
}

// ctxUnused silences the unused-import linter when subcommands don't need ctx.
var ctxUnused = context.TODO
