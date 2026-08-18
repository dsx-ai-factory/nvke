// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
