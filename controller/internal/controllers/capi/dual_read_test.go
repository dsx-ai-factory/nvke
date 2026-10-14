// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"testing"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

var configuredKeys = keys.New("nvke.example.com", "nvke-system")

func topologyNicoMachine() *nicov1.NicoMachine {
	return &nicov1.NicoMachine{Status: nicov1.NicoMachineStatus{
		MachineID: "machine-123",
		SiteID:    "site-123",
		SiteName:  "New York / A",
		VPCID:     "vpc-123",
		VPCName:   "Production VPC",
	}}
}

// A node labeled before the label domain changed carries the legacy keys. The
// controller writes the configured keys and drops the legacy ones, so only one
// domain stays live.
func TestReconcileNodeMetadataMigratesLegacyTopologyLabels(t *testing.T) {
	t.Parallel()

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		"example.com/keep":                   "value",
		testKeys.MachineID.String():          "machine-123",
		testKeys.NodeSiteID.String():         "stale-site",
		testKeys.NodeSiteName.String():       "stale-site-name",
		configuredKeys.NodeVPCID.String():    "stale-vpc",
		testKeys.NodeVPCName.String():        "production-vpc",
		"nke.nvidia.com/unrelated-annotator": "keep",
	}}}

	changed := reconcileNodeMetadata(configuredKeys, node, topologyNicoMachine())

	require.True(t, changed)
	assert.Equal(t, map[string]string{
		"example.com/keep":                   "value",
		"nke.nvidia.com/unrelated-annotator": "keep",
		"nvke.example.com/machine-id":        "machine-123",
		"nvke.example.com/site-id":           "site-123",
		"nvke.example.com/site-name":         "new-york-a",
		"nvke.example.com/vpc-id":            "vpc-123",
		"nvke.example.com/vpc-name":          "production-vpc",
	}, node.Labels)

	// Settled: a second pass has nothing left to change.
	assert.False(t, reconcileNodeMetadata(configuredKeys, node, topologyNicoMachine()))
}

func TestReconcileNodeMetadataClearsEveryFormWhenValueIsEmpty(t *testing.T) {
	t.Parallel()

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		testKeys.NodeSiteID.String():       "legacy-site",
		configuredKeys.NodeSiteID.String(): "configured-site",
	}}}

	changed := reconcileNodeMetadata(configuredKeys, node, &nicov1.NicoMachine{})

	require.True(t, changed)
	assert.Empty(t, node.Labels)
}

func TestMachineHostIDWatchPredicateReadsLegacyAnnotation(t *testing.T) {
	t.Parallel()

	pred := machineHostIDWatchPredicate(configuredKeys)
	legacy := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{testKeys.HostID.String(): nicoSpiffeTestHostID},
	}}

	assert.True(t, pred.Create(event.CreateEvent{Object: legacy}), "a Machine annotated before the change still counts")
	assert.False(t, pred.Create(event.CreateEvent{Object: &clusterv1.Machine{}}))

	// Re-annotating under the configured domain is not a change of host.
	migrated := legacy.DeepCopy()
	migrated.Annotations = map[string]string{configuredKeys.HostID.String(): nicoSpiffeTestHostID}
	assert.False(t, pred.Update(event.UpdateEvent{ObjectOld: legacy, ObjectNew: migrated}))
}

func TestNicoSpiffeControllerReadsLegacyHostIDAnnotation(t *testing.T) {
	t.Parallel()

	// The Machine carries the host ID under the legacy domain only.
	machine := newMachineForNicoSpiffeTest("")
	require.Contains(t, machine.Annotations, "nke.nvidia.com/host-id")
	nicoMachine := newNicoMachineForSpiffeTest(nicov1.NicoMachineStatus{TpmEkPubHash: "hash-current"}, true, false)
	nodes := &nicoSpiffeNodeReaderStub{node: db.Node{SpireAttestationMethod: db.SpireAttestationMethodTPM}}
	controller := newNicoSpiffeControllerForTest(t, nodes, machine, nicoMachine)
	controller.keys = configuredKeys

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: nicoMachine.Namespace, Name: nicoMachine.Name}})
	require.NoError(t, err)

	updated := &clusterv1.Machine{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), types.NamespacedName{Namespace: machine.Namespace, Name: machine.Name}, updated))
	assert.Contains(t, updated.Annotations, configuredKeys.SpireAgentSpiffeIDPath.String(),
		"the path is written under the configured domain")
}
