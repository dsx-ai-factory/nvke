// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/config"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/kube"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/nke"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/tenantauth"
)

// tenantTest carries everything a backend's specs share: the wiring built once
// in the create spec, plus the cluster created there.
type tenantTest struct {
	log           logr.Logger
	cfg           config.Config
	k8sClient     client.Client
	tenantClients *kube.TenantClientFactory
	nke           *nke.Client
	timeouts      nke.Options
	trustDomain   string
	spec          nke.ClusterSpec
	cluster       *api.Cluster
	// cascadePoolID is set by the node-pool cascade post-create spec and read by
	// its post-delete counterpart to prove cluster deletion removed the pool.
	cascadePoolID uuid.UUID
}

var _ = DescribeTableSubtree("NKE with backend", withNicoVMSOSProfileEntries(
	func(spec testSpec) {
		var (
			test    tenantTest
			failed  bool
			deleted bool
		)

		// Record any spec failure so the cluster delete can honor preserveOnFailure.
		AfterEach(func() { failed = failed || CurrentSpecReport().Failed() })

		It("creates the cluster", func(ctx context.Context) {
			// The cp-controller lane needs a flag-on CWE (a flag-off CWE rejects the
			// cluster-level controlPlaneConfig at create). Skip the whole entry rather
			// than fail when the flag is not set, so a default local `capd` run against
			// a flag-off CWE simply skips it. CI selects this entry only in its flag-on
			// lane.
			if (spec.validate.controlPlaneController || spec.validate.controlPlaneControllerUpdate ||
				spec.validate.controlPlaneControllerRollingUpdate) &&
				os.Getenv("PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER") != "true" {
				Skip("requires CWE deployed with PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=true")
			}
			test = expectSucceed(setupTenantTest(ctx, spec.options))
			if spec.validate.brownfieldOIDC {
				// A cluster on an OIDC-enabled site is always born OIDC-on, but
				// brownfield enablement needs an OIDC-off cluster to act on. Turn the
				// CWE create default off (the OIDC config env stays, so the enable
				// workflow still runs once it is flipped back on) so this cluster is
				// created OIDC-off; validateBrownfieldOIDC flips it on and enables.
				// Restored in AfterAll.
				By("Creating the brownfield cluster with the CWE OIDC create-default turned off")
				Expect(nke.SetOIDCEnabled(ctx, test.k8sClient, false)).To(Succeed())
			}
			test.initCluster(ctx, spec)
		}, Label("lifecycle"), NodeTimeout(2*time.Hour))

		Describe("after cluster creation", func() {
			BeforeEach(func() {
				if test.cluster == nil {
					Skip("Cluster creation failed")
				}
			})

			if !spec.validate.provisioningOnly {
				validateOIDCPostCreate(&test)
			}
			validateNodeReadinessPostCreate(&test)
			if spec.validate.openVSwitch {
				validateOpenVSwitchPostCreate(&test)
			}
			if spec.validate.postReadyChecks {
				validatePostReadyClusterState(&test)
				validatePostReadyNodeOperations(&test)
			}
			if spec.validate.kubeVipRouteWithdrawal {
				validatePostReadyKubeVipRouteWithdrawal(&test)
			}
			if spec.validate.certificateRenewal {
				validatePostReadyCertificateRenewal(&test)
			}
			if spec.validate.csrApproval {
				validateKubeletServingCSRApproval(&test)
			}
			if spec.validate.etcdEncryption {
				validateEtcdEncryption(&test)
			}
			if spec.validate.brownfieldEtcdEncryption {
				validateBrownfieldEtcdEncryption(&test)
			}
			if spec.validate.brownfieldOIDC {
				validateBrownfieldOIDC(&test)
			}
			if spec.validate.scale {
				validateNodePoolScale(&test)
			}
			if spec.validate.reboot {
				validateLabelReboot(&test)
			}
			if spec.validate.upgrade {
				validateK8sUpgrade(&test)
			}
			if !spec.validate.provisioningOnly {
				validatePodCIDRPostCreate(&test)
			}
			if spec.validate.clusterConfig {
				validateClusterConfig(&test)
			}
			if spec.validate.nodeReplacement {
				validateNodeReplacement(&test)
			}
			if spec.validate.manualScaleDown {
				validateManualScaleDownStability(&test)
			}
			if spec.validate.cpRotation {
				validateSingleNodeControlPlaneRotation(&test)
			}
			if spec.validate.controlPlaneController {
				validateControllerManagedControlPlane(&test)
			}
			if spec.validate.controlPlaneControllerUpdate {
				validateControllerManagedControlPlaneUpdate(&test)
			}
			if spec.validate.controlPlaneControllerRollingUpdate {
				validateControllerManagedControlPlaneRollingUpdate(&test)
			}
			if spec.validate.nodePoolLifecycle {
				validateAdditionalNodePool(&test)
			}
			if spec.validate.nodePoolCascade {
				validateNodePoolCascadePostCreate(&test)
			}
		})

		It("deletes the cluster", func(ctx context.Context) {
			if test.cluster == nil {
				Skip("Cluster creation failed")
			}
			if test.cfg.Lifecycle.ReuseCluster != "" || test.cfg.Lifecycle.SkipDelete {
				Skip("Lifecycle config keeps the cluster")
			}
			if test.cfg.Lifecycle.PreserveOnFailure && failed {
				Skip("Preserving the cluster after a failure")
			}

			test.deleteClusterAndWait(ctx)
			deleted = true
		}, Label("lifecycle"), NodeTimeout(1*time.Hour))

		Describe("after cluster deletion", func() {
			BeforeEach(func() {
				if !deleted {
					Skip("Cluster was not deleted; skipping post-deletion validations")
				}
			})

			validateTenantAccessSecretGCPostDelete(&test)
			if !spec.validate.provisioningOnly {
				validateOIDCPostDelete(&test)
			}
			if spec.validate.nodePoolCascade {
				validateNodePoolCascadePostDelete(&test)
			}
			if spec.validate.etcdEncryption {
				validateEtcdEncryptionPostDelete(&test)
			}
		})

		AfterAll(func(ctx context.Context) {
			if spec.validate.brownfieldOIDC && test.k8sClient != nil {
				// Restore the CWE OIDC create-default the create spec turned off, so
				// it does not leak to other table entries (the suite runs serially).
				Expect(nke.SetOIDCEnabled(ctx, test.k8sClient, true)).To(Succeed())
			}
			// Use AfterAll so the cluster is removed even if a spec before the
			// delete spec fails (or the delete spec is skipped). It is a no-op
			// once the delete spec has run.
			switch {
			case deleted, test.cluster == nil:
			case test.cfg.Lifecycle.ReuseCluster != "", test.cfg.Lifecycle.SkipDelete:
			case test.cfg.Lifecycle.PreserveOnFailure && failed:
			default:
				test.deleteClusterAndWait(ctx)
			}
		}, NodeTimeout(1*time.Hour))
	},
	// The pull-request smoke lane uses a separate, minimal topology instead of
	// filtering the full CAPD row below. The full row needs three control-plane
	// nodes for its scale, rotation, and certificate checks; the core lane only
	// proves that the ordinary node-pool path can create a usable cluster and
	// delete it again.
	Entry("CAPD core", Ordered, ContinueOnFailure, Label("core", "core-docker"), testSpec{
		buildSpec: capdSpec,
		shape: clusterShape{
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 1}},
			workerPools:       []nke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options:  nke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		validate: validations{provisioningOnly: true},
	}),
	Entry("CAPD", Ordered, ContinueOnFailure, Label("capd", "happy-path", "kube-vip-route-withdrawal", "cert-renewal"), testSpec{
		buildSpec: capdSpec,
		shape: clusterShape{
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3}},
			workerPools:       []nke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options: nke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		// The last four were nico-vms-only until nico-containers took them, which
		// left CAPD the one backend in CI not asserting them. They call CWE's
		// node-pool API and read cluster fields; nothing here is backend-specific.
		validate: validations{
			scale: true, reboot: true, postReadyChecks: true,
			certificateRenewal: true, csrApproval: true, nodePoolLifecycle: true,
			nodePoolCascade: true, nodeReplacement: true,
			manualScaleDown: true, clusterConfig: true,
			kubeVipRouteWithdrawal: true,
		},
	}),
	// This row is selected only by the dedicated identity-exchange lane. Keeping
	// it off the broad "capd" label prevents the direct-SPIRE control lane from
	// exercising a topology it did not deploy.
	Entry("CAPD with DSX identity exchange", Ordered, ContinueOnFailure, Label("identity-exchange"), testSpec{
		buildSpec: capdSpec,
		shape: clusterShape{
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options: nke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
	}),
	// Opt-in via the "cp-controller" label: it exercises cluster-level
	// control-plane-controller provisioning (no control-plane node pool), so it
	// only passes against CWE deployed with
	// PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=true. The CAPD E2E
	// workflow runs it in a dedicated flag-on lane; the readiness spec also
	// self-skips unless that flag is set (belt-and-suspenders).
	Entry("CAPD control-plane controller (no CP node pool)", Ordered, ContinueOnFailure, Label("capd", "cp-controller"), testSpec{
		buildSpec: capdControlPlaneControllerSpec,
		shape: clusterShape{
			controlPlaneConfig: &nke.ControlPlaneConfigSpec{DesiredCount: 1},
			workerPools:        []nke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options:  nke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		validate: validations{controlPlaneController: true, controlPlaneControllerUpdate: true, controlPlaneControllerRollingUpdate: true},
	}),
	// The same for nico-containers, so the cp-controller path is not CAPD-only.
	// Three control-plane nodes because the rolling-update spec needs a
	// multi-node control plane to keep etcd quorum while it rolls.
	Entry("nico-containers control-plane controller (no CP node pool)", Ordered, ContinueOnFailure, Label("nico-containers", "cp-controller"), testSpec{
		buildSpec: nicoControlPlaneControllerSpec,
		shape: clusterShape{
			controlPlaneConfig: &nke.ControlPlaneConfigSpec{DesiredCount: 3},
			workerPools:        []nke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options:  nke.Options{Provision: 15 * time.Minute, Delete: 10 * time.Minute, Poll: 5 * time.Second},
		validate: validations{controlPlaneController: true, controlPlaneControllerUpdate: true, controlPlaneControllerRollingUpdate: true},
	}),
	Entry("nico-vms control-plane controller (no CP node pool)", Ordered, ContinueOnFailure, Label("nico-vms", "cp-controller"), testSpec{
		buildSpec: nicoVMSControlPlaneControllerSpec,
		shape: clusterShape{
			kubernetesVersion: "1.34.2",
			controlPlaneConfig: &nke.ControlPlaneConfigSpec{
				DesiredCount:         1,
				InstanceTypeID:       nicoVMSLargeInstanceID,
				OSProfile:            nicoVMSOSProfile,
				ConfigProfile:        nicoVMSCPConfigProfile,
				VpcPrefixIDs:         []string{nicoVMSVpcPrefixID},
				SSHKeyGroupIDs:       []string{nicoVMSSSHKeyGroupID},
				SystemReservedMemory: "1Gi",
			},
			workerPools: []nke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
		},
		options:  nke.Options{Provision: 90 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
		validate: validations{controlPlaneController: true, controlPlaneControllerUpdate: true, controlPlaneControllerRollingUpdate: true},
	}),
	// NICo gets the same bounded pull-request contract as CAPD. Keep this entry
	// off the broad nico-containers label so scheduled full runs do not repeat
	// the same lifecycle with a smaller topology.
	Entry("nico-containers core", Ordered, ContinueOnFailure, Label("core", "core-nico"), testSpec{
		buildSpec: nicoSpec,
		shape: clusterShape{
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 1}},
			workerPools:       []nke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options:  nke.Options{Provision: 15 * time.Minute, Delete: 10 * time.Minute, Poll: 5 * time.Second},
		validate: validations{provisioningOnly: true},
	}),
	Entry("nico-containers", Ordered, ContinueOnFailure, Label("nico-containers", "happy-path"), testSpec{
		buildSpec: nicoSpec,
		shape: clusterShape{
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3}},
			workerPools:       []nke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options: nke.Options{Provision: 15 * time.Minute, Delete: 10 * time.Minute, Poll: 5 * time.Second},
		// Everything CAPD asserts on this shape, plus the node-pool operations
		// nico-vms carries. None of these reaches the infrastructure API: they
		// call CWE's REST surface or the tenant cluster, and both backends use
		// the same node image.
		validate: validations{
			scale: true, reboot: true,
			postReadyChecks: true, csrApproval: true,
			nodePoolLifecycle: true, nodePoolCascade: true,
			nodeReplacement: true, manualScaleDown: true, clusterConfig: true,
		},
	}),
	// Encryption and OIDC need their own clusters: a cluster born encrypted
	// cannot also exercise enabling encryption on an unencrypted one, and
	// brownfield OIDC needs a cluster created with OIDC off. Same split CAPD
	// uses, and until now CAPD was the only backend asserting any of it.
	Entry("nico-containers with etcd encryption", Ordered, ContinueOnFailure, Label("nico-containers", "extended", "encryption"), testSpec{
		buildSpec: nicoSpec,
		shape: clusterShape{
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3}},
			etcdEncrypted:     true,
		},
		options:  nke.Options{Provision: 15 * time.Minute, Delete: 10 * time.Minute, Poll: 5 * time.Second},
		validate: validations{etcdEncryption: true},
	}),
	Entry("nico-containers brownfield etcd encryption", Ordered, ContinueOnFailure, Label("nico-containers", "extended", "encryption", "etcd-encryption-enable"), testSpec{
		buildSpec: nicoSpec,
		shape: clusterShape{
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3}},
		},
		options:  nke.Options{Provision: 15 * time.Minute, Delete: 10 * time.Minute, Poll: 5 * time.Second},
		validate: validations{brownfieldEtcdEncryption: true},
	}),
	Entry("nico-containers brownfield OIDC enablement", Ordered, ContinueOnFailure, Label("nico-containers", "extended", "oidc", "oidc-enable"), testSpec{
		buildSpec: nicoSpec,
		shape: clusterShape{
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3}},
		},
		options:  nke.Options{Provision: 15 * time.Minute, Delete: 10 * time.Minute, Poll: 5 * time.Second},
		validate: validations{brownfieldOIDC: true},
	}),
	Entry("nico-vms", Ordered, ContinueOnFailure, Label("nico-vms", "happy-path"), testSpec{
		buildSpec: nicoVMSSpec,
		shape: clusterShape{
			kubernetesVersion: "1.34.2",
			cniPlugin:         CNIPluginOVN, // Required by the Open vSwitch typed-path assertion below.
			// Default cpu2-mem2gb cannot schedule ovnkube-node (500m/1500Mi).
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3, InstanceTypeID: nicoVMSLargeInstanceID}},
			workerPools:       []nke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
		},
		options:  nke.Options{Provision: 90 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
		validate: validations{scale: true, reboot: true, openVSwitch: true},
	}),
	Entry("nico-vms k8s upgrade", Ordered, ContinueOnFailure, Label("nico-vms", "happy-path", "upgrade"), testSpec{
		buildSpec: nicoVMSSpec,
		shape: clusterShape{
			kubernetesVersion: upgradeStartingKubernetesVersion,
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3, InstanceTypeID: nicoVMSLargeInstanceID}},
			workerPools:       []nke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
		},
		options:  nke.Options{Provision: 90 * time.Minute, Upgrade: 90 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
		validate: validations{upgrade: true},
	}),
	Entry("CAPD with etcd encryption", Ordered, ContinueOnFailure, Label("capd", "extended", "encryption"), testSpec{
		buildSpec: capdSpec,
		shape: clusterShape{
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3}},
			etcdEncrypted:     true,
		},
		options:  nke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		validate: validations{etcdEncryption: true},
	}),
	Entry("CAPD brownfield etcd encryption", Ordered, ContinueOnFailure, Label("capd", "extended", "encryption", "etcd-encryption-enable"), testSpec{
		buildSpec: capdSpec,
		shape: clusterShape{
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3}},
		},
		options:  nke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		validate: validations{brownfieldEtcdEncryption: true},
	}),
	Entry("CAPD brownfield OIDC enablement", Ordered, ContinueOnFailure, Label("capd", "extended", "oidc", "oidc-enable"), testSpec{
		buildSpec: capdSpec,
		shape: clusterShape{
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3}},
		},
		options:  nke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		validate: validations{brownfieldOIDC: true},
	}),
	Entry("nico-vms OS profiles", Ordered, ContinueOnFailure, Label("nico-vms", "extended", "os-profiles"), testSpec{
		buildSpec: nicoVMSSpec,
		shape: clusterShape{
			cniPlugin:         CNIPluginCalico, // Simply to make this test run faster (520s vs 1011s).
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 1}},
			workerPools: []nke.NodePoolSpec{
				// Old agent image pins the agent version to 0.1.22, which is before the new nkeproto.CheckCloudInitStatus was introduced in version 0.1.24.
				// So this osProfile test exists to prove that CWE can still provision nodePools that may still be pointed at older agent versions.
				{Name: "worker-old-agent", NodeNamePrefix: "wold", DesiredNodeCount: 1, OSProfile: "nke-nico-vms-24.04-amd64-fde-old-agent"},
				// nicoVMSNodePoolSpec defaults kube/system reserved memory to 250Mi so the
				// default nico-vms instance type has enough allocatable memory.
				{Name: "worker-260625-24.04", NodeNamePrefix: "w260625", DesiredNodeCount: 1, OSProfile: "nke-260625-24.04-x86-fde"},
				{Name: "worker-260602-24.04", NodeNamePrefix: "w260602", DesiredNodeCount: 1, OSProfile: "nke-260602-24.04-x86-fde"},
			},
		},
		options: nke.Options{Provision: 30 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
	}),
	Entry("nico-vms multi pod CIDRs", Ordered, ContinueOnFailure, Label("nico-vms", "multi-pod-cidrs"), testSpec{
		buildSpec: nicoVMSMultiPodCIDRSpec,
		shape: clusterShape{
			kubernetesVersion: "1.34.2",
			cniPlugin:         CNIPluginOVN,
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3, InstanceTypeID: nicoVMSLargeInstanceID}},
			workerPools:       []nke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
		},
		options: nke.Options{Provision: 90 * time.Minute, Delete: 10 * time.Minute, Poll: 30 * time.Second},
	}),
	Entry("nico-vms node pool lifecycle", Ordered, ContinueOnFailure, Label("nico-vms", "extended", "nodepool"), testSpec{
		buildSpec: nicoVMSSpec,
		shape: clusterShape{
			cniPlugin:         CNIPluginOVN, // OVN provisions faster than the Calico default.
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
			workerPools:       []nke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
		},
		options:  nke.Options{Provision: 90 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
		validate: validations{clusterConfig: true, nodeReplacement: true, manualScaleDown: true, nodePoolLifecycle: true, nodePoolCascade: true},
	}),
	// Opt-in via the "cp-rotation" label: it exercises a 2-to-1 etcd member
	// transition while rotating a single-node control plane.
	Entry("nico-vms single-node control-plane rotation", Ordered, ContinueOnFailure, Label("nico-vms", "extended", "cp-rotation"), testSpec{
		buildSpec: nicoVMSSpec,
		shape: clusterShape{
			cniPlugin:         CNIPluginOVN, // OVN provisions faster than the Calico default.
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
			workerPools:       []nke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
		},
		options:  nke.Options{Provision: 90 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
		validate: validations{cpRotation: true},
	}),
)...)

func withNicoVMSOSProfileEntries(args ...any) []any {
	for _, entry := range mustNicoVMSOSProfileTableEntries() {
		args = append(args, entry)
	}
	return args
}

// specBuilder builds the create spec for the backend under test from cfg and a
// base cluster name.
type specBuilder func(cfg config.Config, name string, shape clusterShape) nke.ClusterSpec

// testSpec is one DescribeTableSubtree entry: how to build the cluster spec
// plus the validations and cluster-operation options for that variant.
type testSpec struct {
	buildSpec specBuilder
	shape     clusterShape
	options   nke.Options
	validate  validations
}

type clusterShape struct {
	kubernetesVersion string
	cniPlugin         string
	controlPlanePools []nke.NodePoolSpec
	workerPools       []nke.NodePoolSpec
	etcdEncrypted     bool
	controlPlaneVIP   string
	// controlPlaneConfig, when set, provisions the control plane through the
	// cluster-level control-plane controller (no control-plane node pool) instead
	// of controlPlanePools. Consumed by capdControlPlaneControllerSpec and
	// nicoVMSControlPlaneControllerSpec.
	controlPlaneConfig *nke.ControlPlaneConfigSpec
}

type validations struct {
	// provisioningOnly limits the lifecycle to create, node readiness, delete,
	// and the shared tenant-access cleanup check.
	provisioningOnly bool
	// postReadyChecks verifies intended cluster state after the initial CAPD
	// cluster and all desired nodes are Ready.
	postReadyChecks bool
	// kubeVipRouteWithdrawal stops one CAPD kube-apiserver and verifies kube-vip
	// withdraws only that node's BGP path before restoring it.
	kubeVipRouteWithdrawal bool
	// certificateRenewal forces certificate renewal on every control-plane node
	// and verifies the cluster returns to Ready after each sequential restart.
	certificateRenewal bool
	// openVSwitch proves OVN node provisioning used the typed node-agent RPC
	// on every node and did not enter the legacy runbook fallback.
	openVSwitch bool
	// csrApproval rotates a Ready worker's kubelet serving certificate and
	// verifies that the tenant CSR controller approves and obtains a new one.
	csrApproval bool
	// scale exercises control-plane and worker node pool scale-up and scale-down.
	scale bool
	// reboot applies nke.nvidia.com/reboot to a worker and waits for remediation.
	reboot bool
	// etcdEncryption verifies encryption configured during cluster creation.
	etcdEncryption bool
	// brownfieldEtcdEncryption enables encryption on an existing unencrypted
	// cluster and verifies it cannot subsequently be disabled.
	brownfieldEtcdEncryption bool
	// brownfieldOIDC enables OIDC on an existing OIDC-off cluster through the
	// UpdateCluster{enableOidc:true} API and verifies the kubeadm-config write-back
	// plus a SPIRE identity authenticating on the tenant apiserver.
	brownfieldOIDC bool
	// clusterConfig asserts the cluster and initial pools report the config they
	// were created with (spec round-trip).
	clusterConfig bool
	// nodeReplacement deletes a worker node (desired count unchanged) and asserts
	// CWE provisions a replacement while the surviving node is preserved.
	nodeReplacement bool
	// manualScaleDown asserts lowering the desired count does not delete nodes,
	// and a node manually deleted at the lowered count is not re-provisioned.
	manualScaleDown bool
	// nodePoolLifecycle creates, scales, and deletes an additional worker pool.
	nodePoolLifecycle bool
	// nodePoolCascade creates a worker pool left active until cluster deletion,
	// then asserts (post-delete) that deleting the cluster removed it.
	nodePoolCascade bool
	// upgrade performs an in-place Kubernetes version upgrade validation.
	upgrade bool
	// cpRotation rotates a single-node control plane. Requires a 1-CP shape.
	cpRotation bool
	// controlPlaneController asserts the control plane is provisioned by the
	// cluster-level control-plane controller with no control-plane node pool.
	// Requires CWE deployed with
	// PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=true (self-skips
	// otherwise).
	controlPlaneController bool
	// controlPlaneControllerUpdate PATCHes the cluster-level controlPlaneConfig
	// desiredCount and asserts the controller-managed control plane converges on
	// the new count in CWE and the tenant API. Requires the same flag-on CWE as
	// controlPlaneController (self-skips otherwise).
	controlPlaneControllerUpdate bool
	// controlPlaneControllerRollingUpdate PATCHes the cluster-level
	// controlPlaneConfig with a new config profile to bump the control-plane
	// configuration revision and asserts the controller-managed control-plane
	// nodes are rolled onto new nodes at the same desired count in CWE and the
	// tenant API. Ordered after controlPlaneControllerUpdate so the control plane
	// has enough nodes to keep quorum while rolling. Requires the same flag-on CWE
	// as controlPlaneController (self-skips otherwise).
	controlPlaneControllerRollingUpdate bool
}

// setupTenantTest builds the per-backend wiring (clients, trust domain, logger)
// and preflights NKE reachability over the port-forward. It does not create a
// cluster.
func setupTenantTest(ctx context.Context, timeouts nke.Options) (tenantTest, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return tenantTest{}, fmt.Errorf("loading e2e config: %w", err)
	}

	restConfig, err := kube.RESTConfig(cfg.Kube.Kubeconfig, cfg.Kube.Context)
	if err != nil {
		return tenantTest{}, fmt.Errorf("reading kubeconfig: %w", err)
	}

	k8sClient, err := kube.RuntimeClient(restConfig)
	if err != nil {
		return tenantTest{}, fmt.Errorf("building management cluster client: %w", err)
	}

	cweClient, err := nke.NewClient(ctx, restConfig, timeouts)
	if err != nil {
		return tenantTest{}, fmt.Errorf("building CWE API client: %w", err)
	}

	trustDomain, err := kube.TrustDomain(cfg.Kube.Kubeconfig, cfg.Kube.Context)
	if err != nil {
		GinkgoLogr.Info("SPIRE trust domain could not be derived from kubeconfig context; OIDC validations will skip", "error", err)
	}

	var tenantClientOptions []kube.TenantClientFactoryOption
	if os.Getenv(dsxIdentityExchangeEnv) == "true" {
		var exchangeCA corev1.ConfigMap
		if getErr := k8sClient.Get(ctx, client.ObjectKey{
			Namespace: "cloud-workflow-engine",
			Name:      dsxExchangeCAConfigMap,
		}, &exchangeCA); getErr != nil {
			return tenantTest{}, fmt.Errorf("getting CAPD DSX identity exchange CA: %w", getErr)
		}
		tenantClientOptions = append(tenantClientOptions, kube.WithJWTExchangeCABundle([]byte(exchangeCA.Data["ca.crt"])))
	}

	tenantClients, err := kube.BuildTenantClientFactory(restConfig, tenantClientOptions...)
	if err != nil {
		return tenantTest{}, fmt.Errorf("building tenant Kubernetes client factory: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := cweClient.API().GetClusterWithResponse(pingCtx, uuid.New())
	if err != nil {
		return tenantTest{}, fmt.Errorf("reaching NKE over the port-forward: %w", err)
	}
	if resp.StatusCode() != 404 {
		return tenantTest{}, fmt.Errorf("CWE preflight: want 404 for a random cluster UUID, got %s: %s", resp.Status(), resp.Body)
	}

	return tenantTest{
		log:           GinkgoLogr,
		cfg:           cfg,
		k8sClient:     k8sClient,
		tenantClients: tenantClients,
		nke:           cweClient,
		timeouts:      timeouts,
		trustDomain:   trustDomain,
	}, nil
}

// initCluster reuses an existing cluster when lifecycle.reuseCluster is
// set, otherwise creates one for the backend under test and waits for it to be
// provisioned. It fails the spec on any error.
func (t *tenantTest) initCluster(ctx context.Context, b testSpec) {
	GinkgoHelper()
	t.spec = b.buildSpec(t.cfg, clusterNamePrefix, b.shape)
	t.spec.Name = fmt.Sprintf("%s-%s", t.spec.Name, t.spec.Backend) // CreateCluster appends a timestamp

	if t.cfg.Lifecycle.ReuseCluster != "" {
		t.cluster = expectSucceed(t.nke.GetByName(ctx, t.cfg.Lifecycle.ReuseCluster))
		return
	}

	if t.spec.ControlPlane.ControlPlaneVIP == "" {
		vip, err := t.availableControlPlaneVIP(ctx, preferredControlPlaneVIP(t.spec.Backend))
		Expect(err).NotTo(HaveOccurred(), "should select an unused control-plane VIP")
		t.spec.ControlPlane.ControlPlaneVIP = vip
	}
	t.log.Info("Using control-plane VIP (empty means none configured)", "controlPlaneVIP", t.spec.ControlPlane.ControlPlaneVIP)

	By(fmt.Sprintf("Creating the %s tenant cluster", t.spec.Backend))
	cluster, err := t.nke.CreateCluster(ctx, t.spec)
	Expect(err).NotTo(HaveOccurred(), "should create the cluster")
	t.log.Info("Created cluster", "name", cluster.Name, "id", cluster.ID)

	By("Verifying the tenant cluster appears in the cluster list")
	listedCluster, err := t.nke.GetByName(ctx, cluster.Name)
	Expect(err).NotTo(HaveOccurred(), "should list the created cluster by name")
	Expect(listedCluster.ID).To(Equal(cluster.ID), "listed cluster should match the created cluster")

	By("Waiting for the tenant cluster to be provisioned")
	start := time.Now()
	cluster, err = t.nke.WaitForClusterReady(ctx, cluster.ID)
	Expect(err).NotTo(HaveOccurred(), "should provision the cluster")
	t.log.Info("Cluster provisioned", "name", cluster.Name, "duration", time.Since(start).Round(time.Second).String())

	t.cluster = cluster
}

// validateTenantAccessSecretGCPostDelete proves the tenant-access controller's
// owner-reference design: the admin kubeconfig secret is published for every
// cluster regardless of OIDC mode, and this controller never deletes it
// itself, so its absence here can only come from Kubernetes garbage
// collecting it once the CAPI Cluster it is owned by finished deleting.
func validateTenantAccessSecretGCPostDelete(test *tenantTest) {
	It("garbage-collects the tenant-access admin kubeconfig secret", func(ctx context.Context) {
		Eventually(func() error {
			return tenantauth.AssertAdminKubeconfigDeleted(ctx, test.k8sClient, test.cluster.Name)
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	}, Label("lifecycle", "tenant-access", "cleanup"), NodeTimeout(6*time.Minute))
}

// deleteClusterAndWait deletes the tenant cluster and blocks until CWE reports
// it gone (404). Shared by the delete spec and the AfterAll cleanup.
func (t *tenantTest) deleteClusterAndWait(ctx context.Context) {
	GinkgoHelper()
	By("Deleting the tenant cluster")
	Expect(t.nke.DeleteCluster(ctx, t.cluster.ID)).To(Succeed())
	By("Waiting for the tenant cluster to be deleted")
	Expect(t.nke.WaitForClusterDeleted(ctx, t.cluster.ID)).To(Succeed())
}

// Cluster shape is a property of the test, not the environment, so it lives in
// code rather than the e2e config file. Per-backend waiter timeouts live on the
// testSpec entries above.
const (
	clusterNamePrefix        = "e2e" // CreateCluster appends the backend and a timestamp
	defaultKubernetesVersion = "1.34.2"
	CNIPluginCalico          = "calico"
	CNIPluginOVN             = "ovn-kubernetes"
	capdControlPlaneVIP      = "172.31.1.1"

	nicoVMSMultiPodCIDR        = "10.250.0.0/23,10.252.0.0/22"
	nicoVMSMultiPodServiceCIDR = "10.130.0.0/16"
)

func preferredControlPlaneVIP(backend nke.Backend) string {
	switch backend {
	case nke.BackendCAPD:
		return capdControlPlaneVIP
	case nke.BackendNicoContainers:
		return nicoControlPlaneVIP
	case nke.BackendNicoVMS:
		return nicoVMSControlPlaneVIP
	default:
		panic(fmt.Sprintf("no preferred control-plane VIP for backend %q", backend))
	}
}

func (t *tenantTest) availableControlPlaneVIP(ctx context.Context, preferred string) (string, error) {
	clusters, err := t.nke.ListClusters(ctx)
	if err != nil {
		return "", err
	}

	used := make(map[string]struct{}, len(clusters))
	for _, cluster := range clusters {
		if cluster.ControlPlaneVIP != nil {
			used[*cluster.ControlPlaneVIP] = struct{}{}
		}
	}

	preferredAddr, err := netip.ParseAddr(preferred)
	if err != nil {
		return "", fmt.Errorf("parsing preferred control-plane VIP %q: %w", preferred, err)
	}
	if !preferredAddr.Is4() {
		return "", fmt.Errorf("preferred control-plane VIP %s is not IPv4", preferredAddr)
	}
	if _, found := used[preferredAddr.String()]; !found {
		return preferredAddr.String(), nil
	}

	prefix := netip.PrefixFrom(preferredAddr, 24).Masked()
	for candidate := preferredAddr.Next(); prefix.Contains(candidate.Next()); candidate = candidate.Next() {
		if _, found := used[candidate.String()]; !found {
			return candidate.String(), nil
		}
	}
	return "", fmt.Errorf("no unused control-plane VIP at or after %s in %s", preferredAddr, prefix)
}

// capdSpec builds a CAPD topology with the requested cluster shape.
func capdSpec(cfg config.Config, name string, shape clusterShape) nke.ClusterSpec {
	return nke.ClusterSpec{
		Name:              name,
		Backend:           nke.BackendCAPD,
		KubernetesVersion: defaultString(shape.kubernetesVersion, defaultKubernetesVersion),
		CNIPlugin:         defaultString(shape.cniPlugin, CNIPluginCalico),
		CAPDNodeImage:     cfg.Cluster.CAPDNodeImage,
		SiteID:            uuid.Nil.String(),
		ControlPlane: nke.ControlPlane{
			Pools:           capdNodePoolSpecs(shape.controlPlanePools, "control-plane", "cp"),
			EtcdEncrypted:   shape.etcdEncrypted,
			ControlPlaneVIP: shape.controlPlaneVIP,
		},
		Workers: capdNodePoolSpecs(shape.workerPools, "worker", "w"),
	}
}

// capdControlPlaneControllerSpec builds a CAPD topology whose control plane is
// provisioned by the cluster-level control-plane controller: no control-plane
// node pool, a cluster-level control plane config instead. Requires CWE deployed
// with PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=true.
func capdControlPlaneControllerSpec(cfg config.Config, name string, shape clusterShape) nke.ClusterSpec {
	spec := capdSpec(cfg, name, shape)
	spec.ControlPlane.Pools = nil
	spec.ControlPlane.Config = shape.controlPlaneConfig
	return spec
}

// nicoVMSControlPlaneControllerSpec builds a nico-vms topology whose control
// plane is provisioned by the cluster-level control-plane controller: no
// control-plane node pool. Requires CWE deployed with
// PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=true.
func nicoVMSControlPlaneControllerSpec(cfg config.Config, name string, shape clusterShape) nke.ClusterSpec {
	spec := nicoVMSSpec(cfg, name, shape)
	spec.ControlPlane.Pools = nil
	spec.ControlPlane.Config = shape.controlPlaneConfig
	return spec
}

// nicoControlPlaneControllerSpec is the same for the container-backed NICo
// topology. Requires CWE deployed with
// PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=true.
func nicoControlPlaneControllerSpec(cfg config.Config, name string, shape clusterShape) nke.ClusterSpec {
	spec := nicoSpec(cfg, name, shape)
	spec.ControlPlane.Pools = nil
	spec.ControlPlane.Config = shape.controlPlaneConfig
	if spec.ControlPlane.Config != nil {
		// The mock's seeded identifiers, the same ones nicoNodePoolSpecs applies
		// to a pool. Without them the machine template has no instance type or
		// subnet and CWE rejects the create.
		cp := *spec.ControlPlane.Config
		cp.InstanceTypeID = defaultString(cp.InstanceTypeID, nicoInstanceTypeID)
		cp.SubnetIDs = defaultStrings(cp.SubnetIDs, nicoSubnetID)
		cp.SSHKeyGroupIDs = defaultStrings(cp.SSHKeyGroupIDs, nicoSSHKeyGroupID)
		cp.SystemReservedMemory = defaultString(cp.SystemReservedMemory, "1Gi")
		spec.ControlPlane.Config = &cp
	}
	return spec
}

func TestUnit_CAPDSpec(t *testing.T) {
	tests := map[string]string{
		"explicit VIP": capdControlPlaneVIP,
		"empty VIP":    "",
	}

	for name, wantVIP := range tests {
		t.Run(name, func(t *testing.T) {
			spec := capdSpec(config.Config{}, "capd-test", clusterShape{controlPlaneVIP: wantVIP})
			if spec.SiteID != uuid.Nil.String() {
				t.Fatalf("SiteID = %q, want %q", spec.SiteID, uuid.Nil.String())
			}
			if spec.ControlPlane.ControlPlaneVIP != wantVIP {
				t.Fatalf("ControlPlaneVIP = %q, want %q", spec.ControlPlane.ControlPlaneVIP, wantVIP)
			}
		})
	}
}

func capdNodePoolSpecs(pools []nke.NodePoolSpec, defaultName, defaultNodeNamePrefix string) []nke.NodePoolSpec {
	out := make([]nke.NodePoolSpec, 0, len(pools))
	for _, pool := range pools {
		out = append(out, capdNodePoolSpec(pool, defaultName, defaultNodeNamePrefix))
	}
	return out
}

func capdNodePoolSpec(pool nke.NodePoolSpec, defaultName, defaultNodeNamePrefix string) nke.NodePoolSpec {
	return nke.NodePoolSpec{
		Name:                 defaultString(pool.Name, defaultName),
		NodeNamePrefix:       defaultString(pool.NodeNamePrefix, defaultNodeNamePrefix),
		DesiredNodeCount:     pool.DesiredNodeCount,
		SystemReservedMemory: defaultString(pool.SystemReservedMemory, "1Gi"),
	}
}

// NICo container seed identifiers, mirroring hack/create-local-nico-cluster.sh.
const (
	nicoSiteID         = "00000000-0000-0000-0000-000000000001"
	nicoVpcID          = "00000000-0000-0000-0000-000000000002"
	nicoSubnetID       = "00000000-0000-0000-0000-000000000003"
	nicoSSHKeyGroupID  = "00000000-0000-0000-0000-000000000004"
	nicoInstanceTypeID = "00000000-0000-0000-0000-000000000005"

	// Advertised by the bgp-containers provider this lane already deploys, as
	// capdControlPlaneVIP is for CAPD. postReadyChecks points a kubelet at it,
	// so it has to route, not merely be reported.
	nicoControlPlaneVIP = "172.31.2.1"
)

// nicoSpec builds a CAPNico topology backed by the local mock's Docker
// containers. Like CAPD, it uses CWE's nil-UUID CAPI site; the Nico site and
// VPC live inside the infrastructure manifests.
func nicoSpec(_ config.Config, name string, shape clusterShape) nke.ClusterSpec {
	return nke.ClusterSpec{
		Name:              name,
		Backend:           nke.BackendNicoContainers,
		KubernetesVersion: defaultString(shape.kubernetesVersion, defaultKubernetesVersion),
		CNIPlugin:         defaultString(shape.cniPlugin, CNIPluginCalico),
		SiteID:            nicoSiteID,
		VpcID:             nicoVpcID,
		ControlPlane: nke.ControlPlane{
			Pools:           nicoNodePoolSpecs(shape.controlPlanePools, "control-plane", "cp"),
			EtcdEncrypted:   shape.etcdEncrypted,
			ControlPlaneVIP: shape.controlPlaneVIP,
		},
		Workers: nicoNodePoolSpecs(shape.workerPools, "worker", "w"),
	}
}

func nicoNodePoolSpecs(pools []nke.NodePoolSpec, defaultName, defaultNodeNamePrefix string) []nke.NodePoolSpec {
	out := make([]nke.NodePoolSpec, 0, len(pools))
	for _, pool := range pools {
		out = append(out, nke.NodePoolSpec{
			Name:                 defaultString(pool.Name, defaultName),
			NodeNamePrefix:       defaultString(pool.NodeNamePrefix, defaultNodeNamePrefix),
			DesiredNodeCount:     pool.DesiredNodeCount,
			SystemReservedMemory: defaultString(pool.SystemReservedMemory, "1Gi"),
			InstanceTypeID:       defaultString(pool.InstanceTypeID, nicoInstanceTypeID),
			SubnetIDs:            defaultStrings(pool.SubnetIDs, nicoSubnetID),
			SSHKeyGroupIDs:       defaultStrings(pool.SSHKeyGroupIDs, nicoSSHKeyGroupID),
		})
	}
	return out
}

// nico-vms Forge emulator seed identifiers, mirroring
// cloud-workflow-engine/nico-sim/internal/seed/default-seed.yaml. The local
// nico-vms environment always seeds these deterministic IDs.
const (
	nicoVMSOrgID           = "novxwn552ssc"
	nicoVMSSiteID          = "a1b2c3d4-0001-4000-8000-000000000001"
	nicoVMSVpcID           = "a1b2c3d4-0002-4000-8000-000000000001"
	nicoVMSVpcPrefixID     = "a1b2c3d4-0004-4000-8000-000000000001"
	nicoVMSInstanceTypeID  = "a1b2c3d4-0005-4000-8000-000000000001"
	nicoVMSLargeInstanceID = "a1b2c3d4-0005-4000-8000-000000000003"
	nicoVMSSSHKeyGroupID   = "a1b2c3d4-0006-4000-8000-000000000001"
	nicoVMSOSProfile       = "nke-nico-vms-24.04-amd64-fde"
	nicoVMSCPConfigProfile = "nico-vms-cp"
	nicoVMSWConfigProfile  = "nico-vms-worker"
	nicoVMSControlPlaneVIP = "172.31.0.10"
)

// nicoVMSSpec builds a nico-vms topology using the local emulator's seed
// identifiers. kube-vip is left enabled (the nico-vms BGP speaker advertises
// the control-plane VIP), unlike the CAPD topology.
func nicoVMSSpec(_ config.Config, name string, shape clusterShape) nke.ClusterSpec {
	return nke.ClusterSpec{
		Name:              name,
		Backend:           nke.BackendNicoVMS,
		KubernetesVersion: defaultString(shape.kubernetesVersion, defaultKubernetesVersion),
		// NKE_E2E_NICO_VMS_CNI moves the default for entries that do not pin one; Calico's
		// images are a third of OVN's and import in a third of the time on every node.
		CNIPlugin: defaultString(shape.cniPlugin, defaultString(os.Getenv("NKE_E2E_NICO_VMS_CNI"), CNIPluginOVN)),
		OrgID:     nicoVMSOrgID,
		SiteID:    nicoVMSSiteID,
		VpcID:     nicoVMSVpcID,
		ControlPlane: nke.ControlPlane{
			Pools:           nicoVMSNodePoolSpecs(shape.controlPlanePools, "control-plane", "cp", nicoVMSOSProfile, nicoVMSCPConfigProfile),
			ControlPlaneVIP: shape.controlPlaneVIP,
			EtcdEncrypted:   shape.etcdEncrypted,
			Config:          shape.controlPlaneConfig,
		},
		Workers: nicoVMSNodePoolSpecs(shape.workerPools, "worker", "w", nicoVMSOSProfile, nicoVMSWConfigProfile),
	}
}

func nicoVMSNodePoolSpecs(pools []nke.NodePoolSpec, defaultName, defaultNodeNamePrefix, defaultOSProfile, defaultConfigProfile string) []nke.NodePoolSpec {
	out := make([]nke.NodePoolSpec, 0, len(pools))
	for _, pool := range pools {
		out = append(out, nicoVMSNodePoolSpec(pool, defaultName, defaultNodeNamePrefix, defaultOSProfile, defaultConfigProfile))
	}
	return out
}

func nicoVMSNodePoolSpec(pool nke.NodePoolSpec, defaultName, defaultNodeNamePrefix, defaultOSProfile, defaultConfigProfile string) nke.NodePoolSpec {
	return nke.NodePoolSpec{
		Name:                  defaultString(pool.Name, defaultName),
		NodeNamePrefix:        defaultString(pool.NodeNamePrefix, defaultNodeNamePrefix),
		DesiredNodeCount:      pool.DesiredNodeCount,
		SystemReservedMemory:  defaultString(pool.SystemReservedMemory, "250Mi"),
		KubeReservedMemory:    defaultString(pool.KubeReservedMemory, "250Mi"),
		InstanceTypeID:        defaultString(pool.InstanceTypeID, nicoVMSInstanceTypeID),
		VpcPrefixIDs:          defaultStrings(pool.VpcPrefixIDs, nicoVMSVpcPrefixID),
		SSHKeyGroupIDs:        defaultStrings(pool.SSHKeyGroupIDs, nicoVMSSSHKeyGroupID),
		OSProfile:             defaultString(pool.OSProfile, defaultOSProfile),
		ConfigProfile:         defaultString(pool.ConfigProfile, defaultConfigProfile),
		DiskSelectionStrategy: defaultString(pool.DiskSelectionStrategy, "smallest"),
	}
}

func defaultString(s, defaultValue string) string {
	if s == "" {
		return defaultValue
	}
	return s
}

func defaultStrings(s []string, defaultValue string) []string {
	if len(s) == 0 {
		return []string{defaultValue}
	}
	return s
}

func nicoVMSMultiPodCIDRSpec(cfg config.Config, name string, shape clusterShape) nke.ClusterSpec {
	spec := nicoVMSSpec(cfg, name, shape)
	spec.Name = name + "-multi-pod-cidr"
	spec.Network = &nke.ClusterNetworkSpec{
		PodCIDR:          nicoVMSMultiPodCIDR,
		ServiceCIDR:      nicoVMSMultiPodServiceCIDR,
		ExpectedPodCIDRs: []string{"10.250.0.0/23", "10.252.0.0/22"},
	}
	return spec
}

// expectSucceed fails the spec if err is non-nil and otherwise returns v.
func expectSucceed[T any](v T, err error, messages ...any) T {
	GinkgoHelper()
	Expect(err).NotTo(HaveOccurred(), messages...)
	return v
}
