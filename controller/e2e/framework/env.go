// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package framework

import (
	"cmp"
	"os"
)

// getenvWithLegacy reads key, then legacyKey, its deprecated name; an empty
// value counts as unset. It has its own file without the gofail build tag so
// that the unit-test lane, which does not build gofail, runs its test. Delete
// this file and env_test.go when nke#1399 removes the deprecated names.
func getenvWithLegacy(key, legacyKey string) string {
	return cmp.Or(os.Getenv(key), os.Getenv(legacyKey))
}
