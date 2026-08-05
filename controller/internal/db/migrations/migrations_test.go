// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package migrations

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestMigrationNamesUnique guards against the timestamp-prefix collision class of bug.
//
// bun's migrate package identifies each migration solely by the 14-digit timestamp
// prefix of its filename (see migrate.extractMigrationName). The descriptive suffix is
// only stored as a comment. If two migration files share the same 14-digit prefix, bun
// treats them as the same migration: once one has been recorded as applied, the other is
// silently skipped, leaving the database missing that migration's schema changes and
// producing runtime errors (e.g. SQLSTATE 42703, undefined column) on unrelated queries.
//
// This test fails loudly if any two registered migrations share a name.
func TestMigrationNamesUnique(t *testing.T) {
	commentsByName := make(map[string][]string)
	for _, m := range migrations.Sorted() {
		commentsByName[m.Name] = append(commentsByName[m.Name], m.Comment)
	}

	var dups []string
	for name, comments := range commentsByName {
		if len(comments) > 1 {
			sort.Strings(comments)
			dups = append(dups, fmt.Sprintf("%s (%s)", name, strings.Join(comments, ", ")))
		}
	}

	if len(dups) > 0 {
		sort.Strings(dups)
		t.Fatalf("duplicate migration name(s) detected; bun identifies migrations by their "+
			"14-digit timestamp prefix, so colliding migrations are silently skipped: %s",
			strings.Join(dups, "; "))
	}
}
