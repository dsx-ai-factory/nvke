// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepools

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/util"
	api "github.com/dsx-ai-factory/nvke/controller/api"
)

// NewCreateCmd returns the "nvkectl create nodepool" subcommand.
func NewCreateCmd(clientFn ControllerClientFn) *cobra.Command {
	var filename string

	cmd := &cobra.Command{
		Use:     "nodepool",
		Aliases: []string{"nodepools"},
		Short:   "Create a node pool",
		Long: `Create a node pool on the site controller.

The spec must be a single YAML or JSON document whose fields match the controller
CreateNodePoolRequest schema exactly. See /api/docs on your site controller.

Pass a file path or '-' to read from stdin (e.g. via a pipe or heredoc).

Example:
  nvkectl create nodepool -f pool.yaml

Example — stdin via pipe:
  cat pool.yaml | nvkectl create nodepool -f -`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := loadCreateNodePoolRequest(cmd, filename)
			if err != nil {
				return err
			}
			c, err := clientFn()
			if err != nil {
				return err
			}
			pool, err := c.CreateNodePool(cmd.Context(), req)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Node pool %q created (id: %s)\n", pool.Name, pool.ID)
			fmt.Fprintf(cmd.OutOrStdout(),
				"Provisioning is async — run 'nvkectl get nodepools --cluster <name>' to check status.\n")
			return nil
		},
	}

	cmd.Flags().StringVarP(&filename, "filename", "f", "", "Path to YAML spec file, or '-' to read from stdin")
	_ = cmd.MarkFlagRequired("filename")
	return cmd
}

// loadCreateNodePoolRequest reads the spec from path ("-" reads stdin) and
// deserialises it into a CreateNodePoolJSONRequestBody.
func loadCreateNodePoolRequest(cmd *cobra.Command, path string) (api.CreateNodePoolJSONRequestBody, error) {
	data, err := util.ReadFileOrStdin(cmd.InOrStdin(), path, "spec")
	if err != nil {
		return api.CreateNodePoolJSONRequestBody{}, err
	}
	return util.YAMLToRequest[api.CreateNodePoolJSONRequestBody](data)
}
