package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/veenone/pvesnap/internal/config"
	"github.com/veenone/pvesnap/internal/proxmox"
)

func RunDiscover(ctx context.Context, cfg *config.Config, out io.Writer, args []string) int {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	node := fs.String("node", "", "limit to a single node name")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	client := proxmox.NewClient(cfg)
	var queryNodes []string
	if *node != "" {
		if _, ok := cfg.FindNode(*node); !ok {
			fmt.Fprintf(out, "unknown node: %s\n", *node)
			return 3
		}
		queryNodes = []string{*node}
	} else if len(cfg.Nodes) > 0 {
		// Only one of the configured endpoints is needed for cluster-wide view;
		// but on standalone (non-clustered) nodes we must query each separately.
		for _, n := range cfg.Nodes {
			queryNodes = append(queryNodes, n.Name)
		}
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "QUERIED\tNODE\tTYPE\tVMID\tNAME\tSTATUS")
	seen := map[string]bool{} // dedupe when clustered endpoints return overlapping views
	exitCode := 0
	for _, qn := range queryNodes {
		resources, err := client.ClusterResources(ctx, qn)
		if err != nil {
			fmt.Fprintf(out, "discover on %s: %v\n", qn, err)
			exitCode = 1
			continue
		}
		sort.Slice(resources, func(i, j int) bool {
			if resources[i].Node != resources[j].Node {
				return resources[i].Node < resources[j].Node
			}
			return resources[i].VMID < resources[j].VMID
		})
		for _, r := range resources {
			if r.Type != "qemu" && r.Type != "lxc" {
				continue
			}
			if r.Template == 1 {
				continue
			}
			key := fmt.Sprintf("%s/%d", r.Node, r.VMID)
			if seen[key] {
				continue
			}
			seen[key] = true
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", qn, r.Node, r.Type, r.VMID, r.Name, r.Status)
		}
	}
	_ = tw.Flush()
	return exitCode
}
