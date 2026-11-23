// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

const adoptTestManagementNamespace = "nvke-system"

func adoptTestNode(name, ip string, controlPlane bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if controlPlane {
		n.Labels[controlPlaneRoleLabel] = ""
	}
	if ip != "" {
		n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeHostName, Address: name}, {Type: corev1.NodeInternalIP, Address: ip}}
	}
	n.Status.NodeInfo.KubeletVersion = "v1.36.4"
	return n
}

func adoptTestKubeadmConfig(clusterConfiguration string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: kubeadmConfigMapName, Namespace: metav1.NamespaceSystem},
		Data:       map[string]string{"ClusterConfiguration": clusterConfiguration},
	}
}

const adoptTestClusterConfiguration = `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.36.4
networking:
  podSubnet: 10.233.64.0/18
  serviceSubnet: 10.233.0.0/18
`

// adoptTestCluster is a running two-node control plane with a worker.
func adoptTestCluster() []ctrlclient.Object {
	return []ctrlclient.Object{
		adoptTestNode("cp-2", "10.0.0.2", true), adoptTestNode("cp-1", "10.0.0.1", true), adoptTestNode("worker-1", "10.0.0.9", false),
		adoptTestKubeadmConfig(adoptTestClusterConfiguration),
	}
}

func buildFake(objs ...ctrlclient.Object) ctrlclient.Client {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		b = b.WithObjects(o.DeepCopyObject().(ctrlclient.Object))
	}
	return b.Build()
}

// adoptTestRequest is an adopt body for adoptTestCluster.
func adoptTestRequest() *api.AdoptClusterRequest {
	return &api.AdoptClusterRequest{
		Name:          "site",
		Nico:          api.NicoClusterProvider{SiteID: uuid.New(), VpcID: uuid.New()},
		EtcdEncrypted: true,
	}
}

func TestReadAdoption(t *testing.T) {
	existingCAPICluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: adoptTestManagementNamespace}}
	withCluster := func(objs ...ctrlclient.Object) []ctrlclient.Object { return append(adoptTestCluster(), objs...) }
	withKubeadmConfig := func(clusterConfiguration string) []ctrlclient.Object {
		return []ctrlclient.Object{adoptTestNode("cp-1", "10.0.0.1", true), adoptTestKubeadmConfig(clusterConfiguration)}
	}
	behindNode := adoptTestNode("cp-3", "10.0.0.3", true)
	behindNode.Status.NodeInfo.KubeletVersion = "v1.35.9"

	tests := []struct {
		name    string
		cluster []ctrlclient.Object
		mutate  func(*api.AdoptClusterRequest)
		wantBad string
	}{
		{name: "every control-plane node is adopted"},
		{name: "name too long", mutate: func(r *api.AdoptClusterRequest) { r.Name = strings.Repeat("a", 80) }, wantBad: "cluster name"},
		{name: "no NICo site", mutate: func(r *api.AdoptClusterRequest) { r.Nico.SiteID = uuid.Nil }, wantBad: "nico.siteID and nico.vpcID are required"},
		{name: "no NICo VPC", mutate: func(r *api.AdoptClusterRequest) { r.Nico.VpcID = uuid.Nil }, wantBad: "nico.siteID and nico.vpcID are required"},
		{name: "invalid CNI version", mutate: func(r *api.AdoptClusterRequest) { r.CniVersion = ptr.To("v2.4") }, wantBad: "invalid CNI version"},
		{name: "a Cluster API Cluster already has the name", cluster: withCluster(existingCAPICluster), wantBad: "Cluster API Cluster nvke-system/site already exists"},
		{name: "no control-plane node", cluster: []ctrlclient.Object{adoptTestNode("worker-1", "10.0.0.9", false), adoptTestKubeadmConfig(adoptTestClusterConfiguration)}, wantBad: "no Node labeled"},
		{name: "control-plane node without an InternalIP", cluster: withCluster(adoptTestNode("cp-noip", "", true)), wantBad: "cp-noip has no InternalIP"},
		{name: "a control-plane kubelet behind kubeadm-config", cluster: withCluster(behindNode), wantBad: "node cp-3 runs kubelet 1.35.9"},
		{name: "no kubeadm-config", cluster: []ctrlclient.Object{adoptTestNode("cp-1", "10.0.0.1", true)}, wantBad: "kube-system/kubeadm-config not found"},
		{
			name:    "a pre-release Kubernetes version",
			cluster: withKubeadmConfig("kubernetesVersion: v1.36.0-rc.1\nnetworking:\n  podSubnet: 10.233.64.0/18\n  serviceSubnet: 10.233.0.0/18\n"),
			wantBad: `kubernetesVersion "v1.36.0-rc.1" is not a release version`,
		},
		{
			name:    "no Kubernetes version",
			cluster: withKubeadmConfig("networking:\n  podSubnet: 10.233.64.0/18\n"),
			wantBad: "is not a release version",
		},
		{
			name:    "no pod subnet",
			cluster: withKubeadmConfig("kubernetesVersion: v1.36.4\nnetworking:\n  serviceSubnet: 10.233.0.0/18\n"),
			wantBad: "no networking.podSubnet",
		},
		{
			name:    "a pod subnet NVKE does not accept",
			cluster: withKubeadmConfig("kubernetesVersion: v1.36.4\nnetworking:\n  podSubnet: 8.8.0.0/16\n  serviceSubnet: 10.233.0.0/18\n"),
			wantBad: "kube-system/kubeadm-config: network.podCIDR",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cluster := tc.cluster
			if cluster == nil {
				cluster = adoptTestCluster()
			}
			store := &testutil.MockDB{MockTx: &testutil.MockTx{}}
			store.On("ListClusters", mock.Anything, mock.Anything).Return([]db.Cluster{}, nil).Maybe()
			s := &clusterService{logger: slog.Default(), db: store, k8sClient: buildFake(cluster...), managementNamespace: adoptTestManagementNamespace}
			req := adoptTestRequest()
			if tc.mutate != nil {
				tc.mutate(req)
			}

			body, adopt, err := s.readAdoption(context.Background(), req)

			if tc.wantBad != "" {
				var bad *badRequestError
				require.ErrorAs(t, err, &bad)
				assert.Contains(t, bad.Error(), tc.wantBad)
				assert.Nil(t, body)
				assert.Nil(t, adopt)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []adoptedNode{{Name: "cp-1", IP: "10.0.0.1", KubeletVersion: "1.36.4"}, {Name: "cp-2", IP: "10.0.0.2", KubeletVersion: "1.36.4"}}, adopt.nodes)
			assert.Equal(t, "1.36.4", body.ControlPlaneKubernetesVersion)
			assert.Equal(t, &api.ClusterNetworkCreateConfig{PodCIDR: "10.233.64.0/18", ServiceCIDR: "10.233.0.0/18"}, body.Network)
			assert.Equal(t, "10.233.64.0/18", adopt.network.podCIDR)
			assert.Equal(t, 2, body.ControlPlaneConfig.DesiredCount)
			assert.Nil(t, body.ControlPlaneConfig.Nico, "adopt records no control-plane machine settings")
			assert.Equal(t, ptr.To(false), body.EnableCPNodeRecycling)
			assert.Equal(t, ptr.To(true), body.ControlPlaneConfig.AutoScaleDownDisabled)
			assert.Equal(t, ptr.To(true), body.ControlPlaneConfig.NodeRebalanceDisabled)
			assert.Equal(t, req.Nico, *body.Nico)
			assert.Empty(t, body.ControlPlaneVIP)
			assert.Equal(t, api.CreateClusterRequestCniPlugin("ovn-kubernetes"), body.CniPlugin, "an omitted CNI is recorded as the default")
		})
	}
}

// newAdoptTestService serves adoption against adoptTestCluster.
func newAdoptTestService(store *testutil.MockDB) *clusterService {
	s := NewClusterService(slog.Default(), store, NewNodePoolService(slog.Default(), store, config.TeleportConfig{}, nil), config.TeleportConfig{}, false, nil, true, "")
	s.k8sClient, s.managementNamespace = buildFake(adoptTestCluster()...), adoptTestManagementNamespace
	s.adoptionEnabled, s.oidcAuthEnabled = true, true
	return s
}

// TestAdoptClusterRecordsAdoptedNodes pins what adopt writes: an adopted cluster
// with OIDC, recycling, scale-down and rebalancing off, a ProvisionCluster
// outbox entry marked adopted, and one Adopted control-plane row per
// control-plane node.
func TestAdoptClusterRecordsAdoptedNodes(t *testing.T) {
	clusterID := uuid.New()
	tx := &testutil.MockTx{}
	store := &testutil.MockDB{MockTx: tx}
	store.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	store.On("ListClusters", mock.Anything, mock.Anything).Return([]db.Cluster{{ID: uuid.New(), Name: "tenant"}}, nil)
	tx.On("CreateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		return c.IsAdopted() && !c.OIDCAuthEnabled && !c.EnableCPNodeRecycling &&
			c.ControlPlaneConfig.DesiredCount == 2 && c.ControlPlaneConfig.AutoScaleDownDisabled && c.ControlPlaneConfig.NodeRebalanceDisabled &&
			c.KubernetesVersion == "1.36.4" && c.PodCIDR == "10.233.64.0/18" && c.ServiceCIDR == "10.233.0.0/18"
	})).Return(db.Cluster{ID: clusterID, Name: "site", KubernetesVersion: "1.36.4"}, nil)
	tx.On("CreateControlPlaneConfiguration", mock.Anything, mock.Anything).Return(db.ControlPlaneConfiguration{ID: uuid.New(), ClusterID: clusterID}, nil).Maybe()
	tx.On("UpdateCluster", mock.Anything, mock.Anything).Return(db.Cluster{ID: clusterID}, nil).Maybe()
	tx.On("CreateWorkflowOutboxEntry", mock.Anything, "ProvisionCluster", mock.Anything, mock.MatchedBy(func(input any) bool {
		m, ok := input.(map[string]any)
		return ok && m[adoptedConst] == true
	})).Return(db.WorkflowOutbox{}, nil)
	tx.On("CreateNode", mock.Anything, mock.MatchedBy(func(n *db.Node) bool {
		return n.IsAdopted() && n.ControlPlane && n.ClusterID == clusterID
	})).Return(db.Node{}, nil).Twice()

	resp, err := newAdoptTestService(store).AdoptCluster(t.Context(), api.AdoptClusterRequestObject{Body: adoptTestRequest()})

	require.NoError(t, err)
	require.IsType(t, api.AdoptCluster201JSONResponse{}, resp)
	tx.AssertExpectations(t)
}

func TestAdoptClusterRefusesASecondAdoption(t *testing.T) {
	tx := &testutil.MockTx{}
	store := &testutil.MockDB{MockTx: tx}
	store.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	store.On("ListClusters", mock.Anything, mock.Anything).Return([]db.Cluster{
		{ID: uuid.New(), Name: "site-a", Conditions: db.ClusterConditions{Adopted: db.NewCondition(true, "Adopted")}},
	}, nil)

	resp, err := newAdoptTestService(store).AdoptCluster(t.Context(), api.AdoptClusterRequestObject{Body: adoptTestRequest()})

	require.NoError(t, err)
	bad, ok := resp.(api.AdoptCluster400JSONResponse)
	require.True(t, ok, "expected AdoptCluster400JSONResponse, got %T", resp)
	assert.Contains(t, bad.Message, "already adopted")
	tx.AssertNotCalled(t, "CreateCluster", mock.Anything, mock.Anything)
}

func TestAdoptClusterIsNotFoundWhileDisabled(t *testing.T) {
	store := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	s := NewClusterService(slog.Default(), store, NewNodePoolService(slog.Default(), store, config.TeleportConfig{}, nil), config.TeleportConfig{}, false, nil, true, "")

	resp, err := s.AdoptCluster(t.Context(), api.AdoptClusterRequestObject{Body: adoptTestRequest()})

	require.NoError(t, err)
	require.IsType(t, api.AdoptCluster404JSONResponse{}, resp)
	store.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
}

// TestRejectAdoptedSettingChanges pins that PATCH keeps off what adopt turned
// off, and refuses control-plane machine settings NVKE never uses for an
// adopted control plane.
func TestRejectAdoptedSettingChanges(t *testing.T) {
	adopted := db.Cluster{Conditions: db.ClusterConditions{Adopted: db.NewCondition(true, "Adopted")}}
	tests := []struct {
		name    string
		cluster db.Cluster
		req     api.UpdateClusterRequest
		wantBad string
	}{
		{name: "turning recycling on", cluster: adopted, req: api.UpdateClusterRequest{EnableCPNodeRecycling: ptr.To(true)}, wantBad: "enableCPNodeRecycling"},
		{
			name: "turning scale-down and rebalancing on", cluster: adopted,
			req:     api.UpdateClusterRequest{ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{AutoScaleDownDisabled: ptr.To(false), NodeRebalanceDisabled: ptr.To(false)}},
			wantBad: "controlPlaneConfig.autoScaleDownDisabled, controlPlaneConfig.nodeRebalanceDisabled",
		},
		{
			name: "control-plane machine settings", cluster: adopted,
			req:     api.UpdateClusterRequest{ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{Nico: &api.NicoControlPlaneProviderUpdate{SubnetIDs: []uuid.UUID{uuid.New()}}}},
			wantBad: "controlPlaneConfig.nico",
		},
		{name: "keeping recycling off", cluster: adopted, req: api.UpdateClusterRequest{EnableCPNodeRecycling: ptr.To(false)}},
		{
			name: "keeping scale-down and rebalancing off", cluster: adopted,
			req: api.UpdateClusterRequest{ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{AutoScaleDownDisabled: ptr.To(true), NodeRebalanceDisabled: ptr.To(true)}},
		},
		{name: "a cluster NVKE created", req: api.UpdateClusterRequest{EnableCPNodeRecycling: ptr.To(true), ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{Nico: &api.NicoControlPlaneProviderUpdate{}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := rejectAdoptedSettingChanges(tc.cluster, &tc.req)
			if tc.wantBad == "" {
				assert.NoError(t, err)
				return
			}
			var bad *badRequestError
			require.ErrorAs(t, err, &bad)
			assert.Contains(t, bad.Error(), tc.wantBad)
		})
	}
}
