// SPDX-License-Identifier: Apache-2.0

package public

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

var (
	_ resource.ResourceHandler[v1.NVKECluster]          = v1.Cluster{}
	_ resource.ResourceHandler[v1.Cluster]              = v1.NVKECluster{}
	_ resource.ResourceHandler[v1.ClusterRelease]       = v1.PublicClusterRelease{}
	_ resource.ResourceHandler[v1.PublicClusterRelease] = v1.ClusterRelease{}
	_ resource.ResourceHandler[v1.MachineImage]         = v1.PublicMachineImage{}
	_ resource.ResourceHandler[v1.PublicMachineImage]   = v1.MachineImage{}
)

func TestClusterProjection(t *testing.T) {
	data, err := os.ReadFile("../config/samples/internal_v1_nvkecluster.yaml")
	require.NoError(t, err)
	var internal v1.NVKECluster
	require.NoError(t, yaml.Unmarshal(data, &internal))
	now := metav1.NewTime(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	internal.ObjectMeta = metav1.ObjectMeta{
		Name: "prod-us-west", UID: "cluster-uid", CreationTimestamp: now, DeletionTimestamp: &now,
		Namespace: "private-org", ResourceVersion: "opaque-private-version", Generation: 3,
		Labels: map[string]string{"private": "label"}, Annotations: map[string]string{"private": "annotation"},
		Finalizers: []string{"private-finalizer"}, OwnerReferences: []metav1.OwnerReference{{Name: "private-owner"}},
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "private-manager"}},
	}
	internal.Status = v1.NVKEClusterStatus{
		ID: "observed-cluster", ObservedGeneration: 3,
		Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 3}},
	}
	public, err := internal.Convert()
	require.NoError(t, err)
	require.Equal(t, internal.Spec, public.Spec)
	require.Equal(t, internal.Status, public.Status)
	encoded, err := json.Marshal(public)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(encoded, &object))
	require.Len(t, object, 3)
	require.Equal(t, map[string]any{
		"name": "prod-us-west", "uid": "cluster-uid",
		"creationTimestamp": "2026-10-27T12:00:00Z", "deletionTimestamp": "2026-10-27T12:00:00Z",
	}, object["metadata"])
	restored, err := public.Convert()
	require.NoError(t, err)
	require.Equal(t, internal.Spec, restored.Spec)
	require.Equal(t, internal.Status, restored.Status)
	require.Empty(t, restored.Namespace)
	require.Empty(t, restored.ManagedFields)
	public.Spec.NodePools[0].InstanceType = "changed"
	require.NotEqual(t, internal.Spec.NodePools[0].InstanceType, public.Spec.NodePools[0].InstanceType)
}

func TestPublicWriteOmitsUnsetReadOnlyFields(t *testing.T) {
	for _, value := range []any{v1.Cluster{}, v1.PublicClusterRelease{}, v1.PublicMachineImage{}} {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		var object map[string]any
		require.NoError(t, json.Unmarshal(data, &object))
		require.NotContains(t, object, "status")
		require.Empty(t, object["metadata"])
	}
}
