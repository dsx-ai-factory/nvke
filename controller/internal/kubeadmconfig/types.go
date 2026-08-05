// SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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
