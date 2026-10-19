// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package framework

import (
	"cmp"
	"os"
)

// getenvWithLegacy reads key, then legacyKeys, its deprecated names, in order;
// an empty value counts as unset. It has its own file without the gofail build tag so
// that the unit-test lane, which does not build gofail, runs its test. Delete
// this file and env_test.go when #1399 removes the deprecated names.
func getenvWithLegacy(key string, legacyKeys ...string) string {
	value := os.Getenv(key)
	for _, legacyKey := range legacyKeys {
		value = cmp.Or(value, os.Getenv(legacyKey))
	}
	return value
}
