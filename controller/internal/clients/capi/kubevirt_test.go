// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const kubeVirtTestAPIVersion = "infrastructure.cluster.x-k8s.io/v1alpha1"

func kubeVirtMachineTemplateJSON(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"apiVersion": kubeVirtTestAPIVersion,
		"kind":       kubeVirtMachineTemplateKind,
		"metadata":   map[string]any{"name": "control-plane"},
		"spec":       map[string]any{"template": map[string]any{"spec": map[string]any{}}},
	})
	require.NoError(t, err)
	return raw
}

func kubeVirtCluster(t *testing.T, secretRef map[string]any) db.Cluster {
	t.Helper()
	spec := map[string]any{}
	if secretRef != nil {
		spec["infraClusterSecretRef"] = secretRef
	}
	raw, err := json.Marshal(map[string]any{
		"apiVersion": kubeVirtTestAPIVersion,
		"kind":       kubeVirtClusterKind,
		"metadata":   map[string]any{"name": "cluster-a"},
		"spec":       spec,
	})
	require.NoError(t, err)
	return db.Cluster{Name: "cluster-a", Capi: &db.CAPIClusterProvider{InfraCluster: raw}}
}

func TestClient_ValidateNodeAgentProfile_KubeVirt(t *testing.T) {
	t.Parallel()

	kubeVirtPool := db.NodePool{Capi: &db.CAPINodePoolProvider{InfraMachineTemplate: kubeVirtMachineTemplateJSON(t)}}
	dockerTemplate, err := json.Marshal(map[string]any{"apiVersion": kubeVirtTestAPIVersion, "kind": "DockerMachineTemplate"})
	require.NoError(t, err)
	dockerPool := db.NodePool{Capi: &db.CAPINodePoolProvider{InfraMachineTemplate: dockerTemplate}}

	tests := map[string]struct {
		profile  db.Node
		nodePool db.NodePool
		wantErr  error
	}{
		"KubeVirt with SPIFFE and no external mTLS": {
			profile:  db.Node{SpiffeEnabled: true, NodeAgentTLSJWTEnabled: true},
			nodePool: kubeVirtPool,
			wantErr:  errKubeVirtRequiresTLSJWT,
		},
		"KubeVirt with SPIFFE and no TLS/JWT": {
			profile:  db.Node{SpiffeEnabled: true, NodeAgentExternalMTLSEnabled: true},
			nodePool: kubeVirtPool,
			wantErr:  errKubeVirtRequiresTLSJWT,
		},
		"KubeVirt with TLS/JWT and external mTLS": {
			profile:  db.Node{SpiffeEnabled: true, NodeAgentExternalMTLSEnabled: true, NodeAgentTLSJWTEnabled: true},
			nodePool: kubeVirtPool,
		},
		"KubeVirt without SPIFFE": {
			profile:  db.Node{},
			nodePool: kubeVirtPool,
		},
		"other template with SPIFFE and no external mTLS": {
			profile:  db.Node{SpiffeEnabled: true},
			nodePool: dockerPool,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := New().ValidateNodeAgentProfile(tt.profile, tt.nodePool)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func newKubeVirtCreateMachineFixture(t *testing.T) (client.Client, shared.MachineCreateDeps, db.NodeSet, *[]bool, *int) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	nodeSet := db.NodeSet{KubernetesVersion: "1.32.1"}
	nodeSet.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: kubeVirtMachineTemplateJSON(t)})

	var virtualMachineArgs []bool
	joinTokenCalls := 0
	deps := shared.MachineCreateDeps{
		K8sClient: k8sClient, Namespace: "nke-system", FieldManagerName: "test-controller",
		CreateSpireJoinToken: func(context.Context, db.Node) (string, string, error) {
			joinTokenCalls++
			return "join-token", "/join-token-spire-agent/host-id/x", nil
		},
		NodeAgentBootstrapFiles: func(node db.Node) ([]db.CloudInitFile, error) {
			virtualMachineArgs = append(virtualMachineArgs, node.IsKubeVirtVM())
			return nil, nil
		},
		GetTrustDomainBundle: func(context.Context, config.Config) (string, string, error) {
			return "", "", errors.New("a KubeVirt VM has no SPIRE agent")
		},
	}
	return k8sClient, deps, nodeSet, &virtualMachineArgs, &joinTokenCalls
}

func TestClient_CreateMachine_KubeVirtVM(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	k8sClient, deps, nodeSet, virtualMachineArgs, joinTokenCalls := newKubeVirtCreateMachineFixture(t)

	node := db.Node{
		ID:                           uuid.New(),
		Name:                         "cp-0",
		SpiffeEnabled:                true,
		SpireAttestationMethod:       db.SpireAttestationMethodJoinToken,
		NodeAgentExternalMTLSEnabled: true,
		NodeAgentTLSJWTEnabled:       true,
	}
	require.NoError(t, New().CreateMachine(ctx, deps, node, nodeSet, db.NodePool{ControlPlane: true}, db.Cluster{Name: "cluster-a"}))

	assert.Equal(t, []bool{true}, *virtualMachineArgs)
	assert.Zero(t, *joinTokenCalls)
	machine := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Name: node.Name, Namespace: deps.Namespace}, machine))
	assert.True(t, IsKubeVirtMachineRef(machine.Spec.InfrastructureRef))
	assert.Empty(t, machine.Annotations[keys.MachineSpireAgentSpiffeIDPath.String()])
}

func TestClient_CreateMachine_KubeVirtVMRequiresTLSJWT(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	k8sClient, deps, nodeSet, _, _ := newKubeVirtCreateMachineFixture(t)

	node := db.Node{ID: uuid.New(), Name: "cp-0", SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodJoinToken}
	err := New().CreateMachine(ctx, deps, node, nodeSet, db.NodePool{ControlPlane: true}, db.Cluster{Name: "cluster-a"})
	require.ErrorIs(t, err, shared.ErrUnsupportedMachineConfig)
	require.ErrorIs(t, err, errKubeVirtRequiresTLSJWT)

	machines := &clusterv1.MachineList{}
	require.NoError(t, k8sClient.List(ctx, machines))
	assert.Empty(t, machines.Items)
}

func kubeVirtEndpointScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spirev1alpha1.AddToScheme(scheme))
	return scheme
}

func kubeVirtEndpointConfig() config.Config {
	return config.Config{NodeAgent: config.NodeAgentConfig{
		Port:     "15005",
		TLSJWT:   config.TLSJWTConfig{Enabled: true, SubjectTenant: "nke", SubjectService: "node-agent"},
		KubeVirt: config.KubeVirtConfig{SPIFFEIDClassName: "dsx-identity", AddressPool: "pool-a"},
	}}
}

func TestKubeVirtAgentEndpoint_ManagementCluster(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgmt := fake.NewClientBuilder().WithScheme(kubeVirtEndpointScheme(t)).WithStatusSubresource(&corev1.Service{}).Build()
	endpoint := KubeVirtAgentEndpoint{
		ManagementClient: mgmt, Namespace: "nke-system", Config: kubeVirtEndpointConfig(), FieldManagerName: "test-controller",
	}
	cluster := kubeVirtCluster(t, nil)
	node := db.Node{ID: uuid.MustParse("11111111-2222-4333-8444-555555555555"), Name: "cp-0", NodeAgentTLSJWTEnabled: true}
	name := "nvke-node-agent-" + node.ID.String()

	address, err := endpoint.Ensure(ctx, cluster, node)
	require.NoError(t, err)
	assert.Empty(t, address, "no address before MetalLB assigns one")

	service := &corev1.Service{}
	require.NoError(t, mgmt.Get(ctx, client.ObjectKey{Namespace: "nke-system", Name: name}, service))
	assert.Equal(t, corev1.ServiceTypeLoadBalancer, service.Spec.Type)
	assert.Equal(t, corev1.ServiceExternalTrafficPolicyLocal, service.Spec.ExternalTrafficPolicy)
	assert.Equal(t, "pool-a", service.Annotations[metalLBAddressPoolAnnotation])
	assert.Equal(t, map[string]string{
		kubeVirtMachineNameLabel:      "cp-0",
		kubeVirtMachineNamespaceLabel: "nke-system",
	}, service.Spec.Selector)
	require.Len(t, service.Spec.Ports, 1)
	assert.Equal(t, int32(15005), service.Spec.Ports[0].Port)

	registration := &spirev1alpha1.ClusterSPIFFEID{}
	require.NoError(t, mgmt.Get(ctx, client.ObjectKey{Name: name}, registration))
	assert.Equal(t, "spiffe://{{ .TrustDomain }}/tenant/nke/service/node-agent/vm/"+node.ID.String(), registration.Spec.SPIFFEIDTemplate)
	assert.Equal(t, "dsx-identity", registration.Spec.ClassName)
	assert.Equal(t, kubeVirtSVIDHint, registration.Spec.Hint)
	assert.Equal(t, map[string]string{namespaceNameLabel: "nke-system"}, registration.Spec.NamespaceSelector.MatchLabels)
	assert.Equal(t, service.Spec.Selector, registration.Spec.PodSelector.MatchLabels)

	service.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.20"}}
	require.NoError(t, mgmt.Status().Update(ctx, service))
	address, err = endpoint.Ensure(ctx, cluster, node)
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.20", address)

	require.NoError(t, endpoint.Delete(ctx, cluster, node))
	require.True(t, apierrors.IsNotFound(mgmt.Get(ctx, client.ObjectKey{Namespace: "nke-system", Name: name}, &corev1.Service{})))
	require.True(t, apierrors.IsNotFound(mgmt.Get(ctx, client.ObjectKey{Name: name}, &spirev1alpha1.ClusterSPIFFEID{})))
	require.NoError(t, endpoint.Delete(ctx, cluster, node), "deleting again is not an error")
}

func TestKubeVirtAgentEndpoint_WithoutTLSJWTNeedsNoClusterSPIFFEID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	mgmt := fake.NewClientBuilder().WithScheme(scheme).Build()
	endpoint := KubeVirtAgentEndpoint{
		ManagementClient: mgmt, Namespace: "nke-system", Config: kubeVirtEndpointConfig(), FieldManagerName: "test-controller",
	}
	cluster := kubeVirtCluster(t, nil)
	node := db.Node{ID: uuid.New(), Name: "cp-0"}

	_, err := endpoint.Ensure(ctx, cluster, node)
	require.NoError(t, err)
	require.NoError(t, endpoint.Delete(ctx, cluster, node))
}

func TestKubeVirtAgentEndpoint_InfraClusterSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kubeconfig := []byte(`apiVersion: v1
kind: Config
clusters:
- name: infra
  cluster:
    server: https://infra.example.test
contexts:
- name: infra
  context:
    cluster: infra
    user: capk
    namespace: from-kubeconfig
current-context: infra
users:
- name: capk
  user:
    token: test
`)
	tests := map[string]struct {
		secretData    map[string][]byte
		wantNamespace string
	}{
		"namespace from the kubeconfig": {
			secretData:    map[string][]byte{"kubeconfig": kubeconfig},
			wantNamespace: "from-kubeconfig",
		},
		"namespace key overrides the kubeconfig": {
			secretData:    map[string][]byte{"kubeconfig": kubeconfig, "namespace": []byte(" vms\n")},
			wantNamespace: "vms",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "infra-kubeconfig", Namespace: "nke-system"}, Data: tt.secretData}
			mgmt := fake.NewClientBuilder().WithScheme(kubeVirtEndpointScheme(t)).WithObjects(secret).Build()
			infra := fake.NewClientBuilder().WithScheme(kubeVirtEndpointScheme(t)).Build()
			var gotHost string
			endpoint := KubeVirtAgentEndpoint{
				ManagementClient: mgmt, Namespace: "nke-system", Config: kubeVirtEndpointConfig(), FieldManagerName: "test-controller",
				NewClient: func(restConfig *rest.Config, _ client.Options) (client.Client, error) {
					gotHost = restConfig.Host
					return infra, nil
				},
			}
			node := db.Node{ID: uuid.New(), Name: "cp-0"}

			_, err := endpoint.Ensure(ctx, kubeVirtCluster(t, map[string]any{"name": "infra-kubeconfig"}), node)
			require.NoError(t, err)
			assert.Equal(t, "https://infra.example.test", gotHost)
			require.NoError(t, infra.Get(ctx, client.ObjectKey{Namespace: tt.wantNamespace, Name: "nvke-node-agent-" + node.ID.String()}, &corev1.Service{}))
			require.True(t, apierrors.IsNotFound(infra.Get(ctx, client.ObjectKey{Name: "nvke-node-agent-" + node.ID.String()}, &spirev1alpha1.ClusterSPIFFEID{})),
				"no registration without TLS/JWT")
			services := &corev1.ServiceList{}
			require.NoError(t, mgmt.List(ctx, services))
			assert.Empty(t, services.Items)
		})
	}
}

func TestKubeVirtAgentEndpoint_RejectsOtherInfraCluster(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(map[string]any{"apiVersion": kubeVirtTestAPIVersion, "kind": "DockerCluster"})
	require.NoError(t, err)
	endpoint := KubeVirtAgentEndpoint{ManagementClient: fake.NewClientBuilder().Build(), Namespace: "nke-system", Config: kubeVirtEndpointConfig()}
	_, err = endpoint.Ensure(context.Background(), db.Cluster{Capi: &db.CAPIClusterProvider{InfraCluster: raw}}, db.Node{ID: uuid.New()})
	require.ErrorContains(t, err, "need a KubevirtCluster infra cluster")
}
