// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"log/slog"
	"testing"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	capiclient "github.com/dsx-ai-factory/nvke/controller/internal/clients/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const (
	kubeVirtTestHostID      = "3f2a9c11-4b7d-4a0e-8c3f-1d2e5a6b7c80"
	kubeVirtTestDialAddress = "10.90.0.42"
	kubeVirtTestVMNamespace = "vms"
)

func TestUnit_KubeVirtSpiffeControllerReconcile(t *testing.T) {
	t.Parallel()

	type parameters struct {
		tokenSource      db.NodeAgentTokenSource
		noTokenSource    bool
		missingHostID    bool
		noOwnerRef       bool
		noVM             bool
		noLoadBalancer   bool
		wantRequeue      bool
		wantService      bool
		wantRegistration bool
		wantDialAddress  string
	}

	tests := map[string]parameters{
		"publishes the agent endpoint and records its address": {
			tokenSource: db.NodeAgentTokenSourceMetadataService,
			wantService: true, wantRegistration: true, wantDialAddress: kubeVirtTestDialAddress,
		},
		"registers no SPIFFE ID for a node that exchanges its own token": {
			tokenSource: db.NodeAgentTokenSourceTokenExchange,
			wantService: true, wantDialAddress: kubeVirtTestDialAddress,
		},
		"requeues while the load balancer has no address": {
			tokenSource: db.NodeAgentTokenSourceMetadataService, noLoadBalancer: true,
			wantService: true, wantRegistration: true, wantRequeue: true,
		},
		"waits for CAPK to create the VirtualMachine": {
			tokenSource: db.NodeAgentTokenSourceMetadataService, noVM: true,
		},
		"waits for the Machine owner reference": {
			tokenSource: db.NodeAgentTokenSourceMetadataService, noOwnerRef: true,
		},
		"waits for the token source annotation": {
			noTokenSource: true,
		},
		"skips a Machine without a host ID": {
			tokenSource: db.NodeAgentTokenSourceMetadataService, missingHostID: true,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			machine := newMachineForKubeVirtSpiffeTest(params.tokenSource)
			if params.noTokenSource {
				keys.MachineAgentTokenSource.Delete(machine.Annotations)
			}
			if params.missingHostID {
				keys.MachineHostID.Delete(machine.Annotations)
			}
			kubeVirtMachine := newKubeVirtMachineForSpiffeTest(!params.noOwnerRef)

			objects := []client.Object{machine, kubeVirtMachine}
			if !params.noVM {
				objects = append(objects, newVirtualMachineForSpiffeTest())
			}
			// Seeded only where the controller is expected to get as far as
			// reading an address, so the cases that want no Service can tell
			// a Service the controller never created from one it did.
			if params.wantService && !params.noLoadBalancer {
				objects = append(objects, newAgentServiceForSpiffeTest(kubeVirtTestDialAddress))
			}
			controller := newKubeVirtSpiffeControllerForTest(t, objects...)

			result, err := controller.Reconcile(t.Context(), ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(kubeVirtMachine),
			})
			require.NoError(t, err)
			if params.wantRequeue {
				require.Equal(t, kubeVirtDialAddressRequeue, result.RequeueAfter)
			} else {
				require.Zero(t, result.RequeueAfter)
			}

			service := &corev1.Service{}
			serviceErr := controller.k8sClient.Get(t.Context(), types.NamespacedName{
				Namespace: kubeVirtTestVMNamespace, Name: kubeVirtAgentObjectName(kubeVirtTestHostID),
			}, service)
			if params.wantService {
				require.NoError(t, serviceErr)
				require.Equal(t, corev1.ServiceTypeLoadBalancer, service.Spec.Type)
			} else {
				require.True(t, apierrors.IsNotFound(serviceErr), "expected no Service, got %v", serviceErr)
			}

			registration := &spirev1alpha1.ClusterSPIFFEID{}
			registrationErr := controller.k8sClient.Get(t.Context(), types.NamespacedName{
				Name: kubeVirtAgentObjectName(kubeVirtTestHostID),
			}, registration)
			if params.wantRegistration {
				require.NoError(t, registrationErr)
			} else {
				require.True(t, apierrors.IsNotFound(registrationErr), "expected no ClusterSPIFFEID, got %v", registrationErr)
			}

			gotMachine := &clusterv1.Machine{}
			require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
			require.Equal(t, params.wantDialAddress, keys.MachineAgentDialAddress.Value(gotMachine.Annotations))
		})
	}
}

// Node names are reused, so a Service selecting only CAPK's machine-name label
// would send one node's agent traffic to its predecessor's pod.
func TestUnit_KubeVirtSpiffeControllerSelectsThePodByNodeID(t *testing.T) {
	t.Parallel()
	kubeVirtMachine := newKubeVirtMachineForSpiffeTest(true)
	controller := newKubeVirtSpiffeControllerForTest(t,
		newMachineForKubeVirtSpiffeTest(db.NodeAgentTokenSourceMetadataService),
		kubeVirtMachine,
		newVirtualMachineForSpiffeTest(),
		newAgentServiceForSpiffeTest(kubeVirtTestDialAddress),
	)

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(kubeVirtMachine)})
	require.NoError(t, err)

	service := &corev1.Service{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), types.NamespacedName{
		Namespace: kubeVirtTestVMNamespace, Name: kubeVirtAgentObjectName(kubeVirtTestHostID),
	}, service))
	require.Equal(t, kubeVirtTestHostID, service.Spec.Selector[capiclient.NodeIDLabel])

	registration := &spirev1alpha1.ClusterSPIFFEID{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), types.NamespacedName{
		Name: kubeVirtAgentObjectName(kubeVirtTestHostID),
	}, registration))
	require.Equal(t, kubeVirtTestHostID, registration.Spec.PodSelector.MatchLabels[capiclient.NodeIDLabel])
}

// The ClusterSPIFFEID is cluster-scoped and the Service lives in another
// cluster, so neither is reachable by owner reference from the KubevirtMachine.
func TestUnit_KubeVirtSpiffeControllerDeletesTheEndpointItCreated(t *testing.T) {
	t.Parallel()
	kubeVirtMachine := newKubeVirtMachineForSpiffeTest(true)
	controller := newKubeVirtSpiffeControllerForTest(t,
		newMachineForKubeVirtSpiffeTest(db.NodeAgentTokenSourceMetadataService),
		kubeVirtMachine,
		newVirtualMachineForSpiffeTest(),
		newAgentServiceForSpiffeTest(kubeVirtTestDialAddress),
	)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(kubeVirtMachine)}

	_, err := controller.Reconcile(t.Context(), request)
	require.NoError(t, err)

	// The finalizer must be in place, or deletion would complete before the
	// controller could clean up.
	require.NoError(t, controller.k8sClient.Get(t.Context(), request.NamespacedName, kubeVirtMachine))
	require.Contains(t, kubeVirtMachine.GetFinalizers(), kubeVirtAgentFinalizer)

	require.NoError(t, controller.k8sClient.Delete(t.Context(), kubeVirtMachine))
	_, err = controller.Reconcile(t.Context(), request)
	require.NoError(t, err)

	require.True(t, apierrors.IsNotFound(controller.k8sClient.Get(t.Context(), types.NamespacedName{
		Namespace: kubeVirtTestVMNamespace, Name: kubeVirtAgentObjectName(kubeVirtTestHostID),
	}, &corev1.Service{})))
	require.True(t, apierrors.IsNotFound(controller.k8sClient.Get(t.Context(), types.NamespacedName{
		Name: kubeVirtAgentObjectName(kubeVirtTestHostID),
	}, &spirev1alpha1.ClusterSPIFFEID{})))
	// Dropping the finalizer lets the KubevirtMachine go.
	require.True(t, apierrors.IsNotFound(controller.k8sClient.Get(t.Context(), request.NamespacedName, newKubeVirtMachine())))
}

func TestUnit_KubeVirtSpiffeControllerReconcileIgnoresMissingKubeVirtMachine(t *testing.T) {
	t.Parallel()
	controller := newKubeVirtSpiffeControllerForTest(t)

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "cluster-test", Name: "worker-gone",
	}})

	require.NoError(t, err)
}

func TestUnit_KubeVirtSpiffeControllerMachineWatchMapping(t *testing.T) {
	t.Parallel()
	type parameters struct {
		apiGroup string
		kind     string
		name     string
		other    bool
		wantName string
	}
	tests := map[string]parameters{
		"routes a Machine to its KubevirtMachine infrastructure reference": {
			apiGroup: capiclient.KubeVirtInfraGroup, kind: capiclient.KubeVirtMachineKind,
			name: "kv-infra-1", wantName: "kv-infra-1",
		},
		"ignores another infrastructure kind": {
			apiGroup: capiclient.KubeVirtInfraGroup, kind: "NicoMachine", name: "nico-infra-1",
		},
		"ignores another API group": {
			apiGroup: "example.org", kind: capiclient.KubeVirtMachineKind, name: "kv-infra-1",
		},
		"ignores a missing infrastructure name": {
			apiGroup: capiclient.KubeVirtInfraGroup, kind: capiclient.KubeVirtMachineKind,
		},
		"ignores a non-Machine object": {other: true},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var obj client.Object = newMachineForKubeVirtSpiffeTest(db.NodeAgentTokenSourceMetadataService)
			if params.other {
				obj = newKubeVirtMachineForSpiffeTest(false)
			} else {
				obj.(*clusterv1.Machine).Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
					APIGroup: params.apiGroup, Kind: params.kind, Name: params.name,
				}
			}
			requests := kubeVirtMachineRequestsForMachine(t.Context(), obj)
			if params.wantName == "" {
				require.Empty(t, requests)
				return
			}
			require.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{
				Namespace: "cluster-test", Name: params.wantName,
			}}}, requests)
		})
	}
}

func newMachineForKubeVirtSpiffeTest(tokenSource db.NodeAgentTokenSource) *clusterv1.Machine {
	annotations := map[string]string{
		keys.MachineHostID.String():          kubeVirtTestHostID,
		keys.MachineAgentConnection.String(): "spiffe",
		keys.MachineSpiffeEnabled.String():   "true",
	}
	if tokenSource != "" {
		annotations[keys.MachineAgentTokenSource.String()] = string(tokenSource)
	}
	return &clusterv1.Machine{
		TypeMeta:   metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine"},
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "cluster-test", Annotations: annotations},
		Spec: clusterv1.MachineSpec{
			ClusterName: "tenant",
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: capiclient.KubeVirtInfraGroup, Kind: capiclient.KubeVirtMachineKind, Name: "worker-0",
			},
		},
	}
}

func newKubeVirtMachineForSpiffeTest(ownerRef bool) *unstructured.Unstructured {
	kubeVirtMachine := newKubeVirtMachine()
	kubeVirtMachine.SetNamespace("cluster-test")
	kubeVirtMachine.SetName("worker-0")
	require := map[string]any{"metadata": map[string]any{"namespace": kubeVirtTestVMNamespace}}
	kubeVirtMachine.Object["spec"] = map[string]any{"virtualMachineTemplate": require}
	if ownerRef {
		isController := true
		kubeVirtMachine.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
			Name:       "worker-0",
			Controller: &isController,
		}})
	}
	return kubeVirtMachine
}

func newVirtualMachineForSpiffeTest() *unstructured.Unstructured {
	vm := &unstructured.Unstructured{Object: map[string]any{}}
	vm.SetAPIVersion(kubeVirtVMAPIVersion)
	vm.SetKind(kubeVirtVMKind)
	vm.SetNamespace(kubeVirtTestVMNamespace)
	vm.SetName("worker-0")
	vm.SetUID("vm-uid-1")
	return vm
}

// newAgentServiceForSpiffeTest stands in for MetalLB, which assigns the
// address in a live cluster.
func newAgentServiceForSpiffeTest(address string) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name: kubeVirtAgentObjectName(kubeVirtTestHostID), Namespace: kubeVirtTestVMNamespace,
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: address}}},
		},
	}
}

// The VMs run in the management cluster, so the KubevirtCluster names no
// infra cluster Secret and the controller writes through its own client.
func newKubeVirtSpiffeControllerForTest(t *testing.T, objects ...client.Object) *KubeVirtSpiffeController {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, spirev1alpha1.AddToScheme(scheme))

	kubeVirtCluster := &unstructured.Unstructured{Object: map[string]any{}}
	kubeVirtCluster.SetAPIVersion(capiclient.KubeVirtInfraGroup + "/" + capiclient.KubeVirtInfraAPIVersion)
	kubeVirtCluster.SetKind(capiclient.KubeVirtClusterKind)
	kubeVirtCluster.SetNamespace("cluster-test")
	kubeVirtCluster.SetName("tenant")

	cfg := config.Config{}
	cfg.NodeAgent.Port = "9000"
	cfg.NodeAgent.KubeVirt.AddressPool = "vm-pool"
	cfg.NodeAgent.KubeVirt.SPIFFEIDClassName = "spire"
	cfg.NodeAgent.TLSJWT.SubjectTenant = "org-1"
	cfg.NodeAgent.TLSJWT.SubjectService = "nvke"

	return &KubeVirtSpiffeController{
		k8sClient: fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(append(objects, kubeVirtCluster)...).Build(),
		cfg:    cfg,
		logger: slog.New(slog.DiscardHandler),
	}
}
