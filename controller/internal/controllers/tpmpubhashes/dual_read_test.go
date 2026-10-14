// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tpmpubhashes

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

// A site that changes the label domain keeps ClusterStaticEntries labeled under
// the old domain until the Machine controller re-applies them, so the pubhash
// ConfigMap has to include entries carrying either form.
func TestController_ReconcileReadsLegacyAndConfiguredManagedByLabels(t *testing.T) {
	configured := keys.New("nvke.example.com", "")

	legacyEntry := newTPMClusterStaticEntryForTest("host-legacy", constants.FieldManagerMachineSpireEntry, "hash-legacy")
	currentEntry := newTPMClusterStaticEntryForTest("host-current", constants.FieldManagerMachineSpireEntry, "hash-current")
	currentEntry.Labels = map[string]string{configured.ManagedBy.String(): constants.FieldManagerMachineSpireEntry}
	unmanaged := newTPMClusterStaticEntryForTest("host-unmanaged", "other-manager", "hash-unmanaged")

	controller := newControllerForTest(t, legacyEntry, currentEntry, unmanaged)
	controller.keys = configured

	_, err := controller.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)

	want := configured.ManagedBy.String() + "=" + constants.FieldManagerMachineSpireEntry
	assertTPMPubhashesConfigMapData(t, controller, map[string]string{
		"hash-legacy":  want,
		"hash-current": want,
	})
}
