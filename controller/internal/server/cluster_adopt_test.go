// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
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
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

const (
	adoptTestNamespace           = "nvke-controller"
	adoptTestManagementNamespace = "nvke-system"
)

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

func adoptTestEtcdPod(node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "etcd-" + node, Namespace: metav1.NamespaceSystem, Labels: map[string]string{etcdPodLabel: "etcd"}},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

func adoptTestKubeadmConfig() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: kubeadmConfigMapName, Namespace: metav1.NamespaceSystem},
		Data: map[string]string{"ClusterConfiguration": `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.36.4
networking:
  podSubnet: 10.233.64.0/18
  serviceSubnet: 10.233.0.0/18
`},
	}
}

// adoptTestCluster is a running two-node control plane with a worker.
func adoptTestCluster() []ctrlclient.Object {
	return []ctrlclient.Object{
		adoptTestNode("cp-1", "10.0.0.1", true), adoptTestNode("cp-2", "10.0.0.2", true), adoptTestNode("worker-1", "10.0.0.9", false),
		adoptTestEtcdPod("cp-1"), adoptTestEtcdPod("cp-2"),
		adoptTestKubeadmConfig(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ovn-kubernetes"}},
	}
}

func adoptTestCredentials() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: adoptTestNamespace},
		Data:       map[string][]byte{adoptionKubeconfigKey: []byte("kubeconfig")},
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

func TestReadAdoption(t *testing.T) {
	credentials := adoptTestCredentials()
	existingCAPICluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: adoptTestManagementNamespace}}
	noKubeconfig := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: adoptTestNamespace}, Data: map[string][]byte{"ca.crt": []byte("x")}}
	withExtra := func(extra ...ctrlclient.Object) []ctrlclient.Object { return append(adoptTestCluster(), extra...) }
	withoutEtcd := func(node string) []ctrlclient.Object {
		var out []ctrlclient.Object
		for _, o := range adoptTestCluster() {
			if p, ok := o.(*corev1.Pod); ok && p.Spec.NodeName == node {
				continue
			}
			out = append(out, o)
		}
		return out
	}

	tests := []struct {
		name         string
		secrets      []ctrlclient.Object
		cluster      []ctrlclient.Object
		noAdoption   bool
		nodes        []string
		desiredCount int
		wantBad      string
		wantNodes    []adoptedNode
	}{
		{
			name: "every control-plane node listed is adopted", secrets: []ctrlclient.Object{credentials}, nodes: []string{"cp-2", "cp-1"},
			wantNodes: []adoptedNode{{Name: "cp-2", IP: "10.0.0.2", KubeletVersion: "1.36.4"}, {Name: "cp-1", IP: "10.0.0.1", KubeletVersion: "1.36.4"}},
		},
		{name: "missing adoption field", secrets: []ctrlclient.Object{credentials}, noAdoption: true, wantBad: "adoption is required"},
		{name: "no nodes listed", secrets: []ctrlclient.Object{credentials}, wantBad: "adoption.nodes must name"},
		{
			name: "desiredCount differs from the listed nodes", secrets: []ctrlclient.Object{credentials}, nodes: []string{"cp-1", "cp-2"}, desiredCount: 3,
			wantBad: "desiredCount must equal the number of adoption.nodes (2)",
		},
		{name: "credentials secret not found", secrets: []ctrlclient.Object{}, nodes: []string{"cp-1", "cp-2"}, wantBad: "creds not found"},
		{name: "no kubeconfig", secrets: []ctrlclient.Object{noKubeconfig}, nodes: []string{"cp-1", "cp-2"}, wantBad: `no "kubeconfig"`},
		{
			name: "a Cluster API Cluster already has the name", secrets: []ctrlclient.Object{credentials, existingCAPICluster}, nodes: []string{"cp-1", "cp-2"},
			wantBad: "Cluster API Cluster nvke-system/site already exists",
		},
		{name: "unknown node", secrets: []ctrlclient.Object{credentials}, nodes: []string{"cp-1", "cp-9"}, wantBad: "node cp-9 is not a control-plane node"},
		{name: "worker node", secrets: []ctrlclient.Object{credentials}, nodes: []string{"cp-1", "worker-1"}, wantBad: "worker-1 is not a control-plane node"},
		{name: "a control-plane node left out", secrets: []ctrlclient.Object{credentials}, nodes: []string{"cp-1"}, wantBad: "cp-2 is missing"},
		{
			name: "node without an InternalIP", secrets: []ctrlclient.Object{credentials}, cluster: withExtra(adoptTestNode("cp-noip", "", true)),
			nodes: []string{"cp-1", "cp-2", "cp-noip"}, wantBad: "cp-noip has no InternalIP",
		},
		{
			name: "listed node runs no etcd member", secrets: []ctrlclient.Object{credentials}, cluster: withoutEtcd("cp-2"),
			nodes: []string{"cp-1", "cp-2"}, wantBad: "cp-2 runs no etcd member",
		},
		{
			name: "etcd member on an unlisted node", secrets: []ctrlclient.Object{credentials}, cluster: withExtra(adoptTestEtcdPod("worker-1")),
			nodes: []string{"cp-1", "cp-2"}, wantBad: "etcd runs on node worker-1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cluster := tc.cluster
			if cluster == nil {
				cluster = adoptTestCluster()
			}
			adopted := buildFake(cluster...)
			var gotKubeconfig []byte
			s := &clusterService{
				logger: slog.Default(), k8sClient: buildFake(tc.secrets...), podNamespace: adoptTestNamespace, managementNamespace: adoptTestManagementNamespace,
				adoptedClusterClient: func(kubeconfig []byte, _ string) (ctrlclient.Client, error) {
					gotKubeconfig = kubeconfig
					return adopted, nil
				},
			}
			desired := tc.desiredCount
			if desired == 0 {
				desired = len(tc.nodes)
			}
			body := &api.CreateClusterRequest{Name: "site", EtcdEncrypted: true, ControlPlaneConfig: &api.CreateClusterControlPlaneConfig{DesiredCount: desired}}
			if !tc.noAdoption {
				body.Adoption = &api.ClusterAdoption{CredentialsSecretRef: api.SecretReference{Name: "creds"}, Nodes: tc.nodes}
			}

			adopt, bad, err := s.readAdoption(context.Background(), body)

			require.NoError(t, err)
			if tc.wantBad != "" {
				assert.Contains(t, bad, tc.wantBad)
				assert.Nil(t, adopt)
				return
			}
			assert.Empty(t, bad)
			assert.Equal(t, []byte("kubeconfig"), gotKubeconfig)
			assert.Equal(t, tc.wantNodes, adopt.nodes)
			assert.Equal(t, liveCluster{
				kubernetesVersion: "1.36.4", podSubnet: "10.233.64.0/18", serviceSubnet: "10.233.0.0/18", namespaces: []string{"ovn-kubernetes"},
			}, adopt.live)
		})
	}
}

func TestAdoptionMismatch(t *testing.T) {
	matching := db.Cluster{KubernetesVersion: "1.36.4", PodCIDR: "10.233.64.0/18", ServiceCIDR: "10.233.0.0/18", CniPlugin: db.CNIOvnKubernetes}
	adopt := func() *adoption {
		return &adoption{
			nodes: []adoptedNode{{Name: "cp-1", KubeletVersion: "1.36.4"}, {Name: "cp-2", KubeletVersion: "1.36.4"}},
			live:  liveCluster{kubernetesVersion: "1.36.4", podSubnet: "10.233.64.0/18", serviceSubnet: "10.233.0.0/18", namespaces: []string{"kube-system", "ovn-kubernetes"}},
		}
	}
	tests := []struct {
		name    string
		mutate  func(*db.Cluster, *adoption)
		wantBad string
	}{
		{name: "record matches the cluster", mutate: func(*db.Cluster, *adoption) {}},
		{name: "empty CNI plugin means ovn-kubernetes", mutate: func(c *db.Cluster, _ *adoption) { c.CniPlugin = "" }},
		{
			name: "equal CIDR lists in another order", mutate: func(c *db.Cluster, a *adoption) {
				c.PodCIDR = "10.233.64.0/18,fd00:10::/56"
				a.live.podSubnet = "fd00:10::/56, 10.233.64.0/18"
			},
		},
		{name: "declared version differs from a kubelet", mutate: func(_ *db.Cluster, a *adoption) { a.nodes[1].KubeletVersion = "1.36.3" }, wantBad: "node cp-2 runs kubelet 1.36.3"},
		{name: "declared version differs from kubeadm-config", mutate: func(_ *db.Cluster, a *adoption) { a.live.kubernetesVersion = "1.35.9" }, wantBad: "kubeadm-config says 1.35.9"},
		{name: "pod CIDR differs", mutate: func(c *db.Cluster, _ *adoption) { c.PodCIDR = "192.168.0.0/16" }, wantBad: "network.podCIDR is 192.168.0.0/16"},
		{name: "service CIDR differs", mutate: func(c *db.Cluster, _ *adoption) { c.ServiceCIDR = "10.96.0.0/12" }, wantBad: "network.serviceCIDR is 10.96.0.0/12"},
		{name: "declared CNI does not run", mutate: func(c *db.Cluster, _ *adoption) { c.CniPlugin = db.CNICalico }, wantBad: "no tigera-operator namespace"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cluster, a := matching, adopt()
			tc.mutate(&cluster, a)
			got := a.mismatch(cluster)
			if tc.wantBad == "" {
				assert.Empty(t, got)
				return
			}
			assert.Contains(t, got, tc.wantBad)
		})
	}
}

// adoptTestRequest is an adopt body that matches adoptTestCluster.
func adoptTestRequest() *api.CreateClusterRequest {
	return &api.CreateClusterRequest{
		Name:                          "site",
		Nico:                          &api.NicoClusterProvider{OrgID: "novxwn552ssc", SiteID: uuid.New(), VpcID: uuid.New()},
		CniPlugin:                     "ovn-kubernetes",
		ControlPlaneKubernetesVersion: "1.36.4",
		ControlPlaneVIP:               "172.31.1.10",
		EtcdEncrypted:                 true,
		ControlPlaneConfig: &api.CreateClusterControlPlaneConfig{DesiredCount: 2, Nico: &api.NicoControlPlaneProviderCreate{
			InstanceTypePreference: []uuid.UUID{uuid.New()}, Release: "ubuntu", ConfigProfile: "default", SubnetIDs: []uuid.UUID{uuid.New()},
		}},
		Network:  &api.ClusterNetworkCreateConfig{PodCIDR: "10.233.64.0/18", ServiceCIDR: "10.233.0.0/18"},
		Adoption: &api.ClusterAdoption{CredentialsSecretRef: api.SecretReference{Name: "creds"}, Nodes: []string{"cp-1", "cp-2"}},
	}
}

// newAdoptTestService serves adoption against adoptTestCluster.
func newAdoptTestService(store *testutil.MockDB) *clusterService {
	adopted := buildFake(adoptTestCluster()...)
	s := NewClusterService(slog.Default(), store, NewNodePoolService(slog.Default(), store, config.TeleportConfig{}, nil), config.TeleportConfig{}, false, nil, true, "")
	s.k8sClient, s.podNamespace, s.managementNamespace = buildFake(adoptTestCredentials()), adoptTestNamespace, adoptTestManagementNamespace
	s.adoptionEnabled, s.oidcAuthEnabled = true, true
	s.adoptedClusterClient = func([]byte, string) (ctrlclient.Client, error) { return adopted, nil }
	return s
}

// TestAdoptClusterRecordsAdoptedNodes pins what adopt writes: an adopted cluster
// with OIDC off, a ProvisionCluster outbox entry marked adopted, and one
// AdoptedSeed control-plane row per listed node.
func TestAdoptClusterRecordsAdoptedNodes(t *testing.T) {
	clusterID := uuid.New()
	tx := &testutil.MockTx{}
	store := &testutil.MockDB{MockTx: tx}
	store.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	tx.On("ListClusters", mock.Anything, mock.Anything).Return([]db.Cluster{{ID: uuid.New(), Name: "tenant"}}, nil)
	tx.On("CreateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		return c.IsAdopted() && !c.OIDCAuthEnabled
	})).Return(db.Cluster{ID: clusterID, Name: "site", KubernetesVersion: "1.36.4"}, nil)
	tx.On("CreateControlPlaneConfiguration", mock.Anything, mock.Anything).Return(db.ControlPlaneConfiguration{ID: uuid.New(), ClusterID: clusterID}, nil).Maybe()
	tx.On("UpdateCluster", mock.Anything, mock.Anything).Return(db.Cluster{ID: clusterID}, nil).Maybe()
	tx.On("CreateWorkflowOutboxEntry", mock.Anything, "ProvisionCluster", mock.Anything, mock.MatchedBy(func(input any) bool {
		m, ok := input.(map[string]any)
		return ok && m[adoptedConst] == true
	})).Return(db.WorkflowOutbox{}, nil)
	tx.On("CreateNode", mock.Anything, mock.MatchedBy(func(n *db.Node) bool {
		return n.IsAdoptedSeed() && n.ControlPlane && n.ClusterID == clusterID
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
	tx.On("ListClusters", mock.Anything, mock.Anything).Return([]db.Cluster{
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

	resp, err := s.AdoptCluster(t.Context(), api.AdoptClusterRequestObject{Body: &api.CreateClusterRequest{Name: "site"}})

	require.NoError(t, err)
	require.IsType(t, api.AdoptCluster404JSONResponse{}, resp)
	store.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
}
