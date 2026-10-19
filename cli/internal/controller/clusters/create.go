// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/util"
	api "github.com/dsx-ai-factory/nvke/controller/api"
)

// NewCreateCmd returns the "nvkectl create cluster" subcommand.
func NewCreateCmd(clientFn ControllerClientFn) *cobra.Command {
	var filename string

	cmd := &cobra.Command{
		Use:     "cluster",
		Aliases: []string{"clusters"},
		Short:   "Create a tenant cluster",
		Long: `Create a tenant cluster on the management plane.

The spec must be a YAML document whose fields match the controller
CreateClusterRequest schema exactly. See /api/docs on your management plane.

Pass a file path or '-' to read from stdin (e.g. via a pipe or heredoc).

Example:
  nvkectl create cluster -f examples/capd-cluster.yaml

Example — stdin via pipe:
  cat examples/capd-cluster.yaml | nvkectl create cluster -f -`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := loadCreateClusterRequest(cmd, filename)
			if err != nil {
				return err
			}
			client, err := clientFn()
			if err != nil {
				return err
			}
			cluster, err := client.CreateCluster(cmd.Context(), req)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Cluster %q created (id: %s)\n", cluster.Name, cluster.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "Provisioning is async — run 'nvkectl get clusters' to check status.\n")
			return nil
		},
	}

	cmd.Flags().StringVarP(&filename, "filename", "f", "", "Path to YAML spec file, or '-' to read from stdin")
	_ = cmd.MarkFlagRequired("filename")
	return cmd
}

// loadCreateClusterRequest reads YAML from path (or stdin when path is "-")
// and deserialises it into a CreateClusterJSONRequestBody.
func loadCreateClusterRequest(cmd *cobra.Command, path string) (api.CreateClusterJSONRequestBody, error) {
	data, err := util.ReadFileOrStdin(cmd.InOrStdin(), path, "spec")
	if err != nil {
		return api.CreateClusterJSONRequestBody{}, err
	}
	return util.YAMLToRequest[api.CreateClusterJSONRequestBody](data)
}
