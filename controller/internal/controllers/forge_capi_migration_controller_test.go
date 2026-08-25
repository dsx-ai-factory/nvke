// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"
	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestForgeCAPIMigrationControllerReconcileKeepsNicoProviderPayload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	node := adoptedForgeNode()
	preference := []uuid.UUID{uuid.New(), uuid.New()}
	cluster := db.Cluster{
		ID:   node.ClusterID,
		Name: "test-cluster",
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{
		OrgID:  "org-1",
		SiteID: uuid.New(),
		VpcID:  uuid.New(),
	})
	cluster.ControlPlaneConfig.ControlPlaneConfigSpec.SetNico(db.NicoControlPlaneProvider{
		InstanceTypePreference: preference,
	})

	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodes", mock.Anything, db.NodeFilters{ClusterID: &cluster.ID}).
		Return([]db.Node{node}, nil).
		Once()

	controller := NewForgeCAPIMigrationController(
		slog.Default(),
		nil,
		fake.NewClientBuilder().
			WithScheme(legacyMigrationScheme(t)).
			WithObjects(adoptedCAPIObjects(cluster, node)...).
			Build(),
		&unexpectedMachineAdopter{t: t},
	)

	require.NoError(t, controller.reconcileCluster(ctx, mockTx, cluster, nil))
	assert.Equal(t, db.ProviderTypeNico, cluster.ProviderType)
	require.NotNil(t, cluster.ControlPlaneConfig.Nico)
	assert.Equal(t, preference, cluster.ControlPlaneConfig.Nico.InstanceTypePreference)
	mockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNodePoolCAPIProvider", mock.Anything, mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNodeSetCAPIProvider", mock.Anything, mock.Anything, mock.Anything)
	mockTx.AssertExpectations(t)
}

func TestForgeCAPIMigrationControllerUnpausePreservesCopiedClusterSpec(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cluster := db.Cluster{Name: "copied-cluster"}

	clusterObj := &clusterv1.Cluster{
		TypeMeta: metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: constants.NamespaceNKESystem,
			Name:      cluster.Name,
			Labels: map[string]string{
				constants.LabelKeyNkeManagedBy: constants.LabelValueNkeManagedBy,
				"example.com/keep":             "yes",
			},
			Annotations: map[string]string{
				clusterv1.PausedAnnotation: "true",
			},
		},
		Spec: clusterv1.ClusterSpec{
			Paused: ptr.To(true),
			ClusterNetwork: clusterv1.ClusterNetwork{
				Pods: clusterv1.NetworkRanges{CIDRBlocks: []string{"10.244.0.0/16"}},
			},
			ControlPlaneEndpoint: clusterv1.APIEndpoint{Host: "10.0.0.10", Port: 6443},
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: nicov1.GroupVersion.Group,
				Kind:     "NicoCluster",
				Name:     cluster.Name,
			},
		},
	}
	nicoCluster := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": nicov1.GroupVersion.String(),
			"kind":       "NicoCluster",
			"metadata": map[string]any{
				"namespace": constants.NamespaceNKESystem,
				"name":      cluster.Name,
				"annotations": map[string]any{
					clusterv1.PausedAnnotation: "true",
				},
			},
			"spec": map[string]any{
				"siteID": "site-keep",
			},
		},
	}
	nicoClusterCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nicoclusters.infrastructure.cluster.x-k8s.io",
			Labels: map[string]string{
				"cluster.x-k8s.io/v1beta2": nicov1.GroupVersion.Version,
			},
		},
	}

	controller := NewForgeCAPIMigrationController(
		slog.Default(),
		nil,
		fake.NewClientBuilder().
			WithScheme(legacyMigrationScheme(t)).
			WithObjects(nicoClusterCRD, clusterObj, nicoCluster).
			Build(),
		&unexpectedMachineAdopter{t: t},
	)

	require.NoError(t, controller.reconcileCAPICluster(ctx, cluster))

	got := &clusterv1.Cluster{}
	require.NoError(t, controller.k8sClient.Get(ctx, ctrlclient.ObjectKey{
		Namespace: constants.NamespaceNKESystem,
		Name:      cluster.Name,
	}, got))
	require.NotNil(t, got.Spec.Paused)
	assert.False(t, *got.Spec.Paused)
	_, paused := got.Annotations[clusterv1.PausedAnnotation]
	assert.False(t, paused)
	assert.Equal(t, "yes", got.Labels["example.com/keep"])
	assert.Equal(t, []string{"10.244.0.0/16"}, got.Spec.ClusterNetwork.Pods.CIDRBlocks)
	assert.Equal(t, "10.0.0.10", got.Spec.ControlPlaneEndpoint.Host)
	assert.Equal(t, int32(6443), got.Spec.ControlPlaneEndpoint.Port)

	gotInfra := &unstructured.Unstructured{}
	gotInfra.SetAPIVersion(nicov1.GroupVersion.String())
	gotInfra.SetKind("NicoCluster")
	require.NoError(t, controller.k8sClient.Get(ctx, ctrlclient.ObjectKey{
		Namespace: constants.NamespaceNKESystem,
		Name:      cluster.Name,
	}, gotInfra))
	_, paused = gotInfra.GetAnnotations()[clusterv1.PausedAnnotation]
	assert.False(t, paused)
	siteID, found, err := unstructured.NestedString(gotInfra.Object, "spec", "siteID")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "site-keep", siteID)
}

func TestForgeCAPIMigrationControllerMissingAdoptedMachineObjects(t *testing.T) {
	t.Parallel()

	cluster := db.Cluster{Name: "test-cluster"}
	existingNode := adoptedForgeNode()
	existingNode.Name = "existing-node"
	inspectionFailedNode := adoptedForgeNode()
	inspectionFailedNode.Name = "inspection-failed-node"
	missingNode := adoptedForgeNode()
	missingNode.Name = "missing-node"

	existingObjects := adoptedCAPIObjects(cluster, existingNode)
	existingObjects = append(existingObjects, adoptedInspectionFailedCAPIObjects(inspectionFailedNode)...)
	controller := NewForgeCAPIMigrationController(
		slog.Default(),
		nil,
		fake.NewClientBuilder().
			WithScheme(legacyMigrationScheme(t)).
			WithObjects(existingObjects...).
			Build(),
		nil,
	)

	missing, err := controller.missingAdoptedMachineObjects(
		context.Background(),
		[]db.Node{existingNode, inspectionFailedNode, missingNode},
	)
	require.NoError(t, err)
	require.Len(t, missing, 2)
	assert.Equal(t, inspectionFailedNode.Name, missing[0].Name)
	assert.Equal(t, missingNode.Name, missing[1].Name)
}

type unexpectedMachineAdopter struct {
	t *testing.T
}

func (a *unexpectedMachineAdopter) LookupForgeInstanceForAdoption(
	context.Context,
	db.Cluster,
	db.Node,
) (*forgesdk.Instance, error) {
	a.t.Helper()
	a.t.Error("LookupForgeInstanceForAdoption should not be called for existing CAPI objects")
	return nil, nil //nolint:nilnil // test
}

func (a *unexpectedMachineAdopter) ApplyAdoptedForgeCAPIMachine(
	context.Context,
	db.Cluster,
	db.Node,
	db.NodeSet,
	db.NodePool,
	*forgesdk.Instance,
) error {
	a.t.Helper()
	a.t.Error("ApplyAdoptedForgeCAPIMachine should not be called for existing CAPI objects")
	return nil
}

func TestCountOrphanedLegacyNamespacesSkipsClusterSecrets(t *testing.T) {
	t.Parallel()

	namespaces := []corev1.Namespace{
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-secrets"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "nke-system"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-gone"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-live"}},
	}
	clusters := []db.Cluster{{Name: "live"}}

	assert.Equal(t, 1, countOrphanedLegacyNamespaces(namespaces, clusters))
}

func TestCAPIAdoptionPredicates(t *testing.T) {
	t.Parallel()

	provisioned := adoptedForgeNode()
	unprovisioned := adoptedForgeNode()
	unprovisioned.Conditions.Provisioned = db.NewCondition(false, "Not yet provisioned")
	deleting := adoptedForgeNode()
	deleting.Conditions.Deleting = db.NewCondition(true, "Deleting")
	noForgeID := adoptedForgeNode()
	noForgeID.Nico = &db.NicoNodeProvider{}
	noPool := adoptedForgeNode()
	noPool.NodePoolID = uuid.Nil
	noSet := adoptedForgeNode()
	noSet.NodeSetID = uuid.Nil

	assert.True(t, expectsCAPIAdoption(provisioned))
	assert.True(t, eligibleForCAPIAdoption(provisioned))

	assert.True(t, expectsCAPIAdoption(unprovisioned))
	assert.False(t, eligibleForCAPIAdoption(unprovisioned))

	assert.False(t, expectsCAPIAdoption(deleting))
	assert.False(t, eligibleForCAPIAdoption(deleting))

	assert.False(t, expectsCAPIAdoption(noForgeID))
	assert.False(t, eligibleForCAPIAdoption(noForgeID))

	assert.True(t, expectsCAPIAdoption(noPool))
	assert.False(t, eligibleForCAPIAdoption(noPool))

	assert.True(t, expectsCAPIAdoption(noSet))
	assert.False(t, eligibleForCAPIAdoption(noSet))
}

func adoptedForgeNode() db.Node {
	node := db.Node{
		ID:         uuid.New(),
		ClusterID:  uuid.New(),
		NodePoolID: uuid.New(),
		NodeSetID:  uuid.New(),
		Name:       "test-node",
		Conditions: db.NodeConditions{
			Provisioned: db.NewCondition(true, "Provisioned"),
		},
	}
	node.SetNico(db.NicoNodeProvider{ForgeInstanceID: "instance-1"})
	return node
}

func adoptedCAPIObjects(cluster db.Cluster, node db.Node) []ctrlclient.Object {
	clusterObj := &clusterv1.Cluster{
		TypeMeta: metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: constants.NamespaceNKESystem,
			Name:      cluster.Name,
		},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: nicov1.GroupVersion.Group,
				Kind:     "NicoCluster",
				Name:     cluster.Name,
			},
		},
	}
	nicoCluster := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": nicov1.GroupVersion.String(),
			"kind":       "NicoCluster",
			"metadata": map[string]any{
				"namespace": constants.NamespaceNKESystem,
				"name":      cluster.Name,
			},
		},
	}
	machine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: constants.NamespaceNKESystem,
			Name:      node.Name,
		},
	}
	nicoMachine := &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{APIVersion: nicov1.GroupVersion.String(), Kind: "NicoMachine"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: constants.NamespaceNKESystem,
			Name:      node.Name,
		},
	}
	nicoClusterCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nicoclusters.infrastructure.cluster.x-k8s.io",
			Labels: map[string]string{
				"cluster.x-k8s.io/v1beta2": nicov1.GroupVersion.Version,
			},
		},
	}
	return []ctrlclient.Object{nicoClusterCRD, clusterObj, nicoCluster, machine, nicoMachine}
}

func adoptedInspectionFailedCAPIObjects(node db.Node) []ctrlclient.Object {
	machine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: constants.NamespaceNKESystem,
			Name:      node.Name,
		},
	}
	nicoMachine := &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{APIVersion: nicov1.GroupVersion.String(), Kind: "NicoMachine"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: constants.NamespaceNKESystem,
			Name:      node.Name,
		},
		Spec: nicov1.NicoMachineSpec{
			ProviderID: "nico://" + node.Nico.ForgeInstanceID,
		},
		Status: nicov1.NicoMachineStatus{
			Conditions: []metav1.Condition{{
				Type:   clusterv1.ReadyCondition,
				Status: metav1.ConditionFalse,
				Reason: clusterv1.InspectionFailedReason,
			}},
		},
	}
	return []ctrlclient.Object{machine, nicoMachine}
}
