// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	// controlPlaneRoleLabel marks a kubeadm control-plane Node.
	controlPlaneRoleLabel = "node-role.kubernetes.io/control-plane"
	// lookupNodesTimeout bounds the reads of the cluster being adopted.
	lookupNodesTimeout = 30 * time.Second
	// kubeadmConfigMapName holds the ClusterConfiguration kubeadm upgrades from.
	kubeadmConfigMapName = "kubeadm-config"
)

// adoptedNode is a control-plane node found in the cluster being adopted.
type adoptedNode struct {
	Name string
	IP   string
	// KubeletVersion is the node's reported kubelet version, without a leading "v".
	KubeletVersion string
}

// adoptedCluster is what kubeadm-config reports about the cluster being adopted.
type adoptedCluster struct {
	kubernetesVersion string
	podSubnet         string
	serviceSubnet     string
}

// adoption is what AdoptCluster read from the cluster it runs on.
type adoption struct {
	nodes   []adoptedNode
	network resolvedClusterNetwork
}

// AdoptCluster records the cluster the controller runs on, without running
// kubeadm init. Its operator keeps managing the control plane. It reads the
// control-plane nodes, version and networks from the cluster with the
// controller's own client, then records the cluster through recordCluster, as
// CreateCluster does, with the adopted marker and a row per control-plane node.
func (s *clusterService) AdoptCluster(ctx context.Context, req api.AdoptClusterRequestObject) (api.AdoptClusterResponseObject, error) {
	if !s.adoptionEnabled {
		return api.AdoptCluster404JSONResponse{Message: "cluster adoption is not enabled on this controller"}, nil
	}
	body, adopt, err := s.readAdoption(ctx, req.Body)
	var created db.Cluster
	if err == nil {
		err = s.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
			dbCluster, err := DBClusterFromAPI(body)
			if err != nil {
				return fmt.Errorf("failed to convert cluster request: %w", err)
			}
			if err := s.applyClusterDefaults(body, &dbCluster); err != nil {
				return err
			}
			dbCluster.PodCIDR = adopt.network.podCIDR
			dbCluster.ServiceCIDR = adopt.network.serviceCIDR.String()
			dbCluster.Conditions.Adopted = db.NewCondition(true, "Adopted; its operator manages the control plane")
			// NVKE cannot configure an adopted cluster's API servers for the fleet's
			// OIDC issuer, so it reaches the cluster with the admin kubeconfig.
			dbCluster.OIDCAuthEnabled = false

			if created, err = s.recordCluster(ctx, tx, dbCluster, body.ControlPlaneConfig, true); err != nil {
				return err
			}
			return s.createAdoptedNodes(ctx, tx, created, adopt.nodes)
		})
	}
	if errors.Is(err, db.ErrClusterNameNotUnique) {
		return api.AdoptCluster400JSONResponse{Message: fmt.Sprintf("A cluster already exists with the name %q", req.Body.Name)}, nil
	}
	var badRequestErr *badRequestError
	if errors.As(err, &badRequestErr) {
		return api.AdoptCluster400JSONResponse{Message: badRequestErr.Error()}, nil
	}
	if err != nil {
		return nil, err
	}
	s.logger.Info("adopted cluster", "clusterID", created.ID, "clusterName", created.Name, "controlPlaneNodes", len(adopt.nodes))
	return api.AdoptCluster201JSONResponse(APIClusterFromDB(&created)), nil
}

// readAdoption validates the request, reads the cluster the controller runs on,
// and returns the createCluster body that records it. A request or cluster NVKE
// cannot adopt is a *badRequestError.
func (s *clusterService) readAdoption(ctx context.Context, req *api.AdoptClusterRequest) (*api.CreateClusterRequest, *adoption, error) {
	if err := validateClusterName(req.Name); err != nil {
		return nil, nil, newBadRequestErrorf("%s", err)
	}
	if err := s.providers.require(config.InfraProviderNico); err != nil {
		return nil, nil, newBadRequestErrorf("%s", err)
	}
	if req.Nico.SiteID == uuid.Nil || req.Nico.VpcID == uuid.Nil {
		return nil, nil, newBadRequestErrorf("nico.siteID and nico.vpcID are required: NVKE creates worker machines in that site and VPC")
	}
	if err := s.validateCNIChoice(string(req.CniPlugin), req.CniVersion); err != nil {
		return nil, nil, newBadRequestErrorf("%s", err)
	}
	if s.k8sClient == nil {
		return nil, nil, errors.New("adopting a cluster needs a Kubernetes client")
	}
	// The controller runs on one cluster, and an adopted record cannot be deleted.
	recorded, err := s.db.ListClusters(ctx, db.ClusterFilters{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing clusters: %w", err)
	}
	if slices.ContainsFunc(recorded, db.Cluster.IsAdopted) {
		return nil, nil, newBadRequestErrorf("a cluster is already adopted: this controller adopts only the cluster it runs on")
	}

	// ProvisionCluster force-applies the Cluster API scaffold under the cluster's
	// name. Over an existing Cluster (one moved here by a pivot), that would take
	// over its infrastructure and unpause it, so Cluster API could act on the
	// control plane again.
	existing := &clusterv1.Cluster{}
	if err := s.k8sClient.Get(ctx, ctrlclient.ObjectKey{Namespace: s.managementNamespace, Name: req.Name}, existing); err == nil {
		return nil, nil, newBadRequestErrorf("a Cluster API Cluster %s/%s already exists; adopt a cluster that no Cluster API object manages", s.managementNamespace, req.Name)
	} else if !apierrors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("checking for Cluster API Cluster %s/%s: %w", s.managementNamespace, req.Name, err)
	}

	ctx, cancel := context.WithTimeout(ctx, lookupNodesTimeout)
	defer cancel()
	nodes, err := listControlPlaneNodes(ctx, s.k8sClient)
	if err != nil {
		return nil, nil, err
	}
	live, err := readAdoptedCluster(ctx, s.k8sClient)
	if err != nil {
		return nil, nil, err
	}
	// Workers join at the recorded version, so it must be the one every API
	// server runs: kubeadm moves kubeadm-config to the new version on the first
	// control-plane node of an upgrade.
	for _, n := range nodes {
		if n.KubeletVersion != live.kubernetesVersion {
			return nil, nil, newBadRequestErrorf("node %s runs kubelet %s, but kube-system/%s says %s; adopt the cluster once its upgrade finishes",
				n.Name, n.KubeletVersion, kubeadmConfigMapName, live.kubernetesVersion)
		}
	}
	if live.podSubnet == "" {
		return nil, nil, newBadRequestErrorf("kube-system/%s has no networking.podSubnet; NVKE records a cluster's pod CIDR", kubeadmConfigMapName)
	}
	network, err := resolveClusterNetwork(&api.ClusterNetworkCreateConfig{PodCIDR: live.podSubnet, ServiceCIDR: live.serviceSubnet})
	if err != nil {
		return nil, nil, newBadRequestErrorf("kube-system/%s: %v", kubeadmConfigMapName, err)
	}
	if err := validatePodCIDRCapacity(network.podCIDRPrefixes, len(nodes), 0); err != nil {
		return nil, nil, newBadRequestErrorf("kube-system/%s: %v", kubeadmConfigMapName, err)
	}
	cni := db.EffectiveCNIPlugin(db.CniPlugin(req.CniPlugin))
	if err := network.validateCNI(cni); err != nil {
		return nil, nil, newBadRequestErrorf("%s", err)
	}

	nico := req.Nico
	body := &api.CreateClusterRequest{
		Name:                          req.Name,
		ControlPlaneKubernetesVersion: live.kubernetesVersion,
		CniPlugin:                     api.CreateClusterRequestCniPlugin(cni),
		CniVersion:                    req.CniVersion,
		EtcdEncrypted:                 req.EtcdEncrypted,
		Nico:                          &nico,
		Network:                       &api.ClusterNetworkCreateConfig{PodCIDR: network.podCIDR, ServiceCIDR: network.serviceCIDR.String()},
		// The operator keeps the control plane: recycling, automatic scale-down and
		// rebalancing stay off, and PATCH refuses turning them back on.
		EnableCPNodeRecycling: ptr.To(false),
		ControlPlaneConfig: &api.CreateClusterControlPlaneConfig{
			DesiredCount:          len(nodes),
			AutoScaleDownDisabled: ptr.To(true),
			NodeRebalanceDisabled: ptr.To(true),
		},
	}
	return body, &adoption{nodes: nodes, network: network}, nil
}

// listControlPlaneNodes returns every control-plane Node of the cluster, each
// with an InternalIP: NVKE reaches a node's agent by IP, and recording every one
// keeps desiredCount equal to the control-plane nodes on record. A failure to
// list is the controller's own (its service account), not the caller's.
func listControlPlaneNodes(ctx context.Context, client ctrlclient.Reader) ([]adoptedNode, error) {
	var controlPlane corev1.NodeList
	if err := client.List(ctx, &controlPlane, ctrlclient.HasLabels{controlPlaneRoleLabel}); err != nil {
		return nil, fmt.Errorf("listing control-plane Nodes: %w", err)
	}
	if len(controlPlane.Items) == 0 {
		return nil, newBadRequestErrorf("the cluster has no Node labeled %s", controlPlaneRoleLabel)
	}
	nodes := make([]adoptedNode, 0, len(controlPlane.Items))
	for _, n := range controlPlane.Items {
		ip := ""
		for _, addr := range n.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				ip = addr.Address
				break
			}
		}
		if ip == "" {
			return nil, newBadRequestErrorf("control-plane node %s has no InternalIP", n.Name)
		}
		nodes = append(nodes, adoptedNode{Name: n.Name, IP: ip, KubeletVersion: strings.TrimPrefix(n.Status.NodeInfo.KubeletVersion, "v")})
	}
	slices.SortFunc(nodes, func(a, b adoptedNode) int { return strings.Compare(a.Name, b.Name) })
	return nodes, nil
}

// readAdoptedCluster reads the version and networks kubeadm upgrades the cluster
// with.
func readAdoptedCluster(ctx context.Context, client ctrlclient.Reader) (adoptedCluster, error) {
	var cm corev1.ConfigMap
	if err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: metav1.NamespaceSystem, Name: kubeadmConfigMapName}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return adoptedCluster{}, newBadRequestErrorf("kube-system/%s not found: NVKE adopts a cluster kubeadm built", kubeadmConfigMapName)
		}
		return adoptedCluster{}, fmt.Errorf("reading kube-system/%s: %w", kubeadmConfigMapName, err)
	}
	var cfg struct {
		KubernetesVersion string `json:"kubernetesVersion"`
		Networking        struct {
			PodSubnet     string `json:"podSubnet"`
			ServiceSubnet string `json:"serviceSubnet"`
		} `json:"networking"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data["ClusterConfiguration"]), &cfg); err != nil {
		return adoptedCluster{}, newBadRequestErrorf("parsing kube-system/%s ClusterConfiguration: %v", kubeadmConfigMapName, err)
	}
	// NVKE records a release version, X.Y.Z, as createCluster requires: worker
	// packages and version-skew checks are built from it.
	version := strings.TrimPrefix(cfg.KubernetesVersion, "v")
	if !cniVersionPattern.MatchString(version) {
		return adoptedCluster{}, newBadRequestErrorf("kube-system/%s kubernetesVersion %q is not a release version X.Y.Z", kubeadmConfigMapName, cfg.KubernetesVersion)
	}
	return adoptedCluster{
		kubernetesVersion: version,
		podSubnet:         cfg.Networking.PodSubnet,
		serviceSubnet:     cfg.Networking.ServiceSubnet,
	}, nil
}

// createAdoptedNodes records the listed control-plane nodes as adopted. They count
// toward desiredCount, and NVKE never changes them: the operator keeps managing
// the control plane. NVKE only reads through their agents (the cluster's
// kubeconfig and kubeadm join strings), which answer once the operator moves each
// node's SPIRE agent to this site and registers host/<node ID>.
func (s *clusterService) createAdoptedNodes(ctx context.Context, tx db.Tx, cluster db.Cluster, adopted []adoptedNode) error {
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
				Adopted:         db.NewCondition(true, "Ran before NVKE adopted the cluster"),
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
