// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
)

const createInstancesTableSQL = `CREATE TABLE IF NOT EXISTS instances (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	backend TEXT NOT NULL CHECK (backend IN ('vm', 'container')),
	tenant_id TEXT NOT NULL,
	site_id TEXT NOT NULL,
	instance_type_id TEXT NOT NULL,
	vpc_id TEXT NOT NULL,
	machine_id TEXT,
	ipxe_script TEXT,
	user_data TEXT,
	labels_json TEXT NOT NULL,
	ssh_key_group_ids_json TEXT NOT NULL,
	status TEXT NOT NULL,
	tpm_ek_certificate TEXT,
	phone_home_enabled INTEGER NOT NULL DEFAULT 0,
	created_at DATETIME NOT NULL,
	updated_at DATETIME NOT NULL,
	UNIQUE(site_id, name)
);`

const createVMsTableSQL = `CREATE TABLE IF NOT EXISTS vms (
	instance_id TEXT PRIMARY KEY,
	desired_state TEXT NOT NULL,
	observed_state TEXT NOT NULL,
	error_reason TEXT,
	reboot_requested INTEGER NOT NULL DEFAULT 0,
	phoned_home_at DATETIME,
	FOREIGN KEY(instance_id) REFERENCES instances(id) ON DELETE CASCADE
);`

const createContainersTableSQL = `CREATE TABLE IF NOT EXISTS containers (
	instance_id TEXT PRIMARY KEY,
	FOREIGN KEY(instance_id) REFERENCES instances(id) ON DELETE CASCADE
);`

// migrateLegacyInstanceSchema converts the original VM-only instances table
// into a backend-neutral parent plus a one-to-one vms table. SQLite table
// replacement is transactional, and startup foreign-key validation makes a
// partial or invalid migration fail closed.
func (s *Store) migrateLegacyInstanceSchema(ctx context.Context) error {
	exists, err := s.tableExists(ctx, "instances")
	if err != nil || !exists {
		return err
	}
	columns, err := s.tableColumns(ctx, "instances")
	if err != nil {
		return err
	}
	vmsExist, err := s.tableExists(ctx, "vms")
	if err != nil {
		return err
	}
	if columns["backend"] && !columns["desired_state"] {
		if !vmsExist {
			vmCount, err := s.db.NewSelect().
				Model((*instanceRow)(nil)).
				Where("backend = ?", SiteBackendVM).
				Count(ctx)
			if err != nil {
				return fmt.Errorf("count VM instances without runtime table: %w", err)
			}
			if vmCount > 0 {
				return fmt.Errorf("cannot repair missing vms table: %d VM instance records have no recoverable runtime state", vmCount)
			}
			if _, err := s.db.ExecContext(ctx, createVMsTableSQL); err != nil {
				return fmt.Errorf("create missing vms table: %w", err)
			}
		}
		if !columns["phone_home_enabled"] {
			if err := s.movePhoneHomeEnabledToInstances(ctx, vmsExist); err != nil {
				return err
			}
		}
		return s.validateForeignKeys(ctx)
	}
	if !columns["desired_state"] || !columns["observed_state"] {
		return fmt.Errorf("instances schema is neither legacy nor current")
	}
	if err := s.validateLegacySiteNameUniqueness(ctx); err != nil {
		return err
	}

	backendExpr := "'vm'"
	if columns["backend"] {
		backendExpr = "backend"
	}
	tmpCertificateExpr := "''"
	if columns["tpm_ek_certificate"] {
		tmpCertificateExpr = "COALESCE(tpm_ek_certificate, '')"
	}
	phoneHomeExpr := "0"
	if columns["phone_home_enabled"] {
		phoneHomeExpr = "phone_home_enabled"
	}
	phonedHomeExpr := "NULL"
	if columns["phoned_home_at"] {
		phonedHomeExpr = "phoned_home_at"
	}

	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = OFF;"); err != nil {
		return fmt.Errorf("disable foreign keys for instance migration: %w", err)
	}
	migrationErr := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, "DROP TABLE IF EXISTS instances_migrating;"); err != nil {
			return err
		}
		createParent := strings.Replace(createInstancesTableSQL, "instances (", "instances_migrating (", 1)
		if _, err := tx.ExecContext(ctx, createParent); err != nil {
			return fmt.Errorf("create migrated instances table: %w", err)
		}
		copyParent := fmt.Sprintf(`INSERT INTO instances_migrating (
			id, name, backend, tenant_id, site_id, instance_type_id, vpc_id,
			machine_id, ipxe_script, user_data, labels_json, ssh_key_group_ids_json,
			status, tpm_ek_certificate, phone_home_enabled, created_at, updated_at
		) SELECT
			id, name, %s, tenant_id, site_id, instance_type_id, vpc_id,
			machine_id, ipxe_script, user_data, labels_json, ssh_key_group_ids_json,
			status, %s, %s, created_at, updated_at
		FROM instances;`, backendExpr, tmpCertificateExpr, phoneHomeExpr)
		if _, err := tx.ExecContext(ctx, copyParent); err != nil {
			return fmt.Errorf("copy common instance records: %w", err)
		}
		if _, err := tx.ExecContext(ctx, createVMsTableSQL); err != nil {
			return fmt.Errorf("create vms table: %w", err)
		}
		copyVMs := fmt.Sprintf(`INSERT OR IGNORE INTO vms (
			instance_id, desired_state, observed_state, error_reason,
			reboot_requested, phoned_home_at
		) SELECT
			id, desired_state, observed_state, error_reason,
			reboot_requested, %s
		FROM instances WHERE %s = 'vm';`, phonedHomeExpr, backendExpr)
		if _, err := tx.ExecContext(ctx, copyVMs); err != nil {
			return fmt.Errorf("copy VM reconciliation records: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DROP TABLE instances;"); err != nil {
			return fmt.Errorf("drop legacy instances table: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "ALTER TABLE instances_migrating RENAME TO instances;"); err != nil {
			return fmt.Errorf("activate migrated instances table: %w", err)
		}
		return nil
	})
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = ON;"); err != nil && migrationErr == nil {
		migrationErr = fmt.Errorf("re-enable foreign keys after instance migration: %w", err)
	}
	if migrationErr != nil {
		return migrationErr
	}
	if err := s.validateForeignKeys(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) movePhoneHomeEnabledToInstances(ctx context.Context, vmsExist bool) error {
	vmsHavePhoneHome := false
	if vmsExist {
		columns, err := s.tableColumns(ctx, "vms")
		if err != nil {
			return err
		}
		vmsHavePhoneHome = columns["phone_home_enabled"]
	}
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE instances ADD COLUMN phone_home_enabled INTEGER NOT NULL DEFAULT 0;`); err != nil {
			return fmt.Errorf("add instance phone-home setting: %w", err)
		}
		if !vmsHavePhoneHome {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE instances
			SET phone_home_enabled = COALESCE((
				SELECT vm.phone_home_enabled FROM vms AS vm WHERE vm.instance_id = instances.id
			), 0)
			WHERE backend = 'vm';
		`); err != nil {
			return fmt.Errorf("move VM phone-home setting to instance: %w", err)
		}
		return nil
	})
}

func (s *Store) validateLegacySiteNameUniqueness(ctx context.Context) error {
	type duplicate struct {
		SiteID string `bun:"site_id"`
		Name   string `bun:"name"`
		Count  int    `bun:"count"`
	}
	var row duplicate
	err := s.db.NewSelect().
		TableExpr("instances").
		Column("site_id", "name").
		ColumnExpr("COUNT(*) AS count").
		Group("site_id", "name").
		Having("COUNT(*) > 1").
		Limit(1).
		Scan(ctx, &row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check legacy instance name uniqueness: %w", err)
	}
	return fmt.Errorf(
		"cannot migrate instances: site %s has %d records named %q; instance names must be unique within a site",
		row.SiteID, row.Count, row.Name,
	)
}

func (s *Store) tableExists(ctx context.Context, name string) (bool, error) {
	var count int
	if err := s.db.NewSelect().
		TableExpr("sqlite_master").
		ColumnExpr("COUNT(*)").
		Where("type = 'table'").
		Where("name = ?", name).
		Scan(ctx, &count); err != nil {
		return false, fmt.Errorf("check table %s: %w", name, err)
	}
	return count > 0, nil
}

func (s *Store) tableColumns(ctx context.Context, table string) (map[string]bool, error) {
	type columnInfo struct {
		CID          int     `bun:"cid"`
		Name         string  `bun:"name"`
		Type         string  `bun:"type"`
		NotNull      int     `bun:"column:notnull"`
		DefaultValue *string `bun:"dflt_value"`
		PrimaryKey   int     `bun:"column:pk"`
	}
	var rows []columnInfo
	if err := s.db.NewRaw("PRAGMA table_info("+table+");").Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("inspect table %s: %w", table, err)
	}
	columns := make(map[string]bool, len(rows))
	for _, row := range rows {
		columns[row.Name] = true
	}
	return columns, nil
}

func (s *Store) validateForeignKeys(ctx context.Context) error {
	type violation struct {
		Table  string `bun:"table"`
		RowID  int64  `bun:"rowid"`
		Parent string `bun:"parent"`
	}
	var violations []violation
	if err := s.db.NewRaw("PRAGMA foreign_key_check;").Scan(ctx, &violations); err != nil {
		return fmt.Errorf("validate migrated foreign keys: %w", err)
	}
	if len(violations) > 0 {
		return fmt.Errorf("instance schema migration left %d foreign-key violations", len(violations))
	}
	return nil
}

func (s *Store) validateBackendStateRows(ctx context.Context) error {
	var invalid int
	if err := s.db.NewSelect().
		TableExpr("instances AS instance").
		ColumnExpr("COUNT(*)").
		Join("LEFT JOIN vms AS vm ON vm.instance_id = instance.id").
		Join("LEFT JOIN containers AS container_state ON container_state.instance_id = instance.id").
		Where(`
			(instance.backend = 'vm' AND (vm.instance_id IS NULL OR container_state.instance_id IS NOT NULL))
			OR
			(instance.backend = 'container' AND (container_state.instance_id IS NULL OR vm.instance_id IS NOT NULL))
		`).
		Scan(ctx, &invalid); err != nil {
		return fmt.Errorf("validate backend state records: %w", err)
	}
	if invalid > 0 {
		return fmt.Errorf("instance schema has %d invalid backend state records", invalid)
	}
	return nil
}
