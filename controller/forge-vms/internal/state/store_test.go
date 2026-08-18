// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"path/filepath"
	"testing"
)

// Open creates every table and sets both pragmas, so opening a store exercises
// the SQLite driver end to end. The driver is a cgo binding that compiles to a
// stub when cgo is off and only fails on the first query, so this is what
// catches a test binary built without cgo.
func TestOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.db")

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	var journalMode string
	if err := store.sqldb.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var foreignKeys int
	if err := store.sqldb.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}

	if _, err := store.db.NewSelect().Model((*TenantRow)(nil)).Count(context.Background()); err != nil {
		t.Errorf("count tenants: %v", err)
	}
}

// Bootstrap runs on every Open, so reopening an existing file must not fail on
// tables that are already there.
func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
}
