// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func TestOpenMigratesLegacyInstancesToParentAndVM(t *testing.T) {
	t.Parallel()

	dbPath := t.TempDir() + "/state.db"
	legacy, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	legacy.SetMaxOpenConns(1)
	ctx := context.Background()
	for _, statement := range []string{
		`PRAGMA foreign_keys = ON;`,
		`CREATE TABLE instances (
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
			phone_home_enabled INTEGER NOT NULL DEFAULT 0,
			phoned_home_at DATETIME,
			tpm_ek_certificate TEXT,
			UNIQUE(tenant_id, name)
		);`,
		`CREATE TABLE instance_interfaces (
			id TEXT PRIMARY KEY,
			instance_id TEXT NOT NULL,
			subnet_id TEXT,
			vpc_prefix_id TEXT,
			ip_addresses_json TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			mac_address TEXT,
			FOREIGN KEY(instance_id) REFERENCES instances(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE instance_routes (
			instance_id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL,
			backend TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		);`,
	} {
		if _, err := legacy.ExecContext(ctx, statement); err != nil {
			legacy.Close()
			t.Fatalf("prepare legacy schema: %v", err)
		}
	}

	createdAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	phonedHomeAt := createdAt.Add(time.Minute)
	if _, err := legacy.ExecContext(ctx, `INSERT INTO instances (
		id, name, tenant_id, site_id, instance_type_id, vpc_id, machine_id,
		ipxe_script, user_data, labels_json, ssh_key_group_ids_json,
		desired_state, observed_state, status, error_reason, reboot_requested,
		created_at, updated_at, phone_home_enabled, phoned_home_at, tpm_ek_certificate
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`,
		"instance-1", "worker-1", "tenant-1", "site-1", "type-1", "vpc-1", "machine-1",
		"#!ipxe", "user-data", `{"cluster":"one"}`, `["keys-1"]`,
		DesiredStatePresent, ObservedStateReady, "ready", "", true,
		createdAt, phonedHomeAt, true, phonedHomeAt, "certificate-1",
	); err != nil {
		legacy.Close()
		t.Fatalf("insert legacy instance: %v", err)
	}
	if _, err := legacy.ExecContext(ctx, `INSERT INTO instance_interfaces (
		id, instance_id, subnet_id, ip_addresses_json, status, created_at, updated_at, mac_address
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?);`,
		"interface-1", "instance-1", "subnet-1", `["10.0.0.10"]`, "ready", createdAt, phonedHomeAt, "02:00:00:00:00:01",
	); err != nil {
		legacy.Close()
		t.Fatalf("insert legacy interface: %v", err)
	}
	if _, err := legacy.ExecContext(ctx, `INSERT INTO instance_routes VALUES (?, ?, ?, ?, ?);`,
		"instance-1", "site-1", string(SiteBackendVM), createdAt, phonedHomeAt,
	); err != nil {
		legacy.Close()
		t.Fatalf("insert legacy route: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open and migrate database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	record, err := store.GetInstance(ctx, "instance-1")
	if err != nil {
		t.Fatalf("get migrated instance: %v", err)
	}
	if record.Backend != SiteBackendVM || record.Name != "worker-1" {
		t.Fatalf("unexpected migrated parent: %+v", record)
	}
	if record.MachineID != "machine-1" || record.TpmEkCertificate != "certificate-1" || !record.PhoneHomeEnabled {
		t.Fatalf("common instance fields were not preserved: %+v", record)
	}
	if record.VM == nil || record.VM.DesiredState != DesiredStatePresent || record.VM.ObservedState != ObservedStateReady {
		t.Fatalf("unexpected migrated VM state: %+v", record.VM)
	}
	if !record.VM.RebootRequested || record.VM.PhonedHomeAt == nil {
		t.Fatalf("VM lifecycle fields were not preserved: %+v", record.VM)
	}
	if len(record.Interfaces) != 1 || record.Interfaces[0].MACAddress != "02:00:00:00:00:01" {
		t.Fatalf("interfaces were not preserved: %+v", record.Interfaces)
	}

	columns, err := store.tableColumns(ctx, "instances")
	if err != nil {
		t.Fatalf("inspect migrated instances: %v", err)
	}
	if !columns["backend"] || columns["desired_state"] || columns["observed_state"] {
		t.Fatalf("unexpected migrated instance columns: %+v", columns)
	}
	routesExist, err := store.tableExists(ctx, "instance_routes")
	if err != nil {
		t.Fatalf("inspect legacy routes: %v", err)
	}
	if routesExist {
		t.Fatal("legacy instance_routes table still exists")
	}
	if err := store.validateForeignKeys(ctx); err != nil {
		t.Fatalf("migrated foreign keys are invalid: %v", err)
	}

	if _, err := store.db.ExecContext(ctx, `DELETE FROM instances WHERE id = ?;`, "instance-1"); err != nil {
		t.Fatalf("delete migrated parent: %v", err)
	}
	for _, table := range []string{"vms", "instance_interfaces"} {
		var count int
		if err := store.db.NewSelect().TableExpr(table).ColumnExpr("COUNT(*)").Scan(ctx, &count); err != nil {
			t.Fatalf("count %s rows: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("expected parent delete to cascade to %s, got %d rows", table, count)
		}
	}
}

func TestInstancesAreUniqueBySiteAndNameAcrossBackends(t *testing.T) {
	t.Parallel()

	store, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	now := time.Now().UTC()
	base := instanceRow{
		ID: "vm-1", Name: "worker-1", Backend: SiteBackendVM, TenantID: "tenant-1",
		SiteID: "site-1", InstanceTypeID: "type-1", VPCID: "vpc-1",
		LabelsJSON: `{}`, SSHKeyGroupIDsJSON: `[]`, Status: "ready", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.db.NewInsert().Model(&base).Exec(ctx); err != nil {
		t.Fatalf("insert first instance: %v", err)
	}

	conflict := base
	conflict.ID = "container-1"
	conflict.Backend = SiteBackendContainer
	if _, err := store.db.NewInsert().Model(&conflict).Exec(ctx); err == nil || !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("expected cross-backend site/name uniqueness error, got %v", err)
	}

	otherSite := conflict
	otherSite.ID = "container-2"
	otherSite.SiteID = "site-2"
	if _, err := store.db.NewInsert().Model(&otherSite).Exec(ctx); err != nil {
		t.Fatalf("same name on a different site should be allowed: %v", err)
	}
}

func TestOpenBackfillsContainerBackendRecords(t *testing.T) {
	t.Parallel()

	dbPath := t.TempDir() + "/state.db"
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Now().UTC()
	row := instanceRow{
		ID: "container-1", Name: "worker-1", Backend: SiteBackendContainer,
		TenantID: "tenant-1", SiteID: "site-1", InstanceTypeID: "type-1", VPCID: "vpc-1",
		LabelsJSON: `{}`, SSHKeyGroupIDsJSON: `[]`, Status: "ready", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.db.NewInsert().Model(&row).Exec(context.Background()); err != nil {
		store.Close()
		t.Fatalf("insert container parent: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	store, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	instance, err := store.GetInstance(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("get backfilled container: %v", err)
	}
	if instance.Container == nil || instance.VM != nil {
		t.Fatalf("unexpected backend state: VM=%+v container=%+v", instance.VM, instance.Container)
	}
}

func TestOpenRepairsMissingVMTableForCurrentParentSchema(t *testing.T) {
	t.Parallel()

	dbPath := t.TempDir() + "/state.db"
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if _, err := db.Exec(createInstancesTableSQL); err != nil {
		db.Close()
		t.Fatalf("create parent schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open repaired database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exists, err := store.tableExists(context.Background(), "vms")
	if err != nil {
		t.Fatalf("inspect vms table: %v", err)
	}
	if !exists {
		t.Fatal("vms table was not created")
	}
}
