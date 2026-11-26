// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeletcsr

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/source"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const (
	testClusterName = "tenant-a"
	testNamespace   = "nke-system"
	testNodeName    = "worker-a"
	testProviderID  = "nico://instance-a"
	testNodeIP      = "192.0.2.10"
)

func TestUnit_ValidateServingCSR(t *testing.T) {
	t.Parallel()

	trusted := trustedIdentities{
		dnsNames:    map[string]struct{}{testNodeName: {}, "host-a": {}},
		ipAddresses: []net.IP{net.ParseIP(testNodeIP)},
	}
	validRequest := servingCSRRequest(t, kubeletNodeNamePrefix+testNodeName, []string{kubeletNodeGroup}, []string{testNodeName}, []net.IP{net.ParseIP(testNodeIP)}, nil)

	tests := map[string]struct {
		usages     []certificatesv1.KeyUsage
		groups     []string
		request    []byte
		signerName string
		username   string
		wantReason string
	}{
		"digital signature and server auth": {
			request: validRequest,
			groups:  []string{kubeletNodeGroup},
			usages:  defaultServingUsages(),
		},
		"missing system nodes group": {
			request:    validRequest,
			groups:     []string{"system:authenticated"},
			usages:     defaultServingUsages(),
			wantReason: "spec.groups does not include system:nodes",
		},
		"client auth rejected": {
			request: validRequest,
			groups:  []string{kubeletNodeGroup},
			usages: []certificatesv1.KeyUsage{
				certificatesv1.UsageDigitalSignature,
				certificatesv1.UsageClientAuth,
			},
			wantReason: "spec.usages must be digital signature and server auth, with key encipherment optional",
		},
		"wrong common name": {
			request:    servingCSRRequest(t, kubeletNodeNamePrefix+"other", []string{kubeletNodeGroup}, []string{testNodeName}, nil, nil),
			groups:     []string{kubeletNodeGroup},
			usages:     defaultServingUsages(),
			wantReason: "subject common name is not system:node:worker-a",
		},
		"untrusted dns": {
			request:    servingCSRRequest(t, kubeletNodeNamePrefix+testNodeName, []string{kubeletNodeGroup}, []string{"other"}, nil, nil),
			groups:     []string{kubeletNodeGroup},
			usages:     defaultServingUsages(),
			wantReason: "DNS SAN is not trusted for the Machine",
		},
		"wildcard dns": {
			request:    servingCSRRequest(t, kubeletNodeNamePrefix+testNodeName, []string{kubeletNodeGroup}, []string{"*.example.com"}, nil, nil),
			groups:     []string{kubeletNodeGroup},
			usages:     defaultServingUsages(),
			wantReason: "DNS SAN is not trusted for the Machine",
		},
		"untrusted ip": {
			request:    servingCSRRequest(t, kubeletNodeNamePrefix+testNodeName, []string{kubeletNodeGroup}, nil, []net.IP{net.ParseIP("192.0.2.11")}, nil),
			groups:     []string{kubeletNodeGroup},
			usages:     defaultServingUsages(),
			wantReason: "IP SAN is not trusted for the Machine",
		},
		"uri san": {
			request: servingCSRRequest(t, kubeletNodeNamePrefix+testNodeName, []string{kubeletNodeGroup}, []string{testNodeName}, nil, []*url.URL{
				{Scheme: "spiffe", Host: "example.org", Path: "/node/worker-a"},
			}),
			groups:     []string{kubeletNodeGroup},
			usages:     defaultServingUsages(),
			wantReason: "subject alternative names contain an unsupported identity",
		},
		"empty san": {
			request:    servingCSRRequest(t, kubeletNodeNamePrefix+testNodeName, []string{kubeletNodeGroup}, nil, nil, nil),
			groups:     []string{kubeletNodeGroup},
			usages:     defaultServingUsages(),
			wantReason: "certificate request has no subject alternative names",
		},
		"server auth extension": {
			request: servingCSRRequestWithSubject(t, servingSubject(), []string{testNodeName}, nil, []pkix.Extension{
				keyUsageExtension(t, 0, 2),
				extKeyUsageExtension(t, extensionOID(t, oidServerAuth)),
			}, nil),
			groups: []string{kubeletNodeGroup},
			usages: defaultServingUsages(),
		},
		"client auth extension rejected": {
			request: servingCSRRequestWithSubject(t, servingSubject(), []string{testNodeName}, nil, []pkix.Extension{
				extKeyUsageExtension(t, asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 2}),
			}, nil),
			groups:     []string{kubeletNodeGroup},
			usages:     defaultServingUsages(),
			wantReason: "extended key usage extension is not consistent with spec.usages",
		},
		"basic constraints": {
			request: servingCSRRequestWithSubject(t, servingSubject(), []string{testNodeName}, nil, []pkix.Extension{{
				Id:    extensionOID(t, oidBasicConstraints),
				Value: []byte{0x30, 0x03, 0x01, 0x01, 0xff},
			}}, nil),
			groups:     []string{kubeletNodeGroup},
			usages:     defaultServingUsages(),
			wantReason: "certificate request must not assert CA capability",
		},
		"wrong signer": {
			request:    validRequest,
			groups:     []string{kubeletNodeGroup},
			usages:     defaultServingUsages(),
			signerName: "example.com/other",
			wantReason: "spec.signerName is not kubernetes.io/kubelet-serving",
		},
		"wrong username": {
			request:    validRequest,
			groups:     []string{kubeletNodeGroup},
			usages:     defaultServingUsages(),
			username:   kubeletNodeUsername("other"),
			wantReason: "spec.username is not system:node:worker-a",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			signerName := certificatesv1.KubeletServingSignerName
			if test.signerName != "" {
				signerName = test.signerName
			}
			username := kubeletNodeUsername(testNodeName)
			if test.username != "" {
				username = test.username
			}
			csr := &certificatesv1.CertificateSigningRequest{
				Spec: certificatesv1.CertificateSigningRequestSpec{
					Request:    test.request,
					Groups:     test.groups,
					Usages:     test.usages,
					SignerName: signerName,
					Username:   username,
				},
			}
			require.Equal(t, test.wantReason, validateServingCSR(csr, testNodeName, trusted))
		})
	}
}

func TestUnit_ControllerApprovesValidServingCSR(t *testing.T) {
	t.Parallel()

	machine := newServingMachine(testNodeName)
	csr := newServingCSR(t, "valid", testNodeName, net.ParseIP(testNodeIP), metav1.Now())
	controller, tenantClient := newControllerForTest(t, machine, csr)

	result, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	requireServingCSRApproved(t, tenantClient, csr.Name, true)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionTrue, KubeletServingCSRApprovalReadyReason, "Ready to approve kubelet serving CSRs")
	require.Contains(t, servingEvents(controller), "Normal "+KubeletCSRApprovedReason+" Approved kubelet CSR valid")
}

func TestUnit_ControllerApprovesMachineDNSAddress(t *testing.T) {
	t.Parallel()

	machine := newServingMachine(testNodeName)
	machine.Status.Addresses = append(machine.Status.Addresses, clusterv1.MachineAddress{
		Type:    clusterv1.MachineInternalDNS,
		Address: "worker-a.internal",
	})
	csr := newServingCSR(t, "valid", "worker-a.internal", nil, metav1.Now())
	controller, tenantClient := newControllerForTest(t, machine, csr)

	_, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	requireServingCSRApproved(t, tenantClient, csr.Name, true)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionTrue, KubeletServingCSRApprovalReadyReason, "Ready to approve kubelet serving CSRs")
}

func TestUnit_ControllerWaitsForAddressesBeforeApprovingNodeName(t *testing.T) {
	t.Parallel()

	machine := newServingMachine(testNodeName)
	machine.Status.Addresses = nil
	csr := newServingCSR(t, "valid", testNodeName, nil, metav1.Now())
	controller, tenantClient := newControllerForTest(t, machine, csr)

	_, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	requireServingCSRApproved(t, tenantClient, csr.Name, false)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionFalse, KubeletServingCSRWaitingForAddressesReason, "Waiting for trusted Machine addresses")
}

func TestUnit_ControllerApprovesServingCSRAfterAddressArrives(t *testing.T) {
	t.Parallel()

	machine := newServingMachine(testNodeName)
	machine.Status.Addresses = nil
	csr := newServingCSR(t, "valid", testNodeName, net.ParseIP(testNodeIP), metav1.Now())
	controller, tenantClient := newControllerForTest(t, machine, csr)

	_, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	requireServingCSRApproved(t, tenantClient, csr.Name, false)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionFalse, KubeletServingCSRWaitingForAddressesReason, "Waiting for trusted Machine addresses")
	require.NotContains(t, servingEvents(controller), KubeletServingCSRRejectedReason)

	got := &clusterv1.Machine{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), got))
	got.Status.Addresses = clusterv1.MachineAddresses{{Type: clusterv1.MachineInternalIP, Address: testNodeIP}}
	require.NoError(t, controller.k8sClient.Status().Update(t.Context(), got))

	_, err = controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	requireServingCSRApproved(t, tenantClient, csr.Name, true)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionTrue, KubeletServingCSRApprovalReadyReason, "Ready to approve kubelet serving CSRs")
}

func TestUnit_ControllerWaitsWhenMachineHasNoAddresses(t *testing.T) {
	t.Parallel()

	machine := newServingMachine(testNodeName)
	machine.Status.Addresses = nil
	controller, _ := newControllerForTest(t, machine)

	_, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionFalse, KubeletServingCSRWaitingForAddressesReason, "Waiting for trusted Machine addresses")
}

func TestUnit_ControllerWaitsForNodeAssociation(t *testing.T) {
	t.Parallel()

	machine := newServingMachine(testNodeName)
	machine.Status.NodeRef = clusterv1.MachineNodeReference{}
	csr := newServingCSR(t, "valid", testNodeName, net.ParseIP(testNodeIP), metav1.Now())
	controller, tenantClient := newControllerForTest(t, machine, csr)

	_, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	requireServingCSRApproved(t, tenantClient, csr.Name, false)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionFalse, KubeletServingCSRWaitingForNodeReason, "Waiting for Machine node association")
}

func TestUnit_ControllerIgnoresExpectedNodeNameForServingCSR(t *testing.T) {
	t.Parallel()

	machine := newServingMachine(testNodeName)
	machine.Annotations[keys.MachineExpectedNodeName.String()] = "host-a"
	csr := newServingCSR(t, "valid", testNodeName, net.ParseIP(testNodeIP), metav1.Now())
	controller, tenantClient := newControllerForTest(t, machine, csr)

	_, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	requireServingCSRApproved(t, tenantClient, csr.Name, true)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionTrue, KubeletServingCSRApprovalReadyReason, "Ready to approve kubelet serving CSRs")
}

func TestUnit_ControllerRejectsDuplicateNodeRef(t *testing.T) {
	t.Parallel()

	first := newServingMachine(testNodeName)
	second := newServingMachine("other-machine")
	second.Status.NodeRef = clusterv1.MachineNodeReference{Name: testNodeName}
	csr := newServingCSR(t, "valid", testNodeName, net.ParseIP(testNodeIP), metav1.Now())
	controller, tenantClient := newControllerForTest(t, first, second, csr)

	_, err := controller.Reconcile(t.Context(), machineRequest(first))
	require.NoError(t, err)
	requireServingCSRApproved(t, tenantClient, csr.Name, false)
	assertServingCondition(t, controller.k8sClient, first, metav1.ConditionFalse, KubeletServingCSRNodeNameConflictReason, "Node name is not assigned to exactly one Machine")
}

func TestUnit_ControllerDoesNotRequireTenantNodeProviderID(t *testing.T) {
	t.Parallel()

	machine := newServingMachine(testNodeName)
	node := newTenantNode()
	node.Spec.ProviderID = ""
	csr := newServingCSR(t, "valid", testNodeName, net.ParseIP(testNodeIP), metav1.Now())
	controller, tenantClient := newControllerForTest(t, machine, node, newNicoMachine(), csr)

	_, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	requireServingCSRApproved(t, tenantClient, csr.Name, true)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionTrue, KubeletServingCSRApprovalReadyReason, "Ready to approve kubelet serving CSRs")
}

func TestUnit_ControllerIgnoresNodeAndInfrastructureAddresses(t *testing.T) {
	t.Parallel()

	machine := newServingMachine(testNodeName)
	node := newTenantNode()
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "198.51.100.20"}}
	nicoMachine := newNicoMachine()
	nicoMachine.Status.Addresses = clusterv1.MachineAddresses{{Type: clusterv1.MachineInternalIP, Address: "198.51.100.21"}}
	nodeIP := newServingCSR(t, "node-ip", "", net.ParseIP("198.51.100.20"), metav1.Now())
	machineIP := newServingCSR(t, "machine-ip", testNodeName, net.ParseIP(testNodeIP), metav1.Now())
	controller, tenantClient := newControllerForTest(t, machine, node, nicoMachine, nodeIP, machineIP)

	_, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	requireServingCSRApproved(t, tenantClient, nodeIP.Name, false)
	requireServingCSRApproved(t, tenantClient, machineIP.Name, true)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionTrue, KubeletServingCSRApprovalReadyReason, "Ready to approve kubelet serving CSRs")
}

func TestUnit_ControllerDoesNotFilterServingCSRByTenantNodeCreationTime(t *testing.T) {
	t.Parallel()

	node := newTenantNode()
	node.CreationTimestamp = metav1.NewTime(time.Now().Add(time.Hour))
	machine := newServingMachine(testNodeName)
	older := newServingCSR(t, "older", testNodeName, net.ParseIP(testNodeIP), metav1.NewTime(time.Now()))
	newer := newServingCSR(t, "newer", testNodeName, net.ParseIP(testNodeIP), metav1.NewTime(time.Now().Add(2*time.Hour)))
	controller, tenantClient := newControllerForTest(t, machine, node, newNicoMachine(), older, newer)

	_, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	requireServingCSRApproved(t, tenantClient, older.Name, true)
	requireServingCSRApproved(t, tenantClient, newer.Name, true)
}

func TestUnit_ControllerApprovesRenewalAfterIssuedCertificate(t *testing.T) {
	t.Parallel()

	machine := newServingMachine(testNodeName)
	issued := newServingCSR(t, "issued", testNodeName, net.ParseIP(testNodeIP), metav1.Now())
	issued.Status.Certificate = []byte("issued")
	renewal := newServingCSR(t, "renewal", testNodeName, net.ParseIP(testNodeIP), metav1.Now())
	controller, tenantClient := newControllerForTest(t, machine, issued, renewal)

	_, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	requireServingCSRApproved(t, tenantClient, issued.Name, false)
	requireServingCSRApproved(t, tenantClient, renewal.Name, true)
}

func TestUnit_ControllerLeavesInvalidCSRPending(t *testing.T) {
	t.Parallel()

	machine := newServingMachine(testNodeName)
	csr := newServingCSR(t, "invalid", "other", net.ParseIP(testNodeIP), metav1.Now())
	controller, tenantClient := newControllerForTest(t, machine, csr)

	_, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	requireServingCSRApproved(t, tenantClient, csr.Name, false)
	got := &certificatesv1.CertificateSigningRequest{}
	require.NoError(t, tenantClient.Get(t.Context(), client.ObjectKey{Name: csr.Name}, got))
	require.Empty(t, got.Status.Conditions)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionTrue, KubeletServingCSRApprovalReadyReason, "Ready to approve kubelet serving CSRs")
	require.Contains(t, servingEvents(controller), "Warning "+KubeletServingCSRRejectedReason+" invalid: DNS SAN is not trusted for the Machine")
}

func TestUnit_ControllerMarksConditionOptedOut(t *testing.T) {
	t.Parallel()

	machine := newServingMachine(testNodeName)
	controller, _ := newControllerForTest(t, machine)
	_, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionTrue, KubeletServingCSRApprovalReadyReason, "Ready to approve kubelet serving CSRs")

	got := &clusterv1.Machine{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), got))
	delete(got.Labels, keys.ApproveKubeletServingCSRs.String())
	require.NoError(t, controller.k8sClient.Update(t.Context(), got))

	_, err = controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionFalse, KubeletServingCSROptedOutReason, "Machine is not opted in to kubelet serving CSR approval")
	require.Contains(t, servingEvents(controller), "Normal "+KubeletServingCSROptedOutReason+" Machine is not opted in to kubelet serving CSR approval")
}

func TestUnit_ControllerLeavesConditionWhenDeleting(t *testing.T) {
	t.Parallel()

	machine := newServingMachine(testNodeName)
	machine.Finalizers = []string{"test.nvke.nvidia.com/keep"}
	controller, _ := newControllerForTest(t, machine)
	_, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)

	require.NoError(t, controller.k8sClient.Delete(t.Context(), machine))
	_, err = controller.Reconcile(t.Context(), machineRequest(machine))
	require.NoError(t, err)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionTrue, KubeletServingCSRApprovalReadyReason, "Ready to approve kubelet serving CSRs")
	require.NotContains(t, servingEvents(controller), KubeletServingCSROptedOutReason)
}

func TestUnit_ControllerRetriesApprovalFailure(t *testing.T) {
	t.Parallel()

	machine := newServingMachine(testNodeName)
	csr := newServingCSR(t, "valid", testNodeName, net.ParseIP(testNodeIP), metav1.Now())
	controller, tenantClient := newControllerForTest(t, machine, csr)
	controller.clusterCache = staticClusterCache{client: interceptor.NewClient(tenantClient.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return errors.New("approval rejected")
		},
	})}

	_, err := controller.Reconcile(t.Context(), machineRequest(machine))
	require.Error(t, err)
	assertServingCondition(t, controller.k8sClient, machine, metav1.ConditionFalse, KubeletServingCSRFailedReason, "approving kubelet client CSR valid: approval rejected")
}

type staticClusterCache struct {
	client client.Client
	err    error
}

var _ clustercache.ClusterCache = staticClusterCache{}

func (s staticClusterCache) GetClient(context.Context, client.ObjectKey) (client.Client, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.client, nil
}

func (s staticClusterCache) GetReader(ctx context.Context, cluster client.ObjectKey) (client.Reader, error) {
	return s.GetClient(ctx, cluster)
}

func (s staticClusterCache) GetUncachedClient(ctx context.Context, cluster client.ObjectKey) (client.Client, error) {
	return s.GetClient(ctx, cluster)
}

func (s staticClusterCache) GetRESTConfig(context.Context, client.ObjectKey) (*rest.Config, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &rest.Config{}, nil
}

func (s staticClusterCache) GetHealthCheckingState(context.Context, client.ObjectKey) clustercache.HealthCheckingState {
	return clustercache.HealthCheckingState{}
}

func (s staticClusterCache) Watch(context.Context, client.ObjectKey, clustercache.Watcher) error {
	return s.err
}

func (s staticClusterCache) GetClusterSource(string, func(context.Context, client.Object) []ctrl.Request, ...clustercache.GetClusterSourceOption) source.Source {
	return nil
}

func newControllerForTest(t *testing.T, objects ...client.Object) (*ServingCSRController, client.Client) {
	t.Helper()

	var managementObjects []client.Object
	var tenantObjects []client.Object
	managementObjects = append(managementObjects, newCAPICluster(), nicoMachineCRD())
	for _, object := range objects {
		switch object.(type) {
		case *clusterv1.Machine, *nicov1.NicoMachine:
			managementObjects = append(managementObjects, object)
		case *corev1.Node, *certificatesv1.CertificateSigningRequest:
			tenantObjects = append(tenantObjects, object)
		default:
			t.Fatalf("unexpected test object %T", object)
		}
	}

	managementScheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(managementScheme))
	require.NoError(t, nicov1.AddToScheme(managementScheme))
	require.NoError(t, apiextensionsv1.AddToScheme(managementScheme))
	managementClient := fake.NewClientBuilder().
		WithScheme(managementScheme).
		WithStatusSubresource(&clusterv1.Machine{}).
		WithIndex(&clusterv1.Machine{}, machineNodeRefNameField, extractMachineNodeRefName).
		WithObjects(managementObjects...).
		Build()

	tenantScheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(tenantScheme))
	require.NoError(t, certificatesv1.AddToScheme(tenantScheme))
	tenantClient := fake.NewClientBuilder().
		WithScheme(tenantScheme).
		WithIndex(&certificatesv1.CertificateSigningRequest{}, servingCSRUsernameField, extractServingCSRUsername).
		WithObjects(tenantObjects...).
		Build()

	controller := NewServingCSRController(
		managementClient,
		staticClusterCache{client: tenantClient},
		slog.New(slog.DiscardHandler),
	)
	controller.recorder = events.NewFakeRecorder(32)
	return controller, tenantClient
}

func newCAPICluster() *clusterv1.Cluster {
	return &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: testNamespace,
		},
	}
}

func newServingMachine(name string) *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:              testClusterName,
				keys.ApproveKubeletServingCSRs.String(): "true",
			},
			Annotations: map[string]string{},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: testClusterName,
			ProviderID:  testProviderID,
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: nicov1.GroupVersion.Group,
				Kind:     "NicoMachine",
				Name:     testNodeName,
			},
		},
		Status: clusterv1.MachineStatus{
			NodeRef: clusterv1.MachineNodeReference{Name: testNodeName},
			Addresses: clusterv1.MachineAddresses{{
				Type:    clusterv1.MachineInternalIP,
				Address: testNodeIP,
			}},
		},
	}
}

func newTenantNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              testNodeName,
			UID:               types.UID(testNodeName + "-uid"),
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
		Spec: corev1.NodeSpec{ProviderID: testProviderID},
	}
}

func newNicoMachine() *nicov1.NicoMachine {
	return &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       "NicoMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      testNodeName,
			Namespace: testNamespace,
		},
		Spec: nicov1.NicoMachineSpec{ProviderID: testProviderID},
		Status: nicov1.NicoMachineStatus{
			Addresses: clusterv1.MachineAddresses{{
				Type:    clusterv1.MachineInternalIP,
				Address: testNodeIP,
			}},
		},
	}
}

func nicoMachineCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nicomachines.infrastructure.cluster.x-k8s.io",
			Labels: map[string]string{
				"cluster.x-k8s.io/v1beta2": nicov1.GroupVersion.Version,
			},
		},
	}
}

func newServingCSR(t *testing.T, name, dnsName string, ip net.IP, created metav1.Time) *certificatesv1.CertificateSigningRequest {
	t.Helper()
	var dnsNames []string
	if dnsName != "" {
		dnsNames = []string{dnsName}
	}
	var ips []net.IP
	if ip != nil {
		ips = []net.IP{ip}
	}
	return &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: created,
		},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:    servingCSRRequest(t, kubeletNodeNamePrefix+testNodeName, []string{kubeletNodeGroup}, dnsNames, ips, nil),
			SignerName: certificatesv1.KubeletServingSignerName,
			Usages:     defaultServingUsages(),
			Username:   kubeletNodeNamePrefix + testNodeName,
			Groups:     []string{kubeletNodeGroup},
		},
	}
}

func defaultServingUsages() []certificatesv1.KeyUsage {
	return []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageKeyEncipherment,
		certificatesv1.UsageServerAuth,
	}
}

func servingSubject() pkix.Name {
	return pkix.Name{CommonName: kubeletNodeNamePrefix + testNodeName, Organization: []string{kubeletNodeGroup}}
}

func servingCSRRequest(
	t *testing.T,
	commonName string,
	organizations []string,
	dnsNames []string,
	ips []net.IP,
	uris []*url.URL,
) []byte {
	t.Helper()
	return servingCSRRequestWithSubject(t, pkix.Name{CommonName: commonName, Organization: organizations}, dnsNames, ips, nil, uris)
}

func servingCSRRequestWithSubject(t *testing.T, subject pkix.Name, dnsNames []string, ips []net.IP, extensions []pkix.Extension, uris []*url.URL) []byte {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	request, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:         subject,
		DNSNames:        dnsNames,
		IPAddresses:     ips,
		URIs:            uris,
		ExtraExtensions: extensions,
	}, privateKey)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: certificateRequestPEMType, Bytes: request})
}

func requireServingCSRApproved(t *testing.T, tenantClient client.Client, name string, approved bool) {
	t.Helper()
	got := &certificatesv1.CertificateSigningRequest{}
	require.NoError(t, tenantClient.Get(t.Context(), client.ObjectKey{Name: name}, got))
	condition := servingCSRApprovedCondition(got)
	require.Equal(t, approved, condition != nil)
	if approved {
		require.Equal(t, KubeletClientCSRApprovalReason, condition.Reason)
		require.Equal(t, "kubelet CSR matches the node identity recorded by NVKE", condition.Message)
	}
}

func servingCSRApprovedCondition(csr *certificatesv1.CertificateSigningRequest) *certificatesv1.CertificateSigningRequestCondition {
	for i := range csr.Status.Conditions {
		condition := &csr.Status.Conditions[i]
		if condition.Type == certificatesv1.CertificateApproved && condition.Status == corev1.ConditionTrue {
			return condition
		}
	}
	return nil
}

func assertServingCondition(t *testing.T, k8sClient client.Client, machine *clusterv1.Machine, status metav1.ConditionStatus, reason, message string) {
	t.Helper()
	got := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), got))
	condition := meta.FindStatusCondition(got.Status.Conditions, KubeletServingCSRApprovalReadyCondition)
	require.NotNil(t, condition)
	require.Equal(t, status, condition.Status)
	require.Equal(t, reason, condition.Reason)
	require.Equal(t, message, condition.Message)
}

func servingEvents(controller *ServingCSRController) []string {
	recorder, ok := controller.recorder.(*events.FakeRecorder)
	if !ok || recorder == nil {
		return nil
	}
	var events []string
	for {
		select {
		case event := <-recorder.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}
