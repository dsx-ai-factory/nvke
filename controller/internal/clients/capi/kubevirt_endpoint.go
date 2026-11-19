// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	// kubeVirtSVIDHint matches the --svid-hint of the DSX metadata sidecar
	// injected into virt-launcher pods.
	kubeVirtSVIDHint = "dsx-imds-vm"

	metalLBAddressPoolAnnotation = "metallb.universe.tf/address-pool"
	namespaceNameLabel           = "kubernetes.io/metadata.name"
	nodeIDLabel                  = "nvke.nvidia.com/node-id"

	infraKubeconfigSecretKey = "kubeconfig"
	infraNamespaceSecretKey  = "namespace"
)

// KubeVirtAgentEndpoint creates and deletes the infra cluster resources a
// KubeVirt VM node agent needs: a LoadBalancer Service the controller dials,
// since VM pod addresses are not routable from the controller, and, for
// TLS/JWT nodes, a ClusterSPIFFEID that registers the agent's DSX identity
// /vm/<node ID>.
type KubeVirtAgentEndpoint struct {
	// ManagementClient reads the KubevirtCluster's infra kubeconfig Secret.
	// It is also the infra cluster client when the KubevirtCluster names
	// no Secret, since CAPK then creates VMs in the management cluster.
	ManagementClient client.Client
	// Namespace is the management namespace holding CAPI objects.
	Namespace        string
	Config           config.Config
	FieldManagerName string
	// NewClient builds the infra cluster client from its REST config.
	// Nil uses client.New.
	NewClient func(*rest.Config, client.Options) (client.Client, error)
}

// Ensure applies the node's Service and registration and returns the
// Service's load balancer address, or "" until one is assigned.
func (e KubeVirtAgentEndpoint) Ensure(ctx context.Context, cluster db.Cluster, node db.Node) (string, error) {
	infraClient, vmNamespace, err := e.infraClusterClient(ctx, cluster)
	if err != nil {
		return "", err
	}
	service, err := e.buildService(node, vmNamespace)
	if err != nil {
		return "", err
	}
	if err := infraClient.Patch(ctx, service, client.Apply, client.FieldOwner(e.FieldManagerName), client.ForceOwnership); err != nil {
		return "", fmt.Errorf("failed to apply node agent Service %s/%s: %w", service.Namespace, service.Name, err)
	}
	if node.NodeAgentTLSJWTEnabled {
		registration := e.buildClusterSPIFFEID(node, vmNamespace)
		if err := infraClient.Patch(ctx, registration, client.Apply, client.FieldOwner(e.FieldManagerName), client.ForceOwnership); err != nil {
			return "", fmt.Errorf("failed to apply node agent ClusterSPIFFEID %s: %w", registration.Name, err)
		}
	}

	if err := infraClient.Get(ctx, client.ObjectKeyFromObject(service), service); err != nil {
		return "", fmt.Errorf("failed to get node agent Service %s/%s: %w", service.Namespace, service.Name, err)
	}
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			return ingress.IP, nil
		}
	}
	return "", nil
}

// Delete removes the node's Service and registration. Missing objects are
// not an error.
func (e KubeVirtAgentEndpoint) Delete(ctx context.Context, cluster db.Cluster, node db.Node) error {
	infraClient, vmNamespace, err := e.infraClusterClient(ctx, cluster)
	if err != nil {
		return err
	}
	objects := []client.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: kubeVirtAgentObjectName(node), Namespace: vmNamespace}},
		&spirev1alpha1.ClusterSPIFFEID{ObjectMeta: metav1.ObjectMeta{Name: kubeVirtAgentObjectName(node)}},
	}
	for _, obj := range objects {
		if err := infraClient.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete node agent %T %s: %w", obj, obj.GetName(), err)
		}
	}
	return nil
}

// kubeVirtAgentObjectName names the node's Service and ClusterSPIFFEID. The
// node ID keeps it unique across management namespaces and within the
// 63-character Service name limit.
func kubeVirtAgentObjectName(node db.Node) string {
	return "nvke-node-agent-" + node.ID.String()
}

// kubeVirtPodSelector selects the virt-launcher pod CAPK creates for the
// node's KubevirtMachine, which NVKE names after the node.
func (e KubeVirtAgentEndpoint) kubeVirtPodSelector(node db.Node) map[string]string {
	return map[string]string{
		kubeVirtMachineNameLabel:      node.Name,
		kubeVirtMachineNamespaceLabel: e.Namespace,
	}
}

func (e KubeVirtAgentEndpoint) buildService(node db.Node, vmNamespace string) (*corev1.Service, error) {
	port, err := strconv.ParseInt(e.Config.NodeAgent.Port, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid node agent port %q: %w", e.Config.NodeAgent.Port, err)
	}
	service := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeVirtAgentObjectName(node),
			Namespace: vmNamespace,
			Labels:    map[string]string{nodeIDLabel: node.ID.String()},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			// MetalLB then advertises the address only from the host
			// running the VM.
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
			Selector:              e.kubeVirtPodSelector(node),
			Ports: []corev1.ServicePort{{
				Name:       "node-agent",
				Protocol:   corev1.ProtocolTCP,
				Port:       int32(port),
				TargetPort: intstr.FromInt32(int32(port)),
			}},
		},
	}
	if pool := e.Config.NodeAgent.KubeVirt.AddressPool; pool != "" {
		service.Annotations = map[string]string{metalLBAddressPoolAnnotation: pool}
	}
	return service, nil
}

func (e KubeVirtAgentEndpoint) buildClusterSPIFFEID(node db.Node, vmNamespace string) *spirev1alpha1.ClusterSPIFFEID {
	tlsJWT := e.Config.NodeAgent.TLSJWT
	return &spirev1alpha1.ClusterSPIFFEID{
		TypeMeta: metav1.TypeMeta{APIVersion: spirev1alpha1.SchemeGroupVersion.String(), Kind: "ClusterSPIFFEID"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   kubeVirtAgentObjectName(node),
			Labels: map[string]string{nodeIDLabel: node.ID.String()},
		},
		Spec: spirev1alpha1.ClusterSPIFFEIDSpec{
			// DSX Identity exchanges this local ID for the /vm/<node ID>
			// subject the controller accepts.
			SPIFFEIDTemplate: fmt.Sprintf("spiffe://{{ .TrustDomain }}/tenant/%s/service/%s/vm/%s",
				tlsJWT.SubjectTenant, tlsJWT.SubjectService, node.ID),
			ClassName:         e.Config.NodeAgent.KubeVirt.SPIFFEIDClassName,
			Hint:              kubeVirtSVIDHint,
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{namespaceNameLabel: vmNamespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: e.kubeVirtPodSelector(node)},
		},
	}
}

// infraClusterClient returns a client for the cluster CAPK creates the
// cluster's VMs in, and the namespace it creates them in, resolved the way
// CAPK resolves them: from the KubevirtCluster's infraClusterSecretRef, or the
// management cluster and namespace when it names no Secret.
func (e KubeVirtAgentEndpoint) infraClusterClient(ctx context.Context, cluster db.Cluster) (client.Client, string, error) {
	secretRef, found, err := kubeVirtInfraClusterSecretRef(cluster)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return e.ManagementClient, e.Namespace, nil
	}
	if secretRef.Namespace == "" {
		secretRef.Namespace = e.Namespace
	}

	secret := &corev1.Secret{}
	if err := e.ManagementClient.Get(ctx, client.ObjectKey{Namespace: secretRef.Namespace, Name: secretRef.Name}, secret); err != nil {
		return nil, "", fmt.Errorf("failed to get infra cluster kubeconfig Secret %s/%s: %w", secretRef.Namespace, secretRef.Name, err)
	}
	kubeconfig, ok := secret.Data[infraKubeconfigSecretKey]
	if !ok {
		return nil, "", fmt.Errorf("infra cluster Secret %s/%s has no %q key", secretRef.Namespace, secretRef.Name, infraKubeconfigSecretKey)
	}
	clientConfig, err := clientcmd.NewClientConfigFromBytes(kubeconfig)
	if err != nil {
		return nil, "", fmt.Errorf("failed to parse infra cluster kubeconfig: %w", err)
	}
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("failed to build infra cluster REST config: %w", err)
	}
	vmNamespace, _, err := clientConfig.Namespace()
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve infra cluster namespace: %w", err)
	}
	if ns, ok := secret.Data[infraNamespaceSecretKey]; ok {
		vmNamespace = strings.TrimSpace(string(ns))
	}

	scheme := runtime.NewScheme()
	if err := errors.Join(clientgoscheme.AddToScheme(scheme), spirev1alpha1.AddToScheme(scheme)); err != nil {
		return nil, "", fmt.Errorf("failed to build infra cluster client scheme: %w", err)
	}
	newClient := e.NewClient
	if newClient == nil {
		newClient = client.New
	}
	infraClient, err := newClient(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, "", fmt.Errorf("failed to create infra cluster client: %w", err)
	}
	return infraClient, vmNamespace, nil
}

type secretRef struct {
	Name      string
	Namespace string
}

// kubeVirtInfraClusterSecretRef reads spec.infraClusterSecretRef from the
// cluster's KubevirtCluster manifest, reporting false when it names none.
func kubeVirtInfraClusterSecretRef(cluster db.Cluster) (secretRef, bool, error) {
	if cluster.Capi == nil || len(cluster.Capi.InfraCluster) == 0 {
		return secretRef{}, false, errors.New("cluster has no infra cluster manifest")
	}
	infraCluster, err := parseManifest(cluster.Capi.InfraCluster)
	if err != nil {
		return secretRef{}, false, fmt.Errorf("failed to parse infra cluster manifest: %w", err)
	}
	if !isKubeVirtObject(infraCluster, kubeVirtClusterKind) {
		return secretRef{}, false, fmt.Errorf("KubeVirt machines need a %s infra cluster, got %s", kubeVirtClusterKind, infraCluster.GetKind())
	}
	ref, found, err := unstructured.NestedStringMap(infraCluster.Object, "spec", "infraClusterSecretRef")
	if err != nil {
		return secretRef{}, false, fmt.Errorf("failed to read infraClusterSecretRef: %w", err)
	}
	if !found || ref["name"] == "" {
		return secretRef{}, false, nil
	}
	return secretRef{Name: ref["name"], Namespace: ref["namespace"]}, true, nil
}
