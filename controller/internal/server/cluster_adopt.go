// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

const (
	// controlPlaneRoleLabel marks a kubeadm control-plane Node.
	controlPlaneRoleLabel = "node-role.kubernetes.io/control-plane"
	// lookupNodesTimeout bounds the reads of the adopted cluster.
	lookupNodesTimeout = 30 * time.Second
	// adoptionKubeconfigKey is the credentials Secret field holding the site kubeconfig.
	adoptionKubeconfigKey = "kubeconfig"
	// kubeadmConfigMapName holds the ClusterConfiguration kubeadm upgrades from.
	kubeadmConfigMapName = "kubeadm-config"
	// etcdPodLabel selects kubeadm's stacked etcd static pods.
	etcdPodLabel = "component"
)

// adoptedNode is a control-plane node found in the cluster being adopted.
type adoptedNode struct {
	Name string
	IP   string
	// KubeletVersion is the node's reported kubelet version, without a leading "v".
	KubeletVersion string
}

// liveCluster is what the cluster being adopted reports about itself.
type liveCluster struct {
	// kubernetesVersion, podSubnet and serviceSubnet come from kubeadm-config.
	kubernetesVersion string
	podSubnet         string
	serviceSubnet     string
	// namespaces lists the namespaces that exist, to confirm the CNI.
	namespaces []string
}

// adoption is what AdoptCluster read from the credentials Secret and the cluster.
type adoption struct {
	nodes []adoptedNode
	live  liveCluster
}

// AdoptCluster records the cluster the controller runs on, without running
// kubeadm init. Its operator keeps managing the control plane: NVKE never
// upgrades, replaces or deletes an adopted control-plane node. It shares
// createCluster with CreateCluster; the differences are the credentials Secret,
// the checks against the running cluster, the adopted node rows, and the
// control-plane build ProvisionCluster skips.
func (s *clusterService) AdoptCluster(ctx context.Context, req api.AdoptClusterRequestObject) (api.AdoptClusterResponseObject, error) {
	if !s.adoptionEnabled {
		return api.AdoptCluster404JSONResponse{Message: "cluster adoption is not enabled on this controller"}, nil
	}
	adopt, badRequest, err := s.readAdoption(ctx, req.Body)
	if err != nil {
		return nil, err
	}
	if badRequest != "" {
		return api.AdoptCluster400JSONResponse{Message: badRequest}, nil
	}
	created, badRequest, err := s.createCluster(ctx, req.Body, adopt)
	if err != nil {
		return nil, err
	}
	if badRequest != "" {
		return api.AdoptCluster400JSONResponse{Message: badRequest}, nil
	}
	s.logger.Info("adopted cluster", "clusterID", created.ID, "clusterName", created.Name, "controlPlaneNodes", len(adopt.nodes))
	return api.AdoptCluster201JSONResponse(APIClusterFromDB(&created)), nil
}

// readAdoption reads the credentials Secret, and reads the listed control-plane
// nodes and the cluster's kubeadm configuration with its kubeconfig.
func (s *clusterService) readAdoption(ctx context.Context, body *api.CreateClusterRequest) (*adoption, string, error) {
	if body.Adoption == nil {
		return nil, "adoption is required", nil
	}
	if len(body.Adoption.Nodes) == 0 {
		return nil, "adoption.nodes must name at least one control-plane node", nil
	}
	// A gap makes scale-up build control-plane nodes through the provider.
	if body.ControlPlaneConfig == nil || body.ControlPlaneConfig.DesiredCount != len(body.Adoption.Nodes) {
		return nil, fmt.Sprintf("controlPlaneConfig.desiredCount must equal the number of adoption.nodes (%d)", len(body.Adoption.Nodes)), nil
	}
	if s.k8sClient == nil {
		return nil, "", errors.New("adopting a cluster needs a Kubernetes client")
	}

	credentials, badRequest, err := s.readSecret(ctx, body.Adoption.CredentialsSecretRef)
	if err != nil || badRequest != "" {
		return nil, badRequest, err
	}
	kubeconfig := credentials[adoptionKubeconfigKey]
	if len(kubeconfig) == 0 {
		return nil, fmt.Sprintf("adoption.credentialsSecretRef Secret has no %q", adoptionKubeconfigKey), nil
	}

	// ProvisionCluster force-applies the Cluster API scaffold under the cluster's
	// name. Over an existing Cluster (one moved here by a pivot), that would take
	// over its infrastructure and unpause it, so Cluster API could act on the
	// control plane again.
	existing := &clusterv1.Cluster{}
	if err := s.k8sClient.Get(ctx, ctrlclient.ObjectKey{Namespace: s.managementNamespace, Name: body.Name}, existing); err == nil {
		return nil, fmt.Sprintf("a Cluster API Cluster %s/%s already exists; adopt a cluster that no Cluster API object manages", s.managementNamespace, body.Name), nil
	} else if !apierrors.IsNotFound(err) {
		return nil, "", fmt.Errorf("checking for Cluster API Cluster %s/%s: %w", s.managementNamespace, body.Name, err)
	}

	newClient := s.adoptedClusterClient
	if newClient == nil {
		newClient = func(kubeconfig []byte, clusterName string) (ctrlclient.Client, error) {
			return kube.NewClientFromKubeconfig(kubeconfig, clusterName)
		}
	}
	client, err := newClient(kubeconfig, body.Name)
	if err != nil {
		return nil, fmt.Sprintf("kubeconfig in the credentials Secret: %v", err), nil
	}
	ctx, cancel := context.WithTimeout(ctx, lookupNodesTimeout)
	defer cancel()
	nodes, badRequest := lookupControlPlaneNodes(ctx, client, body.Adoption.Nodes)
	if badRequest != "" {
		return nil, badRequest, nil
	}
	live, badRequest := readLiveCluster(ctx, client)
	if badRequest != "" {
		return nil, badRequest, nil
	}
	return &adoption{nodes: nodes, live: live}, "", nil
}

func (s *clusterService) readSecret(ctx context.Context, ref api.SecretReference) (map[string][]byte, string, error) {
	namespace := ref.Namespace
	if namespace == "" {
		namespace = s.podNamespace
	}
	secret := &corev1.Secret{}
	if err := s.k8sClient.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Sprintf("Secret %s/%s not found", namespace, ref.Name), nil
		}
		return nil, "", fmt.Errorf("reading Secret %s/%s: %w", namespace, ref.Name, err)
	}
	return secret.Data, "", nil
}

// lookupControlPlaneNodes requires names to be exactly the cluster's
// control-plane Nodes and its stacked etcd members, each with an InternalIP
// (NVKE addresses nodes by IP). An unlisted control-plane node would be
// unmanaged, and an extra etcd member would be one NVKE never accounts for in
// quorum. A client error is a bad request too: the kubeconfig is the operator's input.
func lookupControlPlaneNodes(ctx context.Context, client ctrlclient.Client, names []string) ([]adoptedNode, string) {
	var controlPlane corev1.NodeList
	if err := client.List(ctx, &controlPlane, ctrlclient.HasLabels{controlPlaneRoleLabel}); err != nil {
		return nil, fmt.Sprintf("listing nodes with the credentials Secret's kubeconfig: %v", err)
	}
	byName := map[string]corev1.Node{}
	for _, n := range controlPlane.Items {
		byName[n.Name] = n
	}
	nodes := make([]adoptedNode, 0, len(names))
	for _, name := range names {
		n, ok := byName[name]
		if !ok {
			return nil, fmt.Sprintf("adoption.nodes: node %s is not a control-plane node of the cluster", name)
		}
		ip := ""
		for _, addr := range n.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				ip = addr.Address
				break
			}
		}
		if ip == "" {
			return nil, fmt.Sprintf("adoption.nodes: node %s has no InternalIP", name)
		}
		nodes = append(nodes, adoptedNode{Name: name, IP: ip, KubeletVersion: strings.TrimPrefix(n.Status.NodeInfo.KubeletVersion, "v")})
	}
	for name := range byName {
		if !slices.Contains(names, name) {
			return nil, fmt.Sprintf("adoption.nodes must list every control-plane node; %s is missing", name)
		}
	}

	// shortcut: etcd membership is read from kubeadm's stacked etcd static pods, since
	// the agents that can list members answer only after the SPIRE move; read the
	// members through an agent if a cluster ever runs etcd outside static pods.
	var etcdPods corev1.PodList
	if err := client.List(ctx, &etcdPods, ctrlclient.InNamespace(metav1.NamespaceSystem), ctrlclient.MatchingLabels{etcdPodLabel: "etcd"}); err != nil {
		return nil, fmt.Sprintf("listing etcd pods with the credentials Secret's kubeconfig: %v", err)
	}
	etcdNodes := map[string]bool{}
	for _, p := range etcdPods.Items {
		etcdNodes[p.Spec.NodeName] = true
	}
	for _, name := range names {
		if !etcdNodes[name] {
			return nil, fmt.Sprintf("adoption.nodes: node %s runs no etcd member", name)
		}
		delete(etcdNodes, name)
	}
	for name := range etcdNodes {
		return nil, fmt.Sprintf("etcd runs on node %s, which adoption.nodes does not list", name)
	}
	return nodes, ""
}

// readLiveCluster reads the version and networks kubeadm upgrades the cluster
// with, and the namespaces that show which CNI runs.
func readLiveCluster(ctx context.Context, client ctrlclient.Client) (liveCluster, string) {
	var cm corev1.ConfigMap
	if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: metav1.NamespaceSystem, Name: kubeadmConfigMapName}, &cm); err != nil {
		return liveCluster{}, fmt.Sprintf("reading kube-system/%s with the credentials Secret's kubeconfig: %v", kubeadmConfigMapName, err)
	}
	var cfg struct {
		KubernetesVersion string `json:"kubernetesVersion"`
		Networking        struct {
			PodSubnet     string `json:"podSubnet"`
			ServiceSubnet string `json:"serviceSubnet"`
		} `json:"networking"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data["ClusterConfiguration"]), &cfg); err != nil {
		return liveCluster{}, fmt.Sprintf("parsing kube-system/%s ClusterConfiguration: %v", kubeadmConfigMapName, err)
	}
	var namespaces corev1.NamespaceList
	if err := client.List(ctx, &namespaces); err != nil {
		return liveCluster{}, fmt.Sprintf("listing namespaces with the credentials Secret's kubeconfig: %v", err)
	}
	live := liveCluster{
		kubernetesVersion: strings.TrimPrefix(cfg.KubernetesVersion, "v"),
		podSubnet:         cfg.Networking.PodSubnet,
		serviceSubnet:     cfg.Networking.ServiceSubnet,
	}
	for _, ns := range namespaces.Items {
		live.namespaces = append(live.namespaces, ns.Name)
	}
	return live, ""
}

// mismatch returns why the cluster record would not describe the running
// cluster, or "". Nodes NVKE adds install exactly the version and CNI the
// record names, so a record that differs builds nodes that do not fit.
//
// The CNI version is not compared: the cluster reports no version NVKE can map
// to cniVersion (the OVN chart and image do not carry the package stream).
func (a *adoption) mismatch(cluster db.Cluster) string {
	for _, n := range a.nodes {
		if n.KubeletVersion != cluster.KubernetesVersion {
			return fmt.Sprintf("controlPlaneKubernetesVersion is %s, but node %s runs kubelet %s", cluster.KubernetesVersion, n.Name, n.KubeletVersion)
		}
	}
	if a.live.kubernetesVersion != cluster.KubernetesVersion {
		return fmt.Sprintf("controlPlaneKubernetesVersion is %s, but kubeadm-config says %s", cluster.KubernetesVersion, a.live.kubernetesVersion)
	}
	if !sameCIDRs(a.live.podSubnet, cluster.PodCIDR) {
		return fmt.Sprintf("network.podCIDR is %s, but the cluster's pod subnet is %s", cluster.PodCIDR, a.live.podSubnet)
	}
	if !sameCIDRs(a.live.serviceSubnet, cluster.ServiceCIDR) {
		return fmt.Sprintf("network.serviceCIDR is %s, but the cluster's service subnet is %s", cluster.ServiceCIDR, a.live.serviceSubnet)
	}
	plugin := db.EffectiveCNIPlugin(cluster.CniPlugin)
	for _, ns := range activities.CNINamespacesForPlugin(plugin) {
		if !slices.Contains(a.live.namespaces, ns) {
			return fmt.Sprintf("cniPlugin is %s, but the cluster has no %s namespace", plugin, ns)
		}
	}
	return ""
}

// sameCIDRs compares comma-separated CIDR lists as sets of canonical prefixes.
func sameCIDRs(a, b string) bool {
	canon := func(s string) []string {
		var out []string
		for _, part := range strings.Split(s, ",") {
			p, err := netip.ParsePrefix(strings.TrimSpace(part))
			if err != nil {
				return []string{s}
			}
			out = append(out, p.Masked().String())
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(canon(a), canon(b))
}

// createSeedNodes records the listed control-plane nodes as adopted. They count
// toward desiredCount, and NVKE never changes them: the operator keeps managing
// the control plane. NVKE only reads through their agents (the cluster's
// kubeconfig and kubeadm join strings), which answer once the operator moves each
// node's SPIRE agent to this site and registers host/<node ID>.
func (s *clusterService) createSeedNodes(ctx context.Context, tx db.Tx, cluster db.Cluster, adopted []adoptedNode) error {
	for _, n := range adopted {
		node := db.Node{
			ID:                          uuid.New(),
			ClusterID:                   cluster.ID,
			ControlPlane:                true,
			ControlPlaneConfigurationID: cluster.ControlPlaneConfig.ConfigurationID,
			Name:                        n.Name,
			IP:                          n.IP,
			Conditions: db.NodeConditions{
				Pending:         db.NewCondition(false, "Adopted"),
				Provisioning:    db.NewCondition(false, "Adopted"),
				ReadyToJoin:     db.NewCondition(true, "Adopted"),
				Joined:          db.NewCondition(true, "Adopted: joined before NVKE managed the cluster"),
				Provisioned:     db.NewCondition(true, "Adopted"),
				Deleting:        db.NewCondition(false, "Node is not being deleted"),
				KubernetesReady: db.NewCondition(true, "Adopted"),
				NodeAgentReady:  db.NewCondition(false, "Adopted: waiting for the node agent"),
				AdoptedSeed:     db.NewCondition(true, "Ran before NVKE adopted the cluster"),
			},
			// The settings every node records at creation, so the agent is reached the same way.
			SpiffeEnabled:                s.nodeAgentProfile.SpiffeEnabled,
			SpireAttestationMethod:       s.nodeAgentProfile.SpireAttestationMethod,
			NodeAgentExternalMTLSEnabled: s.nodeAgentProfile.NodeAgentExternalMTLSEnabled,
			NodeAgentTLSJWTEnabled:       s.nodeAgentProfile.NodeAgentTLSJWTEnabled,
		}
		if _, err := tx.CreateNode(ctx, &node); err != nil {
			return fmt.Errorf("recording adopted node %s: %w", n.Name, err)
		}
	}
	return nil
}
