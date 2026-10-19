// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

//nolint:gochecknoglobals // migrations are global
var migrations = migrate.NewMigrations()

// profilesDirKey carries the controller's profiles directory to migrations that read config profiles
type profilesDirKey struct{}

// Migrate applies pending migrations. profilesDir is the controller's profilesDir setting, read by
// migrations that backfill data from config profiles.
func Migrate(ctx context.Context, db *bun.DB, profilesDir string) error {
	// Note: profilesDir is read by 20261016120000_backfill_config_profile_labels.
	ctx = context.WithValue(ctx, profilesDirKey{}, profilesDir)

	// By default, When a migration fails, Bun still marks the migration as applied.
	// Marking the failed migration as applied is problematic when executing migrations as a part of
	// kubernetes deployment as k8s will restart the failed container,
	// and the second try will succeed, because the bad migration has already been marked as applied.
	// This makes it difficult to detect that a migration had failed and most often results in runtime errors
	// only observed when we try to run queries.
	// See: https://bun.uptrace.dev/guide/migrations.html#migration-names
	migrator := migrate.NewMigrator(db, migrations, migrate.WithMarkAppliedOnSuccess(true))

	err := migrator.Init(ctx)
	if err != nil {
		return fmt.Errorf("failed to init migrations: %w", err)
	}

	if err := migrator.Lock(ctx); err != nil {
		return fmt.Errorf("failed to lock migrations: %w", err)
	}
	defer migrator.Unlock(ctx) //nolint:errcheck // nothing we can really do if this fails

	_, err = migrator.Migrate(ctx)
	return err
}
