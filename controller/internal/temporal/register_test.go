// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package temporal

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	rollouttemporal "github.com/NVIDIA/nke/controller/internal/rollout/temporal"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

func TestLegacyAndRolloutActivityMethodNamesDoNotCollide(t *testing.T) {
	legacyType := reflect.TypeOf((*activities.Activities)(nil))
	rolloutType := reflect.TypeOf((*rollouttemporal.Activities)(nil))

	legacyNames := make(map[string]struct{}, legacyType.NumMethod())
	for i := range legacyType.NumMethod() {
		legacyNames[legacyType.Method(i).Name] = struct{}{}
	}

	for i := range rolloutType.NumMethod() {
		name := rolloutType.Method(i).Name
		_, collides := legacyNames[name]
		require.Falsef(t, collides,
			"activity method %q exists on both legacy and rollout activity receivers; Temporal may resolve a function-based legacy invocation to the rollout activity name", name)
	}
}
