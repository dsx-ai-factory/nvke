// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"github.com/spf13/cobra"

	operatorcmd "github.com/dsx-ai-factory/nvke/cli/internal/operator/command"
)

func newDeprecateCmd() *cobra.Command {
	return operatorcmd.NewDeprecateCmd(loadOperator)
}
