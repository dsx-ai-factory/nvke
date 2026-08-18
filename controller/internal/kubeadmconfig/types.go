// SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"fmt"
	"sort"
	"strings"
)

type KubeLabels map[string]string

func (l KubeLabels) String() string {
	keys := make([]string, 0, len(l))
	for key := range l {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	labels := make([]string, 0, len(l))
	for _, key := range keys {
		labels = append(labels, fmt.Sprintf("%s=%s", key, l[key]))
	}
	return strings.Join(labels, ",")
}
