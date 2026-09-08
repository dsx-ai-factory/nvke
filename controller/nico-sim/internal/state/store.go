// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // sqlite driver; requires cgo
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
)

var (
	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("conflict")
	ErrSubnetExhausted = errors.New("subnet exhausted")
	ErrVpcPfxExhausted = errors.New("vpc prefix exhausted")
	ErrBadRequest      = errors.New("bad request")
)

type Store struct {
	sqldb *sql.DB
	db    *bun.DB
	nowFn func() time.Time
}

func Open(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create db directory: %w", err)
	}

	sqldb, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite db: %w", err)
	}
	sqldb.SetMaxOpenConns(1)

	db := bun.NewDB(sqldb, sqlitedialect.New())
	store := &Store{
		sqldb: sqldb,
		db:    db,
		nowFn: time.Now,
	}

	if _, err := store.db.Exec("PRAGMA journal_mode = WAL;"); err != nil {
		return nil, fmt.Errorf("failed to set journal_mode: %w", err)
	}
	if _, err := store.db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		return nil, fmt.Errorf("failed to set foreign_keys: %w", err)
	}

	if err := store.Bootstrap(context.Background()); err != nil {
		return nil, err
	}

	return store, nil
}

func (s *Store) Close() error {
	return s.sqldb.Close()
}

func (s *Store) Bootstrap(ctx context.Context) error {
	// Simple tables via bun CreateTable — these have straightforward schemas
	// that bun can derive from struct tags.
	models := []interface{}{
		(*TenantRow)(nil),
		(*SiteRow)(nil),
		(*VPCRow)(nil),
		(*SubnetRow)(nil),
		(*VPCPrefixRow)(nil),
		(*InstanceTypeRow)(nil),
		(*SSHKeyGroupRow)(nil),
		(*IPBlockRow)(nil),
		(*InfiniBandPartitionRow)(nil),
	}
	for _, model := range models {
		if _, err := s.db.NewCreateTable().Model(model).IfNotExists().Exec(ctx); err != nil {
			return fmt.Errorf("failed to create table for %T: %w", model, err)
		}
	}

	// Complex tables with constraints — raw SQL for composite unique
	// constraints, foreign-key cascades, and autoincrement PKs.
	complexStmts := []string{
		`CREATE TABLE IF NOT EXISTS instances (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			site_id TEXT NOT NULL,
			instance_type_id TEXT NOT NULL,
			vpc_id TEXT NOT NULL,
			machine_id TEXT,
			ipxe_script TEXT,
			user_data TEXT,
			labels_json TEXT NOT NULL,
			ssh_key_group_ids_json TEXT NOT NULL,
			desired_state TEXT NOT NULL,
			observed_state TEXT NOT NULL,
			status TEXT NOT NULL,
			error_reason TEXT,
			reboot_requested INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			UNIQUE(tenant_id, name)
		);`,
		`CREATE TABLE IF NOT EXISTS instance_interfaces (
			id TEXT PRIMARY KEY,
			instance_id TEXT NOT NULL,
			subnet_id TEXT,
			vpc_prefix_id TEXT,
			ip_addresses_json TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			FOREIGN KEY(instance_id) REFERENCES instances(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS ip_allocations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scope_type TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			ip_address TEXT NOT NULL,
			instance_id TEXT NOT NULL,
			interface_id TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			UNIQUE(scope_type, scope_id, ip_address)
		);`,
	}
	for _, stmt := range complexStmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("failed bootstrap statement: %w", err)
		}
	}

	// Schema migrations — add columns that may not exist in older databases.
	// Each uses "IF NOT EXISTS" or ignores "duplicate column" errors.
	migrations := []string{
		`ALTER TABLE instances ADD COLUMN phone_home_enabled INTEGER NOT NULL DEFAULT 0;`,
		`ALTER TABLE instances ADD COLUMN phoned_home_at DATETIME;`,
		`ALTER TABLE instance_interfaces ADD COLUMN mac_address TEXT;`,
		`ALTER TABLE instances ADD COLUMN tpm_ek_certificate TEXT;`,
		`ALTER TABLE instance_types ADD COLUMN disk_gib INTEGER NOT NULL DEFAULT 0;`,
	}
	for _, stmt := range migrations {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			// Ignore "duplicate column name" — column already exists.
			if !isDuplicateColumnErr(err) {
				return fmt.Errorf("failed migration: %w", err)
			}
		}
	}

	// Indexes
	indexStmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_instances_site_id ON instances(site_id);`,
		`CREATE INDEX IF NOT EXISTS idx_instances_tenant_id ON instances(tenant_id);`,
		`CREATE INDEX IF NOT EXISTS idx_instances_desired_observed ON instances(desired_state, observed_state);`,
		`CREATE INDEX IF NOT EXISTS idx_interfaces_instance_id ON instance_interfaces(instance_id);`,
		`CREATE INDEX IF NOT EXISTS idx_interfaces_mac ON instance_interfaces(mac_address);`,
		`CREATE INDEX IF NOT EXISTS idx_alloc_scope ON ip_allocations(scope_type, scope_id);`,
	}
	for _, stmt := range indexStmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("failed bootstrap index statement: %w", err)
		}
	}

	return nil
}

func isDuplicateColumnErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

func normalizePage(page Page) Page {
	if page.Size <= 0 {
		page.Size = 100
	}
	if page.Number <= 0 {
		page.Number = 1
	}
	return page
}
