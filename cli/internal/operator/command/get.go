// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/dsx-ai-factory/nvke/cli/internal/operator"
	"github.com/dsx-ai-factory/nvke/cli/internal/util"
)

// NewGetCmds returns the "nvkectl get machineimages|clusterreleases|
// nodepoolreleases" subcommands.
func NewGetCmds(clientFn ClientFn) []*cobra.Command {
	cmds := make([]*cobra.Command, 0, len(kindInfos))
	for _, k := range kindInfos {
		cmds = append(cmds, newGetCmd(k, clientFn))
	}
	return cmds
}

func newGetCmd(k kindInfo, clientFn ClientFn) *cobra.Command {
	var outputFormat string

	cmd := &cobra.Command{
		Use:     k.plural() + " [name ...]",
		Aliases: []string{k.use},
		Short:   "List " + k.noun + "s on the active site's management cluster",
		Long: fmt.Sprintf(`List %ss on the active site's management cluster.

Naming %ss narrows the listing to them, in the order given.

With -o json or -o yaml, one name prints that %s as a single object in the
metadata and spec shape that 'nvkectl create %s' accepts, so it can be edited and
created under a new name. No name, or several, prints a list of those objects.

Example - every %s:
  nvkectl get %s

Example - one %s, as YAML:
  nvkectl get %s <name> -o yaml`,
			k.noun, k.noun, k.noun, k.use, k.noun, k.plural(), k.noun, k.plural()),
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := util.ParseFormat(outputFormat)
			if err != nil {
				return err
			}
			c, err := clientFn()
			if err != nil {
				return err
			}
			items, missing, err := listResources(cmd, c, k, args)
			if err != nil {
				return err
			}
			if format != util.FormatTable {
				// Like kubectl: one name is one object, which is what create
				// reads back; no name or several names are a list.
				if len(args) == 1 {
					if len(items) == 0 {
						return notFoundError(k, missing)
					}
					return util.Print(cmd.OutOrStdout(), format, items[0])
				}
				if err := util.Print(cmd.OutOrStdout(), format, items); err != nil {
					return err
				}
				return notFoundError(k, missing)
			}
			printResources(cmd.OutOrStdout(), k.kind, items)
			return notFoundError(k, missing)
		},
	}

	cmd.Flags().StringVarP(&outputFormat, "output", "o", "", "Output format: json or yaml (default: table)")
	return cmd
}

// listResources returns the resources named by names, in the order given, plus
// the names that matched nothing. No names lists every resource of the kind.
func listResources(cmd *cobra.Command, c operator.Client, k kindInfo, names []string) ([]operator.Resource, []string, error) {
	if len(names) == 0 {
		items, err := c.List(cmd.Context(), k.kind)
		if err != nil {
			return nil, nil, err
		}
		if items == nil {
			items = []operator.Resource{}
		}
		return items, nil, nil
	}
	items := make([]operator.Resource, 0, len(names))
	var missing []string
	for _, name := range names {
		r, err := c.Get(cmd.Context(), k.kind, name)
		switch {
		case isNotFound(err):
			missing = append(missing, name)
		case err != nil:
			return nil, nil, err
		default:
			items = append(items, r)
		}
	}
	return items, missing, nil
}

// printResources writes items to out as a table suited to the kind.
func printResources(out io.Writer, kind operator.Kind, items []operator.Resource) {
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	if kind == operator.KindMachineImage {
		fmt.Fprintln(w, "NAME\tOS\tVERSION\tARCH\tVARIANT\tBUILT\tAGE")
		for _, r := range items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.Metadata.Name,
				specString(r, "operatingSystem", "family"),
				specString(r, "operatingSystem", "version"),
				specString(r, "architecture"),
				specString(r, "variant"),
				specString(r, "buildDate"),
				age(r),
			)
		}
	} else {
		fmt.Fprintln(w, "NAME\tIMAGE\tDEPRECATED\tAGE")
		for _, r := range items {
			deprecated, _, _ := unstructured.NestedBool(r.Spec, "lifecycle", "deprecated")
			fmt.Fprintf(w, "%s\t%s\t%t\t%s\n",
				r.Metadata.Name,
				specString(r, "machineImageRef", "name"),
				deprecated,
				age(r),
			)
		}
	}
	w.Flush()
}

// specString returns the string at path in the resource's spec, or "" when it
// is absent.
func specString(r operator.Resource, path ...string) string {
	s, _, _ := unstructured.NestedString(r.Spec, path...)
	return s
}

// age returns how long ago the resource was created, or "" when unknown.
func age(r operator.Resource) string {
	if r.CreatedAt.IsZero() {
		return ""
	}
	return util.FormatAge(r.CreatedAt)
}
