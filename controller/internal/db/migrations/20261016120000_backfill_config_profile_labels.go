// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/uptrace/bun"
	"go.yaml.in/yaml/v4"
	"k8s.io/apimachinery/pkg/api/validate/content"
)

const (
	// controlPlaneRoleLabel is never backfilled: kubeadm writes it when a node joins as a control plane,
	// and the API rejects it as a node label. Storing it would look like it is owned by the node pool,
	// when it is not
	controlPlaneRoleLabel = "node-role.kubernetes.io/control-plane"
	// backfillLabelsMigration identifies this migration in its logs.
	backfillLabelsMigration = "20261016120000_backfill_config_profile_labels"
)

// Copy config-profile labels into node_pools.node_labels and into each control-plane
// configuration's node_labels, so provisioning can stop reading labels from config profiles.
//
// Labels are read from the profiles mounted on this controller, the same files provisioning read,
// so each site backfills the labels its nodes actually carry. They are not hardcoded because each
// site pins its own profiles image, so a site can be on a different profiles release than the source
// repository, with profiles that were since renamed or not yet added. A pool is backfilled from its own
// config profile, which matches the profile of its current node set, where new nodes join.
//
// Profile labels win on a key collision with labels already stored. This migration ships in the same
// release that starts applying stored labels to nodes, so until it runs no stored label has reached a node:
// the profile value is the one existing nodes carry, and new nodes must match.
// Nothing is applied to existing nodes; they already carry their profile labels.
//
// Note: this migration is the reason Migrate takes profilesDir. Revisit it when profilesDir is removed from the
// controller config or config profiles are no longer mounted. Once every site has applied it, it is safe to delete
// this migration along with the profilesDir argument.
//
//nolint:gochecknoinits // Bun registers migrations at package initialization.
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		profilesDir, _ := ctx.Value(profilesDirKey{}).(string)
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			profiles, err := referencedConfigProfiles(ctx, tx)
			if err != nil {
				return err
			}
			profileLabels, err := loadConfigProfileLabels(profilesDir, profiles)
			if err != nil {
				return err
			}
			if len(profileLabels) == 0 {
				slog.Info("config profile label backfill: no config profiles referenced, nothing to backfill",
					"migration", backfillLabelsMigration)
			}
			for _, profile := range slices.Sorted(maps.Keys(profileLabels)) {
				if len(profileLabels[profile]) == 0 {
					continue
				}
				labels, err := json.Marshal(profileLabels[profile])
				if err != nil {
					return fmt.Errorf("encoding labels for config profile %q: %w", profile, err)
				}
				// For a duplicate key, jsonb a || b keeps b's value, so profile labels win.
				poolResult, err := tx.ExecContext(ctx, `
					UPDATE node_pools
					SET node_labels = node_labels || ?::jsonb
					WHERE nico->>'config_profile' = ?`,
					string(labels), profile,
				)
				if err != nil {
					return fmt.Errorf("backfilling node pool labels for config profile %q: %w", profile, err)
				}
				controlPlaneResult, err := tx.ExecContext(ctx, `
					UPDATE control_plane_configurations
					SET config = jsonb_set(
						config,
						'{node_labels}',
						CASE WHEN jsonb_typeof(config->'node_labels') = 'object' THEN config->'node_labels' ELSE '{}'::jsonb END || ?::jsonb
					)
					WHERE config->'nico'->>'config_profile' = ?`,
					string(labels), profile,
				)
				if err != nil {
					return fmt.Errorf("backfilling control-plane configuration labels for config profile %q: %w", profile, err)
				}
				poolRows, err := poolResult.RowsAffected()
				if err != nil {
					return fmt.Errorf("counting node pools backfilled for config profile %q: %w", profile, err)
				}
				cpRows, err := controlPlaneResult.RowsAffected()
				if err != nil {
					return fmt.Errorf("counting control-plane configurations backfilled for config profile %q: %w", profile, err)
				}
				slog.Info("config profile label backfill: stored config profile labels",
					"migration", backfillLabelsMigration,
					"configProfile", profile,
					"labels", profileLabels[profile],
					"nodePoolsUpdatedCount", poolRows,
					"controlPlaneConfigurationsUpdatedCount", cpRows)
			}
			return nil
		})
	}, func(context.Context, *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}

// referencedConfigProfiles returns the config profiles that node pools and control-plane configurations reference.
// A missing or empty config_profile is not a reference: there is no profile to look up, and no label to backfill.
// COALESCE(..., ”) <> ” filters out both, since a missing key reads as NULL.
func referencedConfigProfiles(ctx context.Context, tx bun.Tx) ([]string, error) {
	var profiles []string
	if err := tx.NewRaw(`
		SELECT nico->>'config_profile' FROM node_pools WHERE COALESCE(nico->>'config_profile', '') <> ''
		UNION
		SELECT config->'nico'->>'config_profile' FROM control_plane_configurations WHERE COALESCE(config->'nico'->>'config_profile', '') <> ''
	`).Scan(ctx, &profiles); err != nil {
		return nil, fmt.Errorf("listing referenced config profiles: %w", err)
	}
	return profiles, nil
}

// loadConfigProfileLabels reads the labels of each profile from profilesDir/configProfiles, falling back
// to configProfiles/deprecated like provisioning does. A profile found in neither has been retired and
// has no labels, which is also how provisioning treats it.
// When profiles are referenced but profilesDir is unset or has no configProfiles directory, it fails, so the
// migration stays pending until the profiles are mounted: once applied it never runs again, and skipping it
// would leave those pools without the labels their nodes carry.
func loadConfigProfileLabels(profilesDir string, profiles []string) (map[string]map[string]string, error) {
	if len(profiles) == 0 {
		return map[string]map[string]string{}, nil
	}
	if profilesDir == "" {
		return nil, fmt.Errorf("config profiles %v are referenced but profilesDir is not set: "+
			"mount the config profiles and set profilesDir so their labels can be backfilled", profiles)
	}
	configProfilesDir := filepath.Join(profilesDir, "configProfiles")
	if _, err := os.Stat(configProfilesDir); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("config profiles %v are referenced but %s does not exist: "+
			"mount the config profiles at profilesDir so their labels can be backfilled", profiles, configProfilesDir)
	} else if err != nil {
		return nil, fmt.Errorf("reading config profiles directory: %w", err)
	}

	profileLabels := make(map[string]map[string]string, len(profiles))
	for _, profile := range profiles {
		labels, found, err := readConfigProfileLabels(configProfilesDir, profile)
		if err != nil {
			return nil, err
		}
		if !found {
			// The profile file is missing in profiles dir, it is considered retired and has no labels to store, similar to provisioning workflow.
			slog.Warn("config profile label backfill: config profile file not found, backfilling no labels for it (profile retired)",
				"migration", backfillLabelsMigration, "configProfile", profile)
		}
		profileLabels[profile] = labels
	}
	return profileLabels, nil
}

// readConfigProfileLabels returns the profile's labels as a map (without the control-plane role label),
// and whether the profile file was found.
func readConfigProfileLabels(configProfilesDir, profile string) (map[string]string, bool, error) {
	for _, path := range []string{
		filepath.Join(configProfilesDir, profile+".yaml"),
		filepath.Join(configProfilesDir, "deprecated", profile+".yaml"),
	} {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("reading config profile %q: %w", profile, err)
		}

		var configProfile struct {
			Labels []string `yaml:"labels"`
		}
		if err := yaml.Unmarshal(raw, &configProfile); err != nil {
			return nil, false, fmt.Errorf("parsing config profile %q: %w", profile, err)
		}

		labels := make(map[string]string, len(configProfile.Labels))
		for _, label := range configProfile.Labels {
			key, value, ok := strings.Cut(label, "=")
			if !ok {
				return nil, false, fmt.Errorf("config profile %q: label %q is not key=value", profile, label)
			}
			if errs := content.IsLabelKey(key); len(errs) > 0 {
				return nil, false, fmt.Errorf("config profile %q: invalid label key %q: %s", profile, key, strings.Join(errs, "; "))
			}
			if errs := content.IsLabelValue(value); len(errs) > 0 {
				return nil, false, fmt.Errorf("config profile %q: invalid value for label %q: %s", profile, key, strings.Join(errs, "; "))
			}
			if key == controlPlaneRoleLabel {
				continue
			}
			labels[key] = value
		}
		return labels, true, nil
	}
	return map[string]string{}, false, nil
}
