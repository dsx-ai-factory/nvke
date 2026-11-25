// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	capiclient "github.com/dsx-ai-factory/nvke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const (
	kubeVirtVMAPIVersion = "kubevirt.io/v1"
	kubeVirtVMKind       = "VirtualMachine"

	// kubeVirtSVIDHint must match the DSX metadata sidecar's --svid-hint.
	kubeVirtSVIDHint = "dsx-imds-vm"

	metalLBAddressPoolAnnotation = "metallb.universe.tf/address-pool"
	namespaceNameLabel           = "kubernetes.io/metadata.name"

	infraKubeconfigSecretKey = "kubeconfig"
	infraNamespaceSecretKey  = "namespace"

	// kubeVirtAgentFinalizer keeps the KubevirtMachine around long enough to
	// delete the objects this controller created in the infra cluster. They
	// live in another cluster, so no owner reference reaches them.
	kubeVirtAgentFinalizer = "nvke.nvidia.com/kubevirt-agent-endpoint"

	// kubeVirtDialAddressRequeue covers the load balancer assigning an
	// address, which happens in the infra cluster and raises no event here.
	kubeVirtDialAddressRequeue = 15 * time.Second
)

func newKubeVirtMachine() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: capiclient.KubeVirtInfraGroup, Version: capiclient.KubeVirtInfraAPIVersion, Kind: capiclient.KubeVirtMachineKind})
	return obj
}

// KubeVirtSpiffeController gives a KubeVirt VM node's agent an address the
// controller can reach it at, and an identity to present when it gets there.
//
// A VM's pod IP is not routable from here, so the agent is published through a
// LoadBalancer Service in the cluster the VM runs in, and the address lands on
// the Machine for the provisioning workflow to copy onto the node record. A
// node that takes its token from the metadata service also gets a
// ClusterSPIFFEID, so SPIRE issues its virt-launcher pod an SVID.
//
// It is the KubeVirt counterpart of LaunchLayerSpiffeController, but it writes
// to the infra cluster rather than the management cluster, because that is
// where CAPK creates the VMs.
type KubeVirtSpiffeController struct {
	k8sClient client.Client
	cfg       config.Config
	logger    *slog.Logger

	// newInfraClient defaults to client.New.
	newInfraClient func(*rest.Config, client.Options) (client.Client, error)
}

func NewKubeVirtSpiffeController(k8sClient client.Client, cfg config.Config, logger *slog.Logger) *KubeVirtSpiffeController {
	return &KubeVirtSpiffeController{
		k8sClient: k8sClient,
		cfg:       cfg,
		logger:    logger,
	}
}

// KubeVirtMachineCRDInstalled reports whether the management cluster serves the
// KubevirtMachine CRD.
func KubeVirtMachineCRDInstalled(mapper meta.RESTMapper) (bool, error) {
	return crdInstalled(mapper, schema.GroupVersion{Group: capiclient.KubeVirtInfraGroup, Version: capiclient.KubeVirtInfraAPIVersion}, capiclient.KubeVirtMachineKind)
}

func (c *KubeVirtSpiffeController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(newKubeVirtMachine()).
		Watches(
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(kubeVirtMachineRequestsForMachine),
			builder.WithPredicates(machineIdentityWatchPredicate()),
		).
		Named("kubevirt-spiffe").
		Complete(c)
}

func kubeVirtMachineRequestsForMachine(_ context.Context, obj client.Object) []reconcile.Request {
	machine, ok := obj.(*clusterv1.Machine)
	if !ok || !capiclient.IsKubeVirtMachineRef(machine.Spec.InfrastructureRef) || machine.Spec.InfrastructureRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      machine.Spec.InfrastructureRef.Name,
	}}}
}

func (c *KubeVirtSpiffeController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := c.logger.With("namespace", req.Namespace, "kubevirtMachineName", req.Name)

	kubeVirtMachine := newKubeVirtMachine()
	if err := c.k8sClient.Get(ctx, req.NamespacedName, kubeVirtMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get kubevirt machine: %w", err)
	}

	machine, err := util.GetOwnerMachine(ctx, c.k8sClient, metav1.ObjectMeta{
		Namespace:       kubeVirtMachine.GetNamespace(),
		OwnerReferences: kubeVirtMachine.GetOwnerReferences(),
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get owner machine: %w", err)
	}

	if kubeVirtMachine.GetDeletionTimestamp() != nil {
		return ctrl.Result{}, c.reconcileDelete(ctx, logger, kubeVirtMachine, machine)
	}

	if machine == nil {
		logger.Info("Waiting for Machine controller to set OwnerRef on KubevirtMachine")
		return ctrl.Result{}, nil
	}
	nodeID := keys.MachineHostID.Value(machine.GetAnnotations())
	if nodeID == "" {
		logger.Debug("Machine has no host-id annotation, skipping agent endpoint reconciliation")
		return ctrl.Result{}, nil
	}
	tokenSource, tokenSourceSet := keys.MachineAgentTokenSource.Get(machine.GetAnnotations())
	if !tokenSourceSet {
		logger.Info("Waiting for the agent token source annotation to be set on Machine", "nodeID", nodeID)
		return ctrl.Result{}, nil
	}

	// The finalizer goes on before the first infra-cluster write, so a VM
	// deleted mid-reconcile still gets cleaned up.
	if controllerutil.AddFinalizer(kubeVirtMachine, kubeVirtAgentFinalizer) {
		if err := c.k8sClient.Update(ctx, kubeVirtMachine); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer to kubevirt machine: %w", err)
		}
	}

	infraClient, vmNamespace, err := c.infraCluster(ctx, kubeVirtMachine, machine)
	if err != nil {
		return ctrl.Result{}, err
	}

	// The VM owns the Service, so the Service and its MetalLB address go away
	// with the VM even if this controller's cleanup never runs.
	vm := &unstructured.Unstructured{}
	vm.SetAPIVersion(kubeVirtVMAPIVersion)
	vm.SetKind(kubeVirtVMKind)
	if err := infraClient.Get(ctx, client.ObjectKey{Namespace: vmNamespace, Name: kubeVirtMachine.GetName()}, vm); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Waiting for CAPK to create the VirtualMachine", "vmNamespace", vmNamespace)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get VirtualMachine %s/%s: %w", vmNamespace, kubeVirtMachine.GetName(), err)
	}

	service, err := c.buildService(kubeVirtMachine.GetName(), req.Namespace, nodeID, vmNamespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	service.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: kubeVirtVMAPIVersion,
		Kind:       kubeVirtVMKind,
		Name:       vm.GetName(),
		UID:        vm.GetUID(),
	}}
	if err := infraClient.Patch(ctx, service, client.Apply,
		client.FieldOwner(constants.FieldManagerKubeVirtSpiffe), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply node agent Service %s/%s: %w", service.Namespace, service.Name, err)
	}

	if db.NodeAgentTokenSource(tokenSource) == db.NodeAgentTokenSourceMetadataService {
		registration := c.buildClusterSPIFFEID(kubeVirtMachine.GetName(), req.Namespace, nodeID, vmNamespace)
		if err := infraClient.Patch(ctx, registration, client.Apply,
			client.FieldOwner(constants.FieldManagerKubeVirtSpiffe), client.ForceOwnership); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to apply node agent ClusterSPIFFEID %s: %w", registration.Name, err)
		}
	}

	if err := infraClient.Get(ctx, client.ObjectKeyFromObject(service), service); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get node agent Service %s/%s: %w", service.Namespace, service.Name, err)
	}
	dialAddress := loadBalancerAddress(service)
	if dialAddress == "" {
		// The Service event that assigns the address arrives in the infra
		// cluster, which this controller does not watch.
		logger.Info("Waiting for a load balancer address", "service", service.Name)
		return ctrl.Result{RequeueAfter: kubeVirtDialAddressRequeue}, nil
	}

	return ctrl.Result{}, c.recordDialAddress(ctx, logger, machine, dialAddress)
}

// reconcileDelete removes what this controller created in the infra cluster
// and drops the finalizer. A KubevirtMachine with no owner Machine left, or a
// cluster already gone, still has to release the finalizer.
func (c *KubeVirtSpiffeController) reconcileDelete(
	ctx context.Context, logger *slog.Logger, kubeVirtMachine *unstructured.Unstructured, machine *clusterv1.Machine,
) error {
	if !controllerutil.ContainsFinalizer(kubeVirtMachine, kubeVirtAgentFinalizer) {
		return nil
	}
	if machine != nil {
		if nodeID := keys.MachineHostID.Value(machine.GetAnnotations()); nodeID != "" {
			infraClient, vmNamespace, err := c.infraCluster(ctx, kubeVirtMachine, machine)
			if err != nil {
				return err
			}
			name := kubeVirtAgentObjectName(nodeID)
			objects := []client.Object{
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: vmNamespace}},
				&spirev1alpha1.ClusterSPIFFEID{ObjectMeta: metav1.ObjectMeta{Name: name}},
			}
			for _, obj := range objects {
				if err := infraClient.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
					return fmt.Errorf("failed to delete node agent %T %s: %w", obj, obj.GetName(), err)
				}
			}
			logger.Info("Deleted node agent endpoint", "nodeID", nodeID, "vmNamespace", vmNamespace)
		}
	}
	controllerutil.RemoveFinalizer(kubeVirtMachine, kubeVirtAgentFinalizer)
	if err := c.k8sClient.Update(ctx, kubeVirtMachine); err != nil {
		return fmt.Errorf("failed to remove finalizer from kubevirt machine: %w", err)
	}
	return nil
}

// recordDialAddress puts the address on the Machine for the provisioning
// workflow to copy onto the node record.
func (c *KubeVirtSpiffeController) recordDialAddress(
	ctx context.Context, logger *slog.Logger, machine *clusterv1.Machine, dialAddress string,
) error {
	if keys.MachineAgentDialAddress.Value(machine.GetAnnotations()) == dialAddress {
		return nil
	}
	applyMachine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      machine.Name,
			Namespace: machine.Namespace,
			Annotations: map[string]string{
				keys.MachineAgentDialAddress.String(): dialAddress,
			},
		},
	}
	logger.Info("Applying Machine annotation", "machineName", applyMachine.Name,
		"annotation", keys.MachineAgentDialAddress.String(), "dialAddress", dialAddress)
	if err := c.k8sClient.Patch(ctx, applyMachine, client.Apply,
		client.FieldOwner(constants.FieldManagerKubeVirtSpiffe), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply machine: %w", err)
	}
	return nil
}

func loadBalancerAddress(service *corev1.Service) string {
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			return ingress.IP
		}
	}
	return ""
}

// kubeVirtAgentObjectName must stay within the 63-character Service name limit.
func kubeVirtAgentObjectName(nodeID string) string {
	return "nvke-node-agent-" + nodeID
}

// podSelector selects the virt-launcher pod of this node's VM. CAPK's labels
// name the KubevirtMachine, whose name is the reusable node name, so the node
// ID is what keeps a recycled name from matching the previous node's pod.
func podSelector(machineName, machineNamespace, nodeID string) map[string]string {
	return map[string]string{
		capiclient.KubeVirtMachineNameLabel:      machineName,
		capiclient.KubeVirtMachineNamespaceLabel: machineNamespace,
		capiclient.NodeIDLabel:                   nodeID,
	}
}

func (c *KubeVirtSpiffeController) buildService(machineName, machineNamespace, nodeID, vmNamespace string) (*corev1.Service, error) {
	port, err := strconv.ParseInt(c.cfg.NodeAgent.Port, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid node agent port %q: %w", c.cfg.NodeAgent.Port, err)
	}
	service := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeVirtAgentObjectName(nodeID),
			Namespace: vmNamespace,
			Labels:    map[string]string{capiclient.NodeIDLabel: nodeID},
		},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
			Selector:              podSelector(machineName, machineNamespace, nodeID),
			Ports: []corev1.ServicePort{{
				Name:       "node-agent",
				Protocol:   corev1.ProtocolTCP,
				Port:       int32(port),
				TargetPort: intstr.FromInt32(int32(port)),
			}},
		},
	}
	if pool := c.cfg.NodeAgent.KubeVirt.AddressPool; pool != "" {
		service.Annotations = map[string]string{metalLBAddressPoolAnnotation: pool}
	}
	return service, nil
}

func (c *KubeVirtSpiffeController) buildClusterSPIFFEID(
	machineName, machineNamespace, nodeID, vmNamespace string,
) *spirev1alpha1.ClusterSPIFFEID {
	tlsJWT := c.cfg.NodeAgent.TLSJWT
	selector := podSelector(machineName, machineNamespace, nodeID)
	labels := map[string]string{capiclient.NodeIDLabel: nodeID}
	return &spirev1alpha1.ClusterSPIFFEID{
		TypeMeta: metav1.TypeMeta{APIVersion: spirev1alpha1.SchemeGroupVersion.String(), Kind: "ClusterSPIFFEID"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   kubeVirtAgentObjectName(nodeID),
			Labels: labels,
		},
		Spec: spirev1alpha1.ClusterSPIFFEIDSpec{
			SPIFFEIDTemplate: fmt.Sprintf("spiffe://{{ .TrustDomain }}/tenant/%s/service/%s/vm/%s",
				tlsJWT.SubjectTenant, tlsJWT.SubjectService, nodeID),
			ClassName:         c.cfg.NodeAgent.KubeVirt.SPIFFEIDClassName,
			Hint:              kubeVirtSVIDHint,
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{namespaceNameLabel: vmNamespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: selector},
		},
	}
}

// infraCluster resolves the cluster and namespace CAPK creates the VMs in, the
// same way CAPK does: from the KubevirtCluster's infraClusterSecretRef, or the
// management cluster when it names none. The KubevirtMachine's VM template
// namespace wins over the Secret's namespace when it sets one.
func (c *KubeVirtSpiffeController) infraCluster(
	ctx context.Context, kubeVirtMachine *unstructured.Unstructured, machine *clusterv1.Machine,
) (client.Client, string, error) {
	infraClient := c.k8sClient
	namespace := kubeVirtMachine.GetNamespace()

	ref, found, err := c.infraClusterSecretRef(ctx, machine)
	if err != nil {
		return nil, "", err
	}
	if found {
		infraClient, namespace, err = c.connectInfraCluster(ctx, ref, kubeVirtMachine.GetNamespace())
		if err != nil {
			return nil, "", err
		}
	}

	templateNamespace, _, err := unstructured.NestedString(kubeVirtMachine.Object,
		"spec", "virtualMachineTemplate", "metadata", "namespace")
	if err != nil {
		return nil, "", fmt.Errorf("failed to read KubevirtMachine %s/%s VM template namespace: %w",
			kubeVirtMachine.GetNamespace(), kubeVirtMachine.GetName(), err)
	}
	if templateNamespace != "" {
		namespace = templateNamespace
	}
	return infraClient, namespace, nil
}

// infraClusterSecretRef reads the Secret the Machine's KubevirtCluster names,
// reporting false when it names none and the VMs run in the management cluster.
func (c *KubeVirtSpiffeController) infraClusterSecretRef(
	ctx context.Context, machine *clusterv1.Machine,
) (client.ObjectKey, bool, error) {
	kubeVirtCluster := &unstructured.Unstructured{}
	kubeVirtCluster.SetGroupVersionKind(schema.GroupVersionKind{
		Group: capiclient.KubeVirtInfraGroup, Version: capiclient.KubeVirtInfraAPIVersion, Kind: capiclient.KubeVirtClusterKind,
	})
	key := client.ObjectKey{Namespace: machine.Namespace, Name: machine.Spec.ClusterName}
	if err := c.k8sClient.Get(ctx, key, kubeVirtCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return client.ObjectKey{}, false, nil
		}
		return client.ObjectKey{}, false, fmt.Errorf("failed to get KubevirtCluster %s: %w", key, err)
	}
	ref, found, err := unstructured.NestedStringMap(kubeVirtCluster.Object, "spec", "infraClusterSecretRef")
	if err != nil {
		return client.ObjectKey{}, false, fmt.Errorf("failed to read infraClusterSecretRef on %s: %w", key, err)
	}
	if !found || ref["name"] == "" {
		return client.ObjectKey{}, false, nil
	}
	namespace := ref["namespace"]
	if namespace == "" {
		namespace = machine.Namespace
	}
	return client.ObjectKey{Namespace: namespace, Name: ref["name"]}, true, nil
}

func (c *KubeVirtSpiffeController) connectInfraCluster(
	ctx context.Context, ref client.ObjectKey, defaultNamespace string,
) (client.Client, string, error) {
	secret := &corev1.Secret{}
	if err := c.k8sClient.Get(ctx, ref, secret); err != nil {
		return nil, "", fmt.Errorf("failed to get infra cluster kubeconfig Secret %s: %w", ref, err)
	}
	kubeconfig, ok := secret.Data[infraKubeconfigSecretKey]
	if !ok {
		return nil, "", fmt.Errorf("infra cluster Secret %s has no %q key", ref, infraKubeconfigSecretKey)
	}
	clientConfig, err := clientcmd.NewClientConfigFromBytes(kubeconfig)
	if err != nil {
		return nil, "", fmt.Errorf("failed to parse infra cluster kubeconfig: %w", err)
	}
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("failed to build infra cluster REST config: %w", err)
	}
	namespace, _, err := clientConfig.Namespace()
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve infra cluster namespace: %w", err)
	}
	if namespace == "" {
		namespace = defaultNamespace
	}
	if ns, ok := secret.Data[infraNamespaceSecretKey]; ok {
		namespace = strings.TrimSpace(string(ns))
	}

	scheme := runtime.NewScheme()
	if err := errors.Join(clientgoscheme.AddToScheme(scheme), spirev1alpha1.AddToScheme(scheme)); err != nil {
		return nil, "", fmt.Errorf("failed to build infra cluster client scheme: %w", err)
	}
	newClient := c.newInfraClient
	if newClient == nil {
		newClient = client.New
	}
	infraClient, err := newClient(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, "", fmt.Errorf("failed to create infra cluster client: %w", err)
	}
	return infraClient, namespace, nil
}
