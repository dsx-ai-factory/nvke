// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"

	"go.temporal.io/sdk/activity"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig"
)

const (
	legacyKubeadmFilesRunbook = "kubeadm-files"
)

// ensureKubeadmFiles stages the rendered kubeadm prerequisite files through a
// legacy runbook when the connected node agent cannot consume request files.
func (a *Activities) ensureKubeadmFiles(ctx context.Context, node db.Node, files []kubeadmconfig.File) error {
	if len(files) == 0 {
		return nil
	}

	supported, err := a.nodeAgentClient.SupportsKubeadmFiles(ctx, node)
	if err != nil {
		return fmt.Errorf("checking node agent kubeadm file capability: %w", err)
	}
	if supported {
		return nil
	}

	activity.GetLogger(ctx).Info(
		"node agent does not support kubeadm request files; staging prerequisites through legacy runbook",
		"nodeID", node.ID,
		"nodeName", node.Name,
		"fileCount", len(files),
		"runbookName", legacyKubeadmFilesRunbook,
	)
	if _, err := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
		HostID:      node.ID,
		RunbookName: legacyKubeadmFilesRunbook,
		RunbookArgs: kubeadmFilesRunbookArgs(files),
	}); err != nil {
		return fmt.Errorf("executing legacy kubeadm files runbook: %w", err)
	}

	return nil
}

// kubeadmFilesRunbookArgs serializes each file as path, base64 content, mode,
// and owner. ExecuteScript passes these as argv entries, so file contents are
// not interpreted as shell syntax by the legacy agent.
func kubeadmFilesRunbookArgs(files []kubeadmconfig.File) []string {
	args := make([]string, 0, len(files)*4)
	for _, file := range files {
		args = append(args,
			file.Path,
			base64.StdEncoding.EncodeToString(file.Content),
			strconv.FormatUint(uint64(file.Mode), 8),
			file.Owner,
		)
	}
	return args
}
