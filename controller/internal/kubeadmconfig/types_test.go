// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package kubeadmconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKubeLabelsStringIsDeterministic(t *testing.T) {
	labels := KubeLabels{
		"zebra":  "z",
		"alpha":  "a",
		"middle": "m",
	}

	assert.Equal(t, "alpha=a,middle=m,zebra=z", labels.String())
	// Call again to ensure stable ordering across invocations.
	assert.Equal(t, "alpha=a,middle=m,zebra=z", labels.String())
}
