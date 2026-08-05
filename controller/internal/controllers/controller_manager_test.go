// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package controllers

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestNewControllerManagerGatesCAPIClusterScaffold(t *testing.T) {
	tests := map[string]struct {
		enableForgeCapi bool
		wantScaffold    bool
	}{
		"disabled": {
			enableForgeCapi: false,
			wantScaffold:    false,
		},
		"enabled": {
			enableForgeCapi: true,
			wantScaffold:    true,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			manager, err := NewControllerManager(
				config.Config{EnableForgeCapi: params.enableForgeCapi},
				slog.Default(),
				&testutil.MockDB{},
				fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build(),
				nil,
				nil,
				nil,
				nil,
				"",
				nil,
			)
			require.NoError(t, err)

			if params.wantScaffold {
				require.NotNil(t, manager.capiClusterScaffold)
				return
			}
			require.Nil(t, manager.capiClusterScaffold)
		})
	}
}
