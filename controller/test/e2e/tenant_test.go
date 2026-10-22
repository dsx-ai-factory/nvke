// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/dsx"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/forge"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvkectl"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/tenantauth"
)

// clusterLifecycle creates a cluster, reads it back, and deletes it again.
// *nvke.Client satisfies it and is every entry's default; the nvkectl entry
// substitutes a CLI-backed implementation.
//
// It stays narrow because nvkectl has no node-level commands: the post-create
// validations keep using t.nvke directly rather than widening this into
// something the CLI cannot implement.
type clusterLifecycle interface {
	CreateCluster(ctx context.Context, spec nvke.ClusterSpec) (*api.Cluster, error)
	GetByName(ctx context.Context, name string) (*api.Cluster, error)
	WaitForClusterReady(ctx context.Context, id openapi_types.UUID) (*api.Cluster, error)
	DeleteCluster(ctx context.Context, id openapi_types.UUID) error
	WaitForClusterDeleted(ctx context.Context, id openapi_types.UUID) error
}

// tenantTest carries everything a backend's specs share: the wiring built once
// in the create spec, plus the cluster created there.
type tenantTest struct {
	log           logr.Logger
	cfg           config.Config
	mgmt          managementCluster
	k8sClient     client.Client
	tenantClients *kube.TenantClientFactory
	nvke          *nvke.Client
	// lifecycle creates and deletes the cluster. Defaults to nke; the nvkectl
	// entry swaps it so that row drives both ends through the CLI.
	lifecycle clusterLifecycle
	// nvkectlRunner is set only by the nvkectl entry, for the CLI-only
	// validations that follow the create. Nil everywhere else.
	nvkectlRunner *nvkectl.Runner
	timeouts      nvke.Options
	trustDomain   string
	spec          nvke.ClusterSpec
	cluster       *api.Cluster
	// cascadePoolID is set by the node-pool cascade post-create spec and read by
	// its post-delete counterpart to prove cluster deletion removed the pool.
	cascadePoolID uuid.UUID
	// k8sClientset reads what the controller-runtime k8sClient cannot: the log
	// of a failed verifier pod.
	k8sClientset kubernetes.Interface
}

var _ = DescribeTableSubtree("NVKE with backend", withNicoVMSReleaseEntries(
	tenantSubtree(configuredManagement),
	// The pull-request smoke lane uses a separate, minimal topology instead of
	// filtering the full CAPD row below. The full row needs three control-plane
	// nodes for its scale, rotation, and certificate checks; the core lane only
	// proves that the deployed controller's control-plane path can create a usable
	// cluster and delete it again.
	Entry("CAPD node-agent convergence", Ordered, ContinueOnFailure, Label("node-agent-upgrade"), testSpec{
		buildSpec: capdSpec,
		shape:     clusterShape{controlPlaneConfig: &nvke.ControlPlaneConfigSpec{DesiredCount: 3, NodeAgentVersion: "v0.1.991", SystemReservedMemory: "1Gi"}, workerPools: []nvke.NodePoolSpec{{DesiredNodeCount: 1}}},
		options:   nvke.Options{Provision: 15 * time.Minute, Delete: 5 * time.Minute, Poll: 2 * time.Second},
		validate:  validations{provisioningOnly: true, nodeAgentUpgrade: true},
	}),
	Entry("CAPD core", Ordered, ContinueOnFailure, Label("core", "core-docker"), testSpec{
		buildSpec: capdSpec,
		shape: clusterShape{
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
			workerPools:       []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options:  nvke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		validate: validations{provisioningOnly: true},
	}),
	Entry("CAPD", Ordered, ContinueOnFailure, Label("capd", "happy-path", "kube-vip-route-withdrawal", "cert-renewal"), testSpec{
		buildSpec: capdSpec,
		shape: clusterShape{
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 3}},
			workerPools:       []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options: nvke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		// The last four were nico-vms-only until nico-containers took them, which
		// left CAPD the one backend in CI not asserting them. They call the controller's
		// node-pool API and read cluster fields; nothing here is backend-specific.
		validate: validations{
			scale: true, reboot: true, postReadyChecks: true,
			certificateRenewal: true, csrApproval: true, nodePoolLifecycle: true,
			nodePoolCascade: true, nodeReplacement: true,
			manualScaleDown: true, clusterConfig: true,
			kubeVipRouteWithdrawal: true,
		},
	}),
	// The CLI row: the same CAPD lifecycle as the rows above, driven through the
	// nvkectl binary instead of the generated API client. It is the only coverage
	// of provisioning through the command line, and so the only place
	// `get kubeconfig` succeeds and a patch lands on a cluster that came up --
	// hack/ci/nvkectl-smoke.sh sets autoProvisionDisabled, so its cluster never
	// provisions and the Secret that command reads never appears.
	//
	// One control-plane node, no workers, and provisioningOnly: what this row
	// adds over the others is the transport, not the assertions.
	//
	// The "capd" label puts it in the full CAPD run on main, scheduled and manual
	// runs, on the substrate that run already builds. The bounded
	// pull-request lane selects only "core-docker" and has no room for a second
	// cluster. "nvkectl" selects this row alone.
	Entry("CAPD via nvkectl", Ordered, ContinueOnFailure, Label("capd", "nvkectl"), testSpec{
		buildSpec: capdSpec,
		shape: clusterShape{
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options:   nvke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		createVia: clientNvkectl,
		validate:  validations{provisioningOnly: true},
	}),
	// Mirrors the CAPD row above so the node pool lifecycle is covered on the
	// NICo substrate too; the CLI creates the worker pool this row cares about.
	Entry("nico-containers via nvkectl", Ordered, ContinueOnFailure, Label("nico-containers", "nvkectl"), testSpec{
		buildSpec: nicoSpec,
		shape: clusterShape{
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options:   nvke.Options{Provision: 15 * time.Minute, Delete: 10 * time.Minute, Poll: 5 * time.Second},
		createVia: clientNvkectl,
		validate:  validations{provisioningOnly: true},
	}),
	// This row is selected only by the dedicated identity-exchange lane. Keeping
	// it off the broad "capd" label prevents the direct-SPIRE control lane from
	// exercising a topology it did not deploy.
	Entry("CAPD with DSX identity exchange", Ordered, ContinueOnFailure, Label("identity-exchange"), testSpec{
		buildSpec: capdSpec,
		shape: clusterShape{
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options: nvke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
	}),
	// Opt-in via the "cp-controller" label: it exercises cluster-level
	// control-plane-controller provisioning (no control-plane node pool) and its
	// update, rolling-update and delete specs.
	Entry("CAPD control-plane controller (no CP node pool)", Ordered, ContinueOnFailure, Label("capd", "cp-controller"), testSpec{
		buildSpec: capdControlPlaneControllerSpec,
		shape: clusterShape{
			controlPlaneConfig: &nvke.ControlPlaneConfigSpec{DesiredCount: 1, NodeLabels: map[string]string{controlPlaneTestLabelKey: "true"}},
			workerPools:        []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		// 15m, matching the nico-containers entry below. The roll waits on a
		// replacement node reaching KubernetesReady, and the local-entrypoint
		// CNI-after-init leg straddles 10m.
		options: nvke.Options{Provision: 15 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		validate: validations{
			controlPlaneController:              true,
			controlPlaneControllerUpdate:        true,
			controlPlaneControllerRollingUpdate: true,
			controlPlaneControllerDelete:        true,
			controlPlaneNodeLabels:              true,
		},
	}),
	Entry("CAPD control-plane controller without kube-vip", Ordered, ContinueOnFailure, Label("capd", "cp-controller", "no-vip"), testSpec{
		buildSpec: capdControlPlaneControllerSpec,
		shape: clusterShape{
			controlPlaneConfig:     &nvke.ControlPlaneConfigSpec{DesiredCount: 1},
			withoutControlPlaneVIP: true,
		},
		options:  nvke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		validate: validations{provisioningOnly: true, controlPlaneController: true, noVIPControllerScale: true, noVIPUpgrade: true},
	}),
	// A worker-only advertised domain leaves the controller with no eligible
	// control-plane placement. Keep this as a separate cluster so the spread and
	// rolling-update row above retains its two eligible domains.
	Entry("CAPD control-plane controller without an eligible failure domain", Ordered, ContinueOnFailure,
		Label("capd", "cp-controller", "no-eligible-failure-domain"), testSpec{
			buildSpec: capdNoEligibleControlPlaneDomainSpec,
			shape: clusterShape{
				controlPlaneConfig: &nvke.ControlPlaneConfigSpec{DesiredCount: 1},
				workerPools:        []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
			},
			options:  nvke.Options{Provision: 15 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
			validate: validations{provisioningOnly: true, controlPlaneController: true},
		}),
	// The same for nico-containers, so the cp-controller path is not CAPD-only.
	// Three control-plane nodes because the rolling-update spec needs a
	// multi-node control plane to keep etcd quorum while it rolls, and because
	// the delete specs assert the quorum floor refuses a delete at exactly that
	// size. The delete specs grow the control plane to four and back, which is
	// cheap here (containers) but is why nico-vms does not run them.
	Entry("nico-containers control-plane controller (no CP node pool)", Ordered, ContinueOnFailure, Label("nico-containers", "cp-controller"), testSpec{
		buildSpec: nicoControlPlaneControllerSpec,
		shape: clusterShape{
			controlPlaneConfig: &nvke.ControlPlaneConfigSpec{DesiredCount: 3},
			workerPools:        []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options: nvke.Options{Provision: 15 * time.Minute, Delete: 10 * time.Minute, Poll: 5 * time.Second},
		validate: validations{
			controlPlaneController:              true,
			controlPlaneControllerUpdate:        true,
			controlPlaneControllerRollingUpdate: true,
			controlPlaneControllerDelete:        true,
		},
	}),
	Entry("nico-vms control-plane controller (no CP node pool)", Ordered, ContinueOnFailure, Label("nico-vms", "cp-controller"), testSpec{
		buildSpec: nicoVMSSpec,
		shape: clusterShape{
			kubernetesVersion: "1.34.2",
			controlPlaneConfig: &nvke.ControlPlaneConfigSpec{
				DesiredCount:         1,
				InstanceTypeID:       nicoVMSLargeInstanceID,
				Release:              nicoVMSRelease,
				ConfigProfile:        nicoVMSCPConfigProfile,
				VpcPrefixIDs:         []string{nicoVMSVpcPrefixID},
				SSHKeyGroupIDs:       []string{nicoVMSSSHKeyGroupID},
				SystemReservedMemory: "1Gi",
			},
			workerPools: []nvke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
		},
		// A healthy create takes about 8.5 minutes, and nothing below gives up on
		// its own, so this wait is what a broken node costs.
		options:  nvke.Options{Provision: 30 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
		validate: validations{controlPlaneController: true, controlPlaneControllerUpdate: true, controlPlaneControllerRollingUpdate: true},
	}),
	// NICo gets the same bounded pull-request contract as CAPD. Keep this entry
	// off the broad nico-containers label so scheduled full runs do not repeat
	// the same lifecycle with a smaller topology.
	Entry("nico-containers core", Ordered, ContinueOnFailure, Label("core", "core-nico"), testSpec{
		buildSpec: nicoSpec,
		shape: clusterShape{
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
			workerPools:       []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options:  nvke.Options{Provision: 15 * time.Minute, Delete: 10 * time.Minute, Poll: 5 * time.Second},
		validate: validations{provisioningOnly: true},
	}),
	Entry("nico-containers", Ordered, ContinueOnFailure, Label("nico-containers", "happy-path"), testSpec{
		buildSpec: nicoSpec,
		shape: clusterShape{
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 3}},
			workerPools:       []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options: nvke.Options{Provision: 15 * time.Minute, Delete: 10 * time.Minute, Poll: 5 * time.Second},
		// Everything CAPD asserts on this shape, plus the node-pool operations
		// nico-vms carries. None of these reaches the infrastructure API: they
		// call the controller's REST surface or the tenant cluster, and both backends use
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
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 3}},
			etcdEncrypted:     true,
		},
		options:  nvke.Options{Provision: 15 * time.Minute, Delete: 10 * time.Minute, Poll: 5 * time.Second},
		validate: validations{etcdEncryption: true},
	}),
	Entry("nico-containers brownfield etcd encryption", Ordered, ContinueOnFailure, Label("nico-containers", "extended", "encryption", "etcd-encryption-enable"), testSpec{
		buildSpec: nicoSpec,
		shape: clusterShape{
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 3}},
		},
		options:  nvke.Options{Provision: 15 * time.Minute, Delete: 10 * time.Minute, Poll: 5 * time.Second},
		validate: validations{brownfieldEtcdEncryption: true},
	}),
	Entry("nico-containers brownfield OIDC enablement", Ordered, ContinueOnFailure, Label("nico-containers", "extended", "oidc", "oidc-enable"), testSpec{
		buildSpec: nicoSpec,
		shape: clusterShape{
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 3}},
		},
		options:  nvke.Options{Provision: 15 * time.Minute, Delete: 10 * time.Minute, Poll: 5 * time.Second},
		validate: validations{brownfieldOIDC: true},
	}),
	Entry("nico-vms", Ordered, ContinueOnFailure, Label("nico-vms", "happy-path"), testSpec{
		buildSpec: nicoVMSSpec,
		shape: clusterShape{
			kubernetesVersion: "1.34.2",
			// OVN-Kubernetes, via the nico-vms default. Open vSwitch is asserted
			// on the "nico-vms ovn" entry below. Large instances kept:
			// cpu2-mem2gb was sized against ovnkube-node.
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 3, InstanceTypeID: nicoVMSLargeInstanceID}},
			workerPools:       []nvke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
		},
		options:  nvke.Options{Provision: 30 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
		validate: validations{scale: true, reboot: true},
	}),
	Entry("nico-vms k8s upgrade", Ordered, ContinueOnFailure, Label("nico-vms", "happy-path", "upgrade"), testSpec{
		buildSpec: nicoVMSSpec,
		shape: clusterShape{
			kubernetesVersion: upgradeStartingKubernetesVersion,
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 3, InstanceTypeID: nicoVMSLargeInstanceID}},
			workerPools:       []nvke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
		},
		options:  nvke.Options{Provision: 30 * time.Minute, Upgrade: 90 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
		validate: validations{upgrade: true},
	}),
	// OVN-Kubernetes is the default CNI for nico-vms entries, so most entries
	// exercise it implicitly through NKE_E2E_NICO_VMS_CNI. This entry is the
	// small OVN smoke: it pins OVN explicitly and asserts the typed Open
	// vSwitch node-agent RPC and NetworkPolicy enforcement. Kept to one
	// control plane and one worker: the assertions are per node, and OVN
	// images are the expensive part.
	Entry("nico-vms ovn", Ordered, ContinueOnFailure, Label("nico-vms", "ovn"), testSpec{
		buildSpec: nicoVMSSpec,
		shape: clusterShape{
			kubernetesVersion: "1.34.2",
			cniPlugin:         CNIPluginOVN,
			// Large instances: cpu2-mem2gb was sized against ovnkube-node.
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
			workerPools:       []nvke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
		},
		options:  nvke.Options{Provision: 45 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
		validate: validations{openVSwitch: true, ovnNetworking: true},
	}),
	Entry("CAPD with etcd encryption", Ordered, ContinueOnFailure, Label("capd", "extended", "encryption"), testSpec{
		buildSpec: capdSpec,
		shape: clusterShape{
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 3}},
			etcdEncrypted:     true,
		},
		options:  nvke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		validate: validations{etcdEncryption: true},
	}),
	Entry("CAPD brownfield etcd encryption", Ordered, ContinueOnFailure, Label("capd", "extended", "encryption", "etcd-encryption-enable"), testSpec{
		buildSpec: capdSpec,
		shape: clusterShape{
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 3}},
		},
		options:  nvke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		validate: validations{brownfieldEtcdEncryption: true},
	}),
	Entry("CAPD brownfield OIDC enablement", Ordered, ContinueOnFailure, Label("capd", "extended", "oidc", "oidc-enable"), testSpec{
		buildSpec: capdSpec,
		shape: clusterShape{
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 3}},
		},
		options:  nvke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		validate: validations{brownfieldOIDC: true},
	}),
	Entry("nico-vms releases", Ordered, ContinueOnFailure, Label("nico-vms", "extended", "releases"), testSpec{
		buildSpec: nicoVMSSpec,
		shape: clusterShape{
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
			workerPools: []nvke.NodePoolSpec{
				// Old agent image pins the agent version to 0.1.22, which is before the new nvkeproto.CheckCloudInitStatus was introduced in version 0.1.24.
				// So this release test exists to prove that the controller can still provision nodePools that may still be pointed at older agent versions.
				{Name: "worker-old-agent", NodeNamePrefix: "wold", DesiredNodeCount: 1, Release: "nvke-nico-vms-24.04-amd64-fde-old-agent"},
				// nicoVMSNodePoolSpec defaults kube/system reserved memory to 250Mi so the
				// default nico-vms instance type has enough allocatable memory.
				{Name: "worker-260625-24.04", NodeNamePrefix: "w260625", DesiredNodeCount: 1, Release: "nke-260625-24.04-x86-fde"},
				{Name: "worker-260602-24.04", NodeNamePrefix: "w260602", DesiredNodeCount: 1, Release: "nke-260602-24.04-x86-fde"},
			},
		},
		options: nvke.Options{Provision: 30 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
	}),
	Entry("nico-vms multi pod CIDRs", Ordered, ContinueOnFailure, Label("nico-vms", "multi-pod-cidrs"), testSpec{
		buildSpec: nicoVMSMultiPodCIDRSpec,
		shape: clusterShape{
			kubernetesVersion: "1.34.2",
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 3, InstanceTypeID: nicoVMSLargeInstanceID}},
			workerPools:       []nvke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
		},
		options: nvke.Options{Provision: 30 * time.Minute, Delete: 10 * time.Minute, Poll: 30 * time.Second},
	}),
	Entry("nico-vms node pool lifecycle", Ordered, ContinueOnFailure, Label("nico-vms", "extended", "nodepool"), testSpec{
		buildSpec: nicoVMSSpec,
		shape: clusterShape{
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
			workerPools:       []nvke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: nicoVMSLargeInstanceID}},
		},
		options:  nvke.Options{Provision: 30 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
		validate: validations{clusterConfig: true, nodeReplacement: true, manualScaleDown: true, nodePoolLifecycle: true, nodePoolCascade: true},
	}),
	// The only entry that runs on hardware, and so the only one needing
	// -e2e.environment. Opt-in via the "nico-forge" label, and that label
	// alone: everything above runs against a simulator on the runner, while
	// this one creates real instances in a real Forge site and costs real
	// money. A scenario label such as "happy-path" would pull it into
	// test-e2e-happy-path, which a developer runs expecting a simulator.
	//
	// Node counts are the smallest that still prove a control plane and a
	// worker, and the pools name no instance type: the environment file's
	// declarations answer -e2e.capabilities instead, so the machine is chosen
	// by what the run needs rather than pinned in the table.
	//
	// The validations it runs, and why the rest are off, are on the entry
	// itself: what this site's capacity and shape allow rather than what the
	// subtree offers.
	//
	// needs: is the tenant's own machines, a CPU control plane and a GPU
	// worker, on a site that already exists. The site lane's entry carries
	// the same tenant under a container that also counts the site node.
	Entry("nico-forge", Ordered, ContinueOnFailure, Label("nico-forge", "needs:cpu=1", "needs:gpu=1"), nicoForgeTenant()),
)...)

// nicoForgeTenant is the Forge tenant both the nico-forge lane and the site
// lane create, so a site the suite built is held to the same tenant as a site
// that already existed.
//
// A function rather than a package variable because two trees use it. Entry
// copies the struct, but the copy is shallow: a shared value would hand both
// lanes the same pool slices and the same capabilities map, and a builder
// that ever wrote a default into one would change the other lane with nothing
// in either file to show why.
func nicoForgeTenant() testSpec {
	return testSpec{
		buildSpec: nicoForgeSpec,
		shape: clusterShape{
			// Cluster-level rather than a control-plane node pool: the site's controller
			// rejects such a pool at create.
			controlPlaneConfig: &nvke.ControlPlaneConfigSpec{DesiredCount: 1},
			workerPools:        []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		// Generous next to the simulator lanes: these timeouts cover a bare
		// machine booting an OS image over the network and installing
		// packages, none of which a container does.
		//
		// Provision is 45 rather than the 90 it was. A tenant that comes up
		// takes about 16 minutes, measured across the runs that have passed,
		// so 90 was five times the work and the whole of it was spent
		// whenever a node could not be provisioned: the controller retries forever, so
		// nothing flips Provisioning to False and the deadline is the only
		// thing that ends the wait. Two runs cost two hours each to report
		// that the time ran out. The wait also gives up when the message
		// stops changing, which ends a stuck one sooner than either number.
		//
		// Delete is 75 minutes because Forge takes 20 to 40 to actually
		// release a machine, and sometimes much longer, and the cluster is not
		// gone until both of its machines are. Run 36051890151 failed the
		// delete at 30 minutes, and run 38690403898 on site-1 L40S still had the
		// control plane releasing after 45 plus the sweep's 15. It is capped
		// by the suite's 150-minute -ginkgo.timeout: the delete starts about
		// 42 minutes in, and the site sweep and release still need about 25
		// after it.
		options: nvke.Options{Provision: 45 * time.Minute, Delete: 75 * time.Minute, Poll: 30 * time.Second},
		// Provisioning only, because that is the path this lane exercises:
		// a tenant is created on real hardware, its nodes reach Ready, and it
		// is deleted. The rest assert against a running cluster and belong to
		// a lane that has one for longer than a GB200 can be held.
		//
		// TPM attestation is the exception: it reads what the nodes already
		// did to join, so it costs seconds, and this is the one lane whose
		// machines attest with a hardware TPM. The tenant checks are the
		// other: they read the cluster as built and run short-lived pods,
		// minutes against the hour the tenant is held, and qualifying them on
		// hardware is what they are for.
		validate: validations{provisioningOnly: true, tpmAttestation: true, tenantChecks: true},
	}
}

// nicoDSXTenant is nicoForgeTenant on a DSX datahall: one control-plane node
// and one worker, provisioned and deleted, validating nothing that needs a
// cluster held open.
//
// The shape is the same because the constraint is the same. At a datahall
// whose every machine is a GPU tray both roles land on one, so this holds two
// trays for the run rather than a tray and a CPU box, and the prefix it
// draws /31s from leaves one address spare behind the site controller.
func nicoDSXTenant() testSpec {
	spec := nicoForgeTenant()
	spec.buildSpec = nicoDSXSpec
	return spec
}

// tenantSubtree is one backend's tenant lifecycle against the management
// cluster resolve names. resolve runs when the create spec does, not when the
// tree is built, so a lane that builds its management cluster in a BeforeAll
// can hand over a kubeconfig that did not exist at tree construction.
func tenantSubtree(resolve managementResolver) func(spec testSpec) {
	return func(spec testSpec) {
		var (
			test    tenantTest
			failed  bool
			deleted bool
			// Set once the controller has accepted a delete for this cluster, before the
			// wait for it to go. deleted alone cannot carry that: it means the
			// wait succeeded, so a timed-out wait left it false and the
			// AfterAll below spent a second full budget on the same cluster.
			// It stays false when the delete request itself failed, so the
			// AfterAll still retries a delete the controller never took.
			deleteAccepted      bool
			previousAgentMethod string
			agentMethodChanged  bool
		)

		// Record any spec failure so the cluster delete can honor preserveOnFailure.
		AfterEach(func() { failed = failed || CurrentSpecReport().Failed() })

		It("creates the cluster", func(ctx context.Context) {
			test = expectSucceed(setupTenantTest(ctx, expectSucceed(resolve()), spec.options))
			if spec.createVia == clientNvkectl {
				// Torn down in AfterAll. A DeferCleanup registered here would run
				// when this spec ends, taking the port-forward away from every
				// spec that follows.
				test.useNvkectl(ctx, spec.options)
			}
			if spec.validate.brownfieldOIDC {
				// A cluster on an OIDC-enabled site is always born OIDC-on, but
				// brownfield enablement needs an OIDC-off cluster to act on. Turn the
				// controller create default off (the OIDC issuer settings stay, so the enable
				// workflow still runs once it is flipped back on) so this cluster is
				// created OIDC-off; validateBrownfieldOIDC flips it on and enables.
				// Restored in AfterAll.
				By("Creating the brownfield cluster with the controller OIDC create-default turned off")
				Expect(nvke.SetOIDCEnabled(ctx, test.k8sClient, false)).To(Succeed())
			}
			if spec.validate.nodeAgentUpgrade {
				Expect(os.Getenv("NVKE_E2E_NODE_AGENT_UPGRADE")).To(Equal("true"), "requires isolated node-agent fixtures")
				var methodErr error
				previousAgentMethod, agentMethodChanged, methodErr = nvke.SetNodeAgentUpdateMethod(ctx, test.k8sClient, "bundle")
				Expect(methodErr).NotTo(HaveOccurred())
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
			if spec.validate.tpmAttestation {
				validateTPMAttestationPostCreate(&test)
			}
			if spec.validate.nodeAgentUpgrade {
				validateNodeAgentUpgrade(&test)
			}
			if spec.validate.openVSwitch {
				validateOpenVSwitchPostCreate(&test)
			}
			if spec.validate.ovnNetworking {
				validateOVNNetworkPolicyPostCreate(&test)
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
			if spec.validate.controlPlaneController {
				validateControllerManagedControlPlane(&test)
			}
			if spec.validate.controlPlaneNodeLabels {
				validateControlPlaneNodeLabels(&test, "after create")
			}
			if spec.validate.controlPlaneControllerUpdate {
				validateControllerManagedControlPlaneUpdate(&test)
			}
			if spec.validate.noVIPControllerScale {
				validateNoVIPControllerScaleRefused(&test)
			}
			if spec.validate.noVIPUpgrade {
				validateNoVIPUpgradeRefused(&test)
			}
			if spec.validate.controlPlaneControllerRollingUpdate {
				validateControllerManagedControlPlaneRollingUpdate(&test)
				if spec.validate.controlPlaneNodeLabels {
					validateControlPlaneNodeLabels(&test, "after the rolling update")
				}
			}
			if spec.validate.controlPlaneControllerDelete {
				validateControllerManagedControlPlaneDeleteRefused(&test)
				validateControllerManagedControlPlaneSerializedDelete(&test)
				validateControllerManagedControlPlaneUnreachableDelete(&test)
			}
			if spec.validate.nodePoolLifecycle {
				validateAdditionalNodePool(&test)
			}
			if spec.validate.nodePoolCascade {
				validateNodePoolCascadePostCreate(&test)
			}
			if spec.createVia == clientNvkectl {
				validateNvkectlPostCreate(&test)
				validateNvkectlNodePoolLifecycle(&test)
			}
			if spec.validate.tenantChecks {
				validateTenant(laneTenantTarget(&test, func() bool { return failed }), laneTenantSettings)
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

			if spec.validate.nodeAgentUpgrade {
				deleteClusterWithAgentDrain(ctx, &test, &deleteAccepted)
				deleted = true
			} else {
				deleted = test.deleteClusterAndWait(ctx, &deleteAccepted)
			}
		}, Label("lifecycle"), NodeTimeout(80*time.Minute))

		Describe("after cluster deletion", func() {
			BeforeEach(func() {
				if test.cluster != nil && terminatingTenants[test.cluster.ID] {
					Skip("the tenant's instances are Terminating and its cluster is still being deleted; nothing is left behind to assert yet")
				}
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
			defer func() {
				if agentMethodChanged && test.k8sClient != nil {
					_, _, err := nvke.SetNodeAgentUpdateMethod(ctx, test.k8sClient, previousAgentMethod)
					Expect(err).NotTo(HaveOccurred())
				}
			}()
			// Named once so the delete below and the nvkectl sweep cannot disagree
			// about when the cluster should outlive the run.
			keepCluster := test.cfg.Lifecycle.ReuseCluster != "" ||
				test.cfg.Lifecycle.SkipDelete ||
				(test.cfg.Lifecycle.PreserveOnFailure && failed)

			// Deferred: everything below reports through Gomega, which fails by
			// panicking, and a delete that does not complete would otherwise leave
			// the forward and its credential directory behind and skip the sweep.
			// Ordered so the sweep, which needs the forward, runs first.
			defer test.closeNvkectl()
			defer func() {
				if !keepCluster {
					// Catches what the switch below cannot see: a cluster nvkectl
					// created that never reached test.cluster.
					test.cleanupNvkectlCreated(ctx)
				}
			}()

			if spec.validate.brownfieldOIDC && test.k8sClient != nil {
				// Restore the controller OIDC create-default the create spec turned off, so
				// it does not leak to other table entries (the suite runs serially).
				Expect(nvke.SetOIDCEnabled(ctx, test.k8sClient, true)).To(Succeed())
			}
			// Use AfterAll so the cluster is removed even if a spec before the
			// delete spec fails (or the delete spec is skipped). It is a no-op
			// once the controller has accepted a delete for the cluster.
			switch {
			case deleted, test.cluster == nil:
			case keepCluster:
			case deleteAccepted:
				// The controller already accepted a delete for this cluster and the wait
				// ran out without it going. Asking again changes nothing, and
				// waiting again spends the suite's remaining time
				// on a cluster that is not moving -- time the site teardown
				// needs, because releasing the site is what stops its Forge
				// instance leaking. Run 36147423536 waited three times on one
				// stuck tenant, 112 minutes of a 150-minute suite, and the
				// suite timeout then killed the teardown before it released the
				// site.
				GinkgoLogr.Info("tenant delete already accepted but not complete; leaving it to the site sweep rather than waiting again",
					"cluster", test.cluster.ID)
			default:
				deleted = test.deleteClusterAndWait(ctx, &deleteAccepted)
			}
			// Sized to what this node can actually contain rather than to the
			// suite: WaitForClusterDeleted is bounded by the entry's
			// nvke.Options.Delete, which is 5 or 10 minutes everywhere above,
			// so fifteen leaves half again over the largest of them.
			//
			// It was an hour, and that is the number that matters on the
			// interrupt path. A cleanup node entered after the suite is
			// already interrupted gets its whole NodeTimeout over again, so an
			// hour here plus the site's forty-five minutes is a teardown that
			// cannot fit between -ginkgo.timeout and the go-test backstop --
			// and that backstop panics without unwinding, which leaks the
			// site's instance. See the three nested timeouts in
			// qualify-e2e.yml.
		}, NodeTimeout(15*time.Minute))
	}
}

func withNicoVMSReleaseEntries(args ...any) []any {
	for _, entry := range mustNicoVMSReleaseTableEntries() {
		args = append(args, entry)
	}
	return args
}

// specBuilder builds the create spec for the backend under test from cfg and a
// base cluster name.
type specBuilder func(cfg config.Config, name string, shape clusterShape) nvke.ClusterSpec

// testSpec is one DescribeTableSubtree entry: how to build the cluster spec
// plus the validations and cluster-operation options for that variant.
type testSpec struct {
	buildSpec specBuilder
	shape     clusterShape
	options   nvke.Options
	validate  validations
	// createVia selects the client that drives this entry's cluster lifecycle.
	createVia clusterClient
}

// clusterClient names the transport an entry drives its cluster lifecycle
// through. The zero value is the generated REST client that setupTenantTest
// wires by default, so only the alternative needs a name.
type clusterClient int

const (
	// clientNvkectl shells out to the nvkectl binary instead of using the REST
	// client.
	clientNvkectl clusterClient = iota + 1
)

type clusterShape struct {
	kubernetesVersion string
	cniPlugin         string
	// controlPlanePools describes the control plane as one node pool. The spec
	// builders send it as a cluster-level controlPlaneConfig (see
	// applyControlPlaneMode).
	controlPlanePools []nvke.NodePoolSpec
	workerPools       []nvke.NodePoolSpec
	etcdEncrypted     bool
	controlPlaneVIP   string
	// withoutControlPlaneVIP suppresses automatic VIP allocation.
	withoutControlPlaneVIP bool
	// controlPlaneConfig provisions the control plane through the cluster-level
	// control-plane controller, with no control-plane node pool. Set it instead
	// of controlPlanePools, never both: the cp-controller entries use it for
	// controller-only fields, and the builders leave a spec that already carries
	// a config unconverted. nicoForgeSpec uses it too, because a site controller runs
	// with the flag on by default and rejects control-plane pools outright.
	controlPlaneConfig *nvke.ControlPlaneConfigSpec
}

type validations struct {
	// provisioningOnly limits the lifecycle to create, node readiness, delete,
	// and the shared tenant-access cleanup check.
	provisioningOnly bool
	nodeAgentUpgrade bool
	// postReadyChecks verifies intended cluster state after the initial CAPD
	// cluster and all desired nodes are Ready.
	postReadyChecks bool
	// kubeVipRouteWithdrawal stops one CAPD kube-apiserver and verifies kube-vip
	// withdraws only that node's BGP path before restoring it.
	kubeVipRouteWithdrawal bool
	// certificateRenewal forces certificate renewal on every control-plane node
	// and verifies the cluster returns to Ready after each sequential restart.
	certificateRenewal bool
	// tenantChecks runs the validation image's tenant checks against the
	// cluster, after every other post-create check.
	tenantChecks bool
	// tpmAttestation asserts every node's SPIRE agent attested to the
	// management cluster by its TPM. Only for a backend whose machines have
	// one; the spec carries the tpm-attestation label so a lane without a TPM
	// excludes it.
	tpmAttestation bool
	// openVSwitch proves OVN node provisioning used the typed node-agent RPC
	// on every node and did not enter the legacy runbook fallback.
	openVSwitch bool
	// ovnNetworking validates NetworkPolicy enforcement on the explicit OVN
	// smoke topology.
	ovnNetworking bool
	// csrApproval rotates a Ready worker's kubelet serving certificate and
	// verifies that the tenant CSR controller approves and obtains a new one.
	csrApproval bool
	// scale exercises worker node pool scale-up and scale-down.
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
	// the controller provisions a replacement while the surviving node is preserved.
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
	// controlPlaneController asserts the control plane is provisioned by the
	// cluster-level control-plane controller with no control-plane node pool.
	controlPlaneController bool
	// controlPlaneControllerUpdate PATCHes the cluster-level controlPlaneConfig
	// desiredCount and asserts the controller-managed control plane converges on
	// the new count in the controller and the tenant API.
	controlPlaneControllerUpdate bool
	// The noVIP flags add rejection checks to single-node no-VIP rows.
	noVIPControllerScale bool
	noVIPUpgrade         bool
	// controlPlaneControllerRollingUpdate PATCHes the cluster-level
	// controlPlaneConfig with a new config profile to bump the control-plane
	// configuration revision and asserts the controller-managed control-plane
	// nodes are rolled onto new nodes at the same desired count in the controller and the
	// tenant API. Ordered after controlPlaneControllerUpdate so the control plane
	// has enough nodes to keep quorum while rolling.
	controlPlaneControllerRollingUpdate bool
	// controlPlaneControllerDelete asserts the controller-owned, serialized
	// control-plane delete: a delete that would leave fewer than three joined
	// control-plane nodes is refused at the API, and an accepted one runs one
	// teardown at a time with no control-plane join overlapping it. Ordered after
	// controlPlaneControllerRollingUpdate so the control plane is already at three
	// nodes.
	controlPlaneControllerDelete bool
	// controlPlaneNodeLabels asserts the control plane's configured node labels
	// are on every control-plane node after create and, when the entry runs it,
	// after the rolling update. The entry must set controlPlaneConfig.NodeLabels.
	controlPlaneNodeLabels bool
}

// managementCluster is the cluster the controller runs on, which a tenant test reaches
// the controller and the tenant clusters through.
type managementCluster struct {
	Kubeconfig string
	Context    string

	// TrustDomain is the SPIRE trust domain. Empty derives it from a kind
	// context name, which is all a simulator lane has to go on.
	TrustDomain string

	// Relay reaches a tenant's API server when the test process cannot route
	// to the address the tenant publishes. Only the Forge site lane sets it:
	// every other lane shares a network with its clusters, and nil leaves
	// them talking to the published address directly.
	Relay kube.EndpointRelay

	// Instances ends the tenant delete wait once the tenant's machines are
	// Terminating, rather than once they are gone. Only the Forge site lane
	// sets it, because only there does the release outlast the controller's
	// own teardown by most of an hour. Nil waits for the cluster to go.
	Instances terminationWaiter
}

// terminationWaiter is the part of the Forge provider the delete wait needs.
type terminationWaiter interface {
	WaitTerminating(ctx context.Context, ids []string) error
}

// tenantTerminating budgets the wait for Forge to take a deleted tenant's
// machines out of service. The controller deletes the CAPI Machines within a
// minute of accepting the delete, and Forge marks an instance Terminating as
// soon as it is asked to release it, so a quarter hour is far more than the
// healthy path needs.
const tenantTerminating = 15 * time.Minute

// terminatingTenants are the tenant clusters whose machines Forge has already
// confirmed Terminating. The site sweep reads it so it does not wait a second
// time on a tenant whose release Forge already owns.
var terminatingTenants = map[openapi_types.UUID]bool{}

// managementResolver names the management cluster when a tenant lane starts.
type managementResolver func() (managementCluster, error)

// configuredManagement is the management cluster the e2e config file names:
// every lane but the one that builds its own.
func configuredManagement() (managementCluster, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return managementCluster{}, fmt.Errorf("loading e2e config: %w", err)
	}
	return managementCluster{Kubeconfig: cfg.Kube.Kubeconfig, Context: cfg.Kube.Context}, nil
}

// setupTenantTest builds the per-backend wiring (clients, trust domain, logger)
// and preflights NVKE reachability over the port-forward. It does not create a
// cluster.
func setupTenantTest(ctx context.Context, mgmt managementCluster, timeouts nvke.Options) (tenantTest, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return tenantTest{}, fmt.Errorf("loading e2e config: %w", err)
	}

	restConfig, err := kube.RESTConfig(mgmt.Kubeconfig, mgmt.Context)
	if err != nil {
		return tenantTest{}, fmt.Errorf("reading kubeconfig: %w", err)
	}

	k8sClient, err := kube.RuntimeClient(restConfig)
	if err != nil {
		return tenantTest{}, fmt.Errorf("building management cluster client: %w", err)
	}

	k8sClientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return tenantTest{}, fmt.Errorf("building management cluster clientset: %w", err)
	}

	controllerClient, err := nvke.NewClient(ctx, restConfig, timeouts)
	if err != nil {
		return tenantTest{}, fmt.Errorf("building controller API client: %w", err)
	}

	trustDomain := mgmt.TrustDomain
	if trustDomain == "" {
		var tdErr error
		trustDomain, tdErr = kube.TrustDomain(mgmt.Kubeconfig, mgmt.Context)
		if tdErr != nil {
			GinkgoLogr.Info("SPIRE trust domain could not be derived from kubeconfig context; OIDC validations will skip", "error", tdErr)
		}
	}

	var tenantClientOptions []kube.TenantClientFactoryOption
	if mgmt.Relay != nil {
		tenantClientOptions = append(tenantClientOptions, kube.WithEndpointRelay(mgmt.Relay))
	}
	if os.Getenv(dsxIdentityExchangeEnv) == "true" {
		var exchangeCA corev1.ConfigMap
		if getErr := k8sClient.Get(ctx, client.ObjectKey{
			Namespace: controller.Namespace(),
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
	resp, err := controllerClient.API().GetClusterWithResponse(pingCtx, uuid.New())
	if err != nil {
		return tenantTest{}, fmt.Errorf("reaching NVKE over the port-forward: %w", err)
	}
	if resp.StatusCode() != 404 {
		return tenantTest{}, fmt.Errorf("controller preflight: want 404 for a random cluster UUID, got %s: %s", resp.Status(), resp.Body)
	}

	return tenantTest{
		log:           GinkgoLogr,
		cfg:           cfg,
		mgmt:          mgmt,
		k8sClient:     k8sClient,
		tenantClients: tenantClients,
		nvke:          controllerClient,
		lifecycle:     controllerClient,
		timeouts:      timeouts,
		trustDomain:   trustDomain,
		k8sClientset:  k8sClientset,
	}, nil
}

// initCluster reuses an existing cluster when lifecycle.reuseCluster is
// set, otherwise creates one for the backend under test and waits for it to be
// provisioned. It fails the spec on any error.
func (t *tenantTest) initCluster(ctx context.Context, b testSpec) {
	GinkgoHelper()
	t.spec = b.buildSpec(t.cfg, clusterNamePrefix, b.shape)
	Expect(validateCAPDFailureDomainReuse(t.spec, b.validate.controlPlaneController, t.cfg.Lifecycle.ReuseCluster)).To(Succeed())
	if b.validate.controlPlaneController && t.spec.Backend == nvke.BackendCAPD {
		Expect(t.spec.CAPDFailureDomains).NotTo(BeEmpty(), "CAPD control-plane-controller rows must advertise failure domains")
	}
	t.spec.Name = fmt.Sprintf("%s-%s", t.spec.Name, t.spec.Backend) // CreateCluster appends a timestamp

	if t.cfg.Lifecycle.ReuseCluster != "" {
		t.cluster = expectSucceed(t.lifecycle.GetByName(ctx, t.cfg.Lifecycle.ReuseCluster))
		return
	}

	// A backend with no preferred VIP wants none: the controller reads an empty one as
	// "do not configure kube-vip", which is how a real site runs.
	if preferred := preferredControlPlaneVIP(t.spec.Backend); preferred != "" && t.spec.ControlPlane.ControlPlaneVIP == "" && !b.shape.withoutControlPlaneVIP {
		vip, err := t.availableControlPlaneVIP(ctx, preferred)
		Expect(err).NotTo(HaveOccurred(), "should select an unused control-plane VIP")
		t.spec.ControlPlane.ControlPlaneVIP = vip
	}
	t.log.Info("Using control-plane VIP (empty means none configured)", "controlPlaneVIP", t.spec.ControlPlane.ControlPlaneVIP)

	By(fmt.Sprintf("Creating the %s tenant cluster", t.spec.Backend))
	cluster, err := t.lifecycle.CreateCluster(ctx, t.spec)
	Expect(err).NotTo(HaveOccurred(), "should create the cluster")
	t.log.Info("Created cluster", "name", cluster.Name, "id", cluster.ID)

	By("Verifying the tenant cluster appears in the cluster list")
	listedCluster, err := t.lifecycle.GetByName(ctx, cluster.Name)
	Expect(err).NotTo(HaveOccurred(), "should list the created cluster by name")
	Expect(listedCluster.ID).To(Equal(cluster.ID), "listed cluster should match the created cluster")

	By("Waiting for the tenant cluster to be provisioned")
	start := time.Now()
	// The id, not cluster.ID: WaitForClusterReady returns a nil cluster when
	// it expires, and assigning that over the one just created leaves the
	// collector below dereferencing nil. A timed-out provision then panicked
	// instead of reporting, which cost the diagnostics the wait exists to
	// produce.
	id := cluster.ID
	ready, err := t.lifecycle.WaitForClusterReady(ctx, id)
	if err != nil {
		// Here rather than in a post-create spec: when this wait expires the
		// specs that would have reported anything never run, which is how a
		// failed provision produced a log naming no node at all. Detached
		// because the context that just expired is the one that failed.
		t.collectDiagnostics(context.WithoutCancel(ctx), id)
	}
	Expect(err).NotTo(HaveOccurred(), "should provision the cluster")
	cluster = ready
	t.log.Info("Cluster provisioned", "name", cluster.Name, "duration", time.Since(start).Round(time.Second).String())

	t.cluster = cluster
}

// tenantDiagnostics is written beside the run log, in the directory the suite
// runs from, so CI picks it up with the rest of the run's record.
const tenantDiagnostics = "tenant-diagnostics.json"

// collectDiagnostics writes the cluster, its node pools and every node it has,
// conditions included, for a run that is already failing.
//
// Best effort and never fails the spec: the failure that prompted it is the
// finding, and losing the diagnostics must not replace it with a second, less
// useful one.
func (t *tenantTest) collectDiagnostics(ctx context.Context, clusterID openapi_types.UUID) {
	// Named in the log as well as the artifact. Nothing else in the lane
	// prints a tenant node name: the only path that does is the readiness
	// assertion, which a provision timeout skips, so run 37394133486 produced
	// a 1h51m log naming no node and the instance had to be found by querying
	// Forge by hand.
	if nodes, err := t.nvke.ListClusterNodes(ctx, clusterID); err != nil {
		t.log.Info("could not list the cluster's nodes", "error", err.Error())
	} else {
		for _, node := range nodes {
			fields := []any{"node", node.Name, "controlPlane", node.ControlPlane}
			if node.Nico != nil {
				fields = append(fields, "instanceID", node.Nico.InstanceID, "machineID", node.Nico.MachineID)
			}
			t.log.Info("tenant node", fields...)
		}
	}

	report := t.nvke.CollectNodeDiagnostics(ctx, clusterID)
	if err := os.WriteFile(tenantDiagnostics, report, 0o600); err != nil {
		t.log.Error(err, "could not write the tenant diagnostics", "path", tenantDiagnostics)
		return
	}
	t.log.Info("collected the tenant's diagnostics", "path", tenantDiagnostics, "bytes", len(report))
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

// deleteClusterAndWait deletes the tenant cluster and blocks until the controller reports
// it gone (404). Shared by the delete spec and the AfterAll cleanup. accepted
// is set once the controller takes the delete, so a failure in the wait leaves it true and
// a failure in the request leaves it as it was.
//
// With mgmt.Instances set it returns once Forge reports the tenant's machines
// Terminating instead, and reports false: the cluster is still being deleted,
// so the post-deletion specs, which read what a finished delete leaves
// behind, have nothing to assert yet. The instance IDs are read before the
// delete is sent, because the controller drops its node records within
// seconds of taking it.
func (t *tenantTest) deleteClusterAndWait(ctx context.Context, accepted *bool) bool {
	GinkgoHelper()
	var instances []string
	if t.mgmt.Instances != nil {
		instances = t.tenantInstances(ctx)
	}
	By("Deleting the tenant cluster")
	Expect(t.lifecycle.DeleteCluster(ctx, t.cluster.ID)).To(Succeed())
	*accepted = true
	if len(instances) > 0 {
		By("Waiting for Forge to report the tenant's instances Terminating")
		waitCtx, cancel := context.WithTimeout(ctx, tenantTerminating)
		defer cancel()
		Expect(t.mgmt.Instances.WaitTerminating(waitCtx, instances)).To(Succeed())
		terminatingTenants[t.cluster.ID] = true
		t.log.Info("tenant instances are Terminating; Forge releases them from here", "cluster", t.cluster.ID, "instances", instances)
		return false
	}
	By("Waiting for the tenant cluster to be deleted")
	Expect(t.lifecycle.WaitForClusterDeleted(ctx, t.cluster.ID)).To(Succeed())
	return true
}

// tenantInstances is the Forge instance behind every node of the tenant, or
// nil when any node does not name one. Nil falls back to waiting for the
// cluster itself, which is slower but cannot pass while a machine is still
// in service.
func (t *tenantTest) tenantInstances(ctx context.Context) []string {
	nodes, err := t.nvke.ListClusterNodes(ctx, t.cluster.ID)
	if err != nil {
		t.log.Info("could not list the tenant's nodes; waiting for the cluster to be deleted instead", "error", err.Error())
		return nil
	}
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node.Nico == nil || node.Nico.InstanceID == "" {
			t.log.Info("a tenant node names no Forge instance; waiting for the cluster to be deleted instead", "node", node.Name)
			return nil
		}
		ids = append(ids, node.Nico.InstanceID)
	}
	return ids
}

// Cluster shape is a property of the test, not the environment, so it lives in
// code rather than the e2e config file. Per-backend waiter timeouts live on the
// testSpec entries above.
const (
	clusterNamePrefix        = "e2e" // CreateCluster appends the backend and a timestamp
	defaultKubernetesVersion = "1.34.2"
	CNIPluginOVN             = "ovn-kubernetes"
	capdControlPlaneVIP      = "172.31.1.1"

	nicoVMSMultiPodCIDR        = "10.250.0.0/23,10.252.0.0/22"
	nicoVMSMultiPodServiceCIDR = "10.130.0.0/16"
)

func preferredControlPlaneVIP(backend nvke.Backend) string {
	switch backend {
	case nvke.BackendCAPD:
		return capdControlPlaneVIP
	case nvke.BackendNicoContainers:
		return nicoControlPlaneVIP
	case nvke.BackendNicoVMS:
		return nicoVMSControlPlaneVIP
	case nvke.BackendNicoForge:
		// None. A real site owns its address space, so there is no private
		// range to pick an unused address out of, and the controller reads an empty VIP
		// as "do not configure kube-vip". initCluster skips the search when
		// the preference is empty.
		return ""
	default:
		panic(fmt.Sprintf("no preferred control-plane VIP for backend %q", backend))
	}
}

func (t *tenantTest) availableControlPlaneVIP(ctx context.Context, preferred string) (string, error) {
	clusters, err := t.nvke.ListClusters(ctx)
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

// applyControlPlaneMode sends spec's control-plane pool as the equivalent
// cluster-level controlPlaneConfig, the only shape the controller provisions. A spec that
// already carries a controlPlaneConfig is left as it is.
func applyControlPlaneMode(spec nvke.ClusterSpec) nvke.ClusterSpec {
	if spec.ControlPlane.Config != nil || len(spec.ControlPlane.Pools) == 0 {
		return spec
	}
	spec.ControlPlane.Config = controlPlaneConfigFromPools(spec.ControlPlane.Pools)
	spec.ControlPlane.Pools = nil
	return spec
}

// controlPlaneConfigFromPools converts a control plane described as one
// defaulted node pool into the cluster-level config for the same nodes. The
// pool's kubeReservedMemory is not carried: the create API has no such field,
// so the controller applies its default (see nicoVMSSpec).
func controlPlaneConfigFromPools(pools []nvke.NodePoolSpec) *nvke.ControlPlaneConfigSpec {
	if len(pools) != 1 {
		panic(fmt.Sprintf("a cluster-level control plane has one shape; got %d control-plane pools", len(pools)))
	}
	pool := pools[0]
	return &nvke.ControlPlaneConfigSpec{
		DesiredCount:         pool.DesiredNodeCount,
		InstanceTypeID:       pool.InstanceTypeID,
		Release:              pool.Release,
		ConfigProfile:        pool.ConfigProfile,
		VpcPrefixIDs:         pool.VpcPrefixIDs,
		SubnetIDs:            pool.SubnetIDs,
		SSHKeyGroupIDs:       pool.SSHKeyGroupIDs,
		SystemReservedMemory: pool.SystemReservedMemory,
	}
}

// capdSpec builds a CAPD topology with the requested cluster shape.
func capdSpec(cfg config.Config, name string, shape clusterShape) nvke.ClusterSpec {
	return applyControlPlaneMode(nvke.ClusterSpec{
		Name:              name,
		Backend:           nvke.BackendCAPD,
		KubernetesVersion: cmp.Or(shape.kubernetesVersion, defaultKubernetesVersion),
		CNIPlugin:         cmp.Or(shape.cniPlugin, CNIPluginOVN),
		CAPDNodeImage:     cfg.Cluster.CAPDNodeImage,
		SiteID:            uuid.Nil.String(),
		ControlPlane: nvke.ControlPlane{
			Pools:           capdNodePoolSpecs(shape.controlPlanePools, "control-plane", "cp"),
			EtcdEncrypted:   shape.etcdEncrypted,
			ControlPlaneVIP: shape.controlPlaneVIP,
			Config:          shape.controlPlaneConfig,
		},
		Workers: capdNodePoolSpecs(shape.workerPools, "worker", "w"),
	})
}

// capdControlPlaneControllerSpec builds a CAPD topology whose control plane is
// provisioned by the cluster-level control-plane controller: no control-plane
// node pool, a cluster-level control plane config instead.
func capdControlPlaneControllerSpec(cfg config.Config, name string, shape clusterShape) nvke.ClusterSpec {
	spec := capdSpec(cfg, name, shape)
	spec.ControlPlane.Pools = nil
	spec.ControlPlane.Config = shape.controlPlaneConfig
	if shape.controlPlaneConfig != nil {
		cp := *shape.controlPlaneConfig
		cp.SystemReservedMemory = cmp.Or(cp.SystemReservedMemory, "1Gi")
		spec.ControlPlane.Config = &cp
	}
	// CAPD copies these simulated domains to CAPI Cluster status. The third
	// domain proves that controlPlane: false is excluded from placement.
	spec.CAPDFailureDomains = []nvke.CAPDFailureDomain{
		{Name: "fd-a", ControlPlane: true},
		{Name: "fd-b", ControlPlane: true},
		{Name: "fd-worker", ControlPlane: false},
	}
	return spec
}

// capdNoEligibleControlPlaneDomainSpec advertises a worker-only domain so the
// control-plane controller must leave Machine failureDomain unset.
func capdNoEligibleControlPlaneDomainSpec(cfg config.Config, name string, shape clusterShape) nvke.ClusterSpec {
	spec := capdControlPlaneControllerSpec(cfg, name, shape)
	spec.CAPDFailureDomains = []nvke.CAPDFailureDomain{{Name: "fd-worker", ControlPlane: false}}
	return spec
}

// nicoControlPlaneControllerSpec builds a NICo container topology whose control
// plane is shape.controlPlaneConfig, filling in the mock's seed identifiers the
// entry leaves unset.
func nicoControlPlaneControllerSpec(cfg config.Config, name string, shape clusterShape) nvke.ClusterSpec {
	spec := nicoSpec(cfg, name, shape)
	if spec.ControlPlane.Config != nil {
		// The same identifiers nicoNodePoolSpecs applies to a pool. Without them
		// the machine template has no instance type or subnet and the controller rejects the
		// create.
		cp := *spec.ControlPlane.Config
		cp.InstanceTypeID = cmp.Or(cp.InstanceTypeID, nicoInstanceTypeID)
		cp.SubnetIDs = defaultStrings(cp.SubnetIDs, nicoSubnetID)
		cp.SSHKeyGroupIDs = defaultStrings(cp.SSHKeyGroupIDs, nicoSSHKeyGroupID)
		cp.SystemReservedMemory = cmp.Or(cp.SystemReservedMemory, "1Gi")
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

func TestUnit_ApplyControlPlaneMode(t *testing.T) {
	poolShape := clusterShape{
		controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 3, InstanceTypeID: nicoVMSLargeInstanceID}},
		workerPools:       []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
	}
	configShape := clusterShape{
		controlPlaneConfig: &nvke.ControlPlaneConfigSpec{DesiredCount: 1},
		workerPools:        []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
	}

	tests := []struct {
		name       string
		build      specBuilder
		shape      clusterShape
		wantConfig *nvke.ControlPlaneConfigSpec
	}{
		{
			name:  "a nico-vms pool is sent as config",
			build: nicoVMSSpec,
			shape: poolShape,
			wantConfig: &nvke.ControlPlaneConfigSpec{
				DesiredCount:         3,
				InstanceTypeID:       nicoVMSLargeInstanceID,
				Release:              nicoVMSRelease,
				ConfigProfile:        nicoVMSCPConfigProfile,
				VpcPrefixIDs:         []string{nicoVMSVpcPrefixID},
				SSHKeyGroupIDs:       []string{nicoVMSSSHKeyGroupID},
				SystemReservedMemory: "250Mi",
			},
		},
		{
			name:       "a CAPD pool is sent as config",
			build:      capdSpec,
			shape:      poolShape,
			wantConfig: &nvke.ControlPlaneConfigSpec{DesiredCount: 3, SystemReservedMemory: "1Gi"},
		},
		{
			name:  "a NICo container pool gets the seed identifiers",
			build: nicoSpec,
			shape: clusterShape{controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 1}}},
			wantConfig: &nvke.ControlPlaneConfigSpec{
				DesiredCount:         1,
				InstanceTypeID:       nicoInstanceTypeID,
				SubnetIDs:            []string{nicoSubnetID},
				SSHKeyGroupIDs:       []string{nicoSSHKeyGroupID},
				SystemReservedMemory: "1Gi",
			},
		},
		{
			name:       "an explicit config is left as it is",
			build:      capdSpec,
			shape:      configShape,
			wantConfig: configShape.controlPlaneConfig,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := tt.build(config.Config{}, "unit", tt.shape)

			require.Empty(t, spec.ControlPlane.Pools)
			require.Equal(t, tt.wantConfig, spec.ControlPlane.Config)
			require.Len(t, spec.Workers, len(tt.shape.workerPools))
		})
	}
}

// controllerDefaultKubeReservedMemory is what the controller reserves for the kubelet when a
// node's config sets nothing (controller/internal/kubeadmconfig/render.go). A
// controller-managed control plane always gets it, because the create API's
// controlPlaneConfig has no kubeReservedMemory.
const controllerDefaultKubeReservedMemory = "2Gi"

func TestUnit_NicoVMSControlPlaneFitsKubeletReservation(t *testing.T) {
	fits := func(t *testing.T, instanceTypeID, systemReserved string) bool {
		t.Helper()
		memory, ok := nicoVMSInstanceMemory[instanceTypeID]
		if !ok {
			t.Fatalf("instance type %q has no memory in nicoVMSInstanceMemory", instanceTypeID)
		}
		capacity := resource.MustParse(memory)
		reserved := resource.MustParse(controllerDefaultKubeReservedMemory)
		reserved.Add(resource.MustParse(systemReserved))
		return reserved.Cmp(capacity) < 0
	}

	shapes := map[string]clusterShape{
		"control plane with no instance type": {controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 1}}},
		"OS profile matrix row":               nicoVMSReleaseTestSpec(nicoVMSReleaseEntry{name: "unit-profile"}, 0).shape,
	}
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			cp := nicoVMSSpec(config.Config{}, "unit", shape).ControlPlane.Config
			if cp == nil {
				t.Fatal("a nico-vms spec carries no controlPlaneConfig")
			}
			if !fits(t, cp.InstanceTypeID, cp.SystemReservedMemory) {
				t.Fatalf("instance %s cannot hold %s kube-reserved plus %s system-reserved",
					cp.InstanceTypeID, controllerDefaultKubeReservedMemory, cp.SystemReservedMemory)
			}
		})
	}

	t.Run("the default 2 GiB instance cannot hold a controller-managed control plane", func(t *testing.T) {
		if fits(t, nicoVMSInstanceTypeID, "250Mi") {
			t.Fatalf("instance %s now fits; the large-instance default for control planes may be unnecessary", nicoVMSInstanceTypeID)
		}
	})
}

// builtControlPlaneInstanceType returns the instance type spec's control plane
// runs on, in either control-plane shape.
func builtControlPlaneInstanceType(spec nvke.ClusterSpec) string {
	if spec.ControlPlane.Config != nil {
		return spec.ControlPlane.Config.InstanceTypeID
	}
	if len(spec.ControlPlane.Pools) > 0 {
		return spec.ControlPlane.Pools[0].InstanceTypeID
	}
	return ""
}

func TestUnit_ForgeControlPlaneConfig(t *testing.T) {
	cpu := forge.InstanceType{Name: "cpu", ID: uuid.NewString()}
	subnet, prefix, keys := uuid.NewString(), uuid.NewString(), uuid.NewString()

	tests := map[string]struct {
		site                      forge.Site
		wantSubnets, wantPrefixes []string
	}{
		// site-1: attaches by subnet.
		"subnet site": {
			site:        forge.Site{ID: uuid.NewString(), VpcID: uuid.NewString(), SubnetID: subnet, SSHKeyGroupID: keys},
			wantSubnets: []string{subnet},
		},
		// site-2: attaches by VPC prefix and has no subnet, which must stay empty
		// rather than become [""].
		"VPC prefix site": {
			site:         forge.Site{ID: uuid.NewString(), VpcID: uuid.NewString(), VpcPrefixID: prefix, SSHKeyGroupID: keys},
			wantPrefixes: []string{prefix},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := forgeControlPlaneConfig(nil, tc.site, cpu); got != nil {
				t.Fatalf("config for a pool-based entry = %+v, want nil", got)
			}

			got := forgeControlPlaneConfig(&nvke.ControlPlaneConfigSpec{DesiredCount: 1}, tc.site, cpu)
			if got.InstanceTypeID != cpu.ID || got.SystemReservedMemory != "16Gi" || !slices.Equal(got.SSHKeyGroupIDs, []string{keys}) {
				t.Errorf("defaults = %+v, want the control plane type, 16Gi and the site's key group", got)
			}
			if !slices.Equal(got.SubnetIDs, tc.wantSubnets) || !slices.Equal(got.VpcPrefixIDs, tc.wantPrefixes) {
				t.Errorf("subnets %q prefixes %q, want %q and %q", got.SubnetIDs, got.VpcPrefixIDs, tc.wantSubnets, tc.wantPrefixes)
			}

			// The request the site controller is sent: the control plane rides the
			// cluster-level config, and no pool claims it.
			payloads := nvke.NodePayloads{IPXEScript: "#!ipxe", PreKubeadmCommands: []string{"true"}}
			req, err := nvke.BuildCreateRequest(nvke.ClusterSpec{
				Name:         "forge-test",
				Backend:      nvke.BackendNicoForge,
				SiteID:       tc.site.ID,
				VpcID:        tc.site.VpcID,
				NodePayloads: &nvke.RolePayloads{ControlPlane: payloads, Worker: payloads},
				ControlPlane: nvke.ControlPlane{Config: got},
				Workers:      forgeNodePoolSpecs([]nvke.NodePoolSpec{{DesiredNodeCount: 1}}, "worker", "w", tc.site, cpu),
			})
			if err != nil {
				t.Fatalf("building the create request: %v", err)
			}
			if req.ControlPlaneConfig == nil {
				t.Fatal("request has no cluster-level control plane")
			}
			for _, pool := range *req.NodePools {
				if pool.ControlPlane {
					t.Errorf("pool %q is a control-plane pool, which the site controller rejects", pool.Name)
				}
			}
		})
	}
}

func capdNodePoolSpecs(pools []nvke.NodePoolSpec, defaultName, defaultNodeNamePrefix string) []nvke.NodePoolSpec {
	out := make([]nvke.NodePoolSpec, 0, len(pools))
	for _, pool := range pools {
		out = append(out, capdNodePoolSpec(pool, defaultName, defaultNodeNamePrefix))
	}
	return out
}

func capdNodePoolSpec(pool nvke.NodePoolSpec, defaultName, defaultNodeNamePrefix string) nvke.NodePoolSpec {
	return nvke.NodePoolSpec{
		Name:                 cmp.Or(pool.Name, defaultName),
		NodeNamePrefix:       cmp.Or(pool.NodeNamePrefix, defaultNodeNamePrefix),
		DesiredNodeCount:     pool.DesiredNodeCount,
		SystemReservedMemory: cmp.Or(pool.SystemReservedMemory, "1Gi"),
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
// containers. Like CAPD, it uses the controller's nil-UUID CAPI site; the Nico site and
// VPC live inside the infrastructure manifests.
func nicoSpec(_ config.Config, name string, shape clusterShape) nvke.ClusterSpec {
	return applyControlPlaneMode(nvke.ClusterSpec{
		Name:              name,
		Backend:           nvke.BackendNicoContainers,
		KubernetesVersion: cmp.Or(shape.kubernetesVersion, defaultKubernetesVersion),
		CNIPlugin:         cmp.Or(shape.cniPlugin, CNIPluginOVN),
		SiteID:            nicoSiteID,
		VpcID:             nicoVpcID,
		ControlPlane: nvke.ControlPlane{
			Pools:           nicoNodePoolSpecs(shape.controlPlanePools, "control-plane", "cp"),
			EtcdEncrypted:   shape.etcdEncrypted,
			ControlPlaneVIP: shape.controlPlaneVIP,
			Config:          shape.controlPlaneConfig,
		},
		Workers: nicoNodePoolSpecs(shape.workerPools, "worker", "w"),
	})
}

// A Forge tenant takes its Kubernetes and CNI versions from -e2e.versions, both
// required. There is no SPIRE pin: the OS image owns its spire-agent, so the
// lane installs none.

// nicoForgeSpec builds a CAPNico topology on real Forge. The manifests are the
// ones nicoSpec already builds, since CAPNico reconciles both, so what this
// adds is identifiers of infrastructure that exists and the two payloads a
// machine that really boots needs.
//
// It fails the spec rather than returning an error, like every other builder
// in this table. Each thing it can fail on is an input the run was supposed to
// supply, so naming which one is the useful part.
func nicoForgeSpec(_ config.Config, name string, shape clusterShape) nvke.ClusterSpec {
	GinkgoHelper()

	env, err := runEnvironment()
	Expect(err).NotTo(HaveOccurred(), "should load the environment named by -e2e.environment")
	forgeEnv, err := forge.Decode(env.Infrastructure)
	Expect(err).NotTo(HaveOccurred(), "should read it as a Forge declaration")
	capabilities, err := runCapabilities()
	Expect(err).NotTo(HaveOccurred(), "should parse -e2e.capabilities")
	// The same resolution the site lane made, so a tenant lands on the site
	// hosting it rather than on whichever other site happens to offer a
	// matching shape.
	//
	// The control plane takes the CPU shape and the workers take the GPU one,
	// so a GB200 tenant is arm64 workers behind an amd64 control plane and
	// each role boots the image built for its own machine.
	//
	// Which shape a control plane should take is not really fixed like this:
	// it depends on what is scarce at the site, and a run ought to be able to
	// choose. That choice is not built yet, so this is the one configuration
	// the lane offers. A pool that names its own InstanceTypeID still
	// overrides it, though its payloads follow the role rather than the pool,
	// so an override that changes architecture would boot the wrong image.
	selection, err := forgeEnv.Resolve(capabilities)
	Expect(err).NotTo(HaveOccurred(), "should resolve a site offering the requested shapes")
	site := selection.Site
	controlPlaneType, workerType := selection.CPU, selection.WorkerType()

	versions, err := requireVersions(versionKube, versionCNIOVN)
	Expect(err).NotTo(HaveOccurred(), "should name the Kubernetes and CNI versions under qualification")
	kubernetesVersion := versions[versionKube]

	payloads, err := forgeRolePayloads(roleArchs{
		ControlPlane: controlPlaneType.Arch(),
		Worker:       workerType.Arch(),
	}, kubernetesVersion, versions[versionCNIOVN])
	Expect(err).NotTo(HaveOccurred(), "should render the Forge node payloads")

	return nvke.ClusterSpec{
		Name:              name,
		Backend:           nvke.BackendNicoForge,
		KubernetesVersion: kubernetesVersion,
		// OVN, matching the nvke-cni-ovn-images package the pre-kubeadm
		// commands install. Calico here would install one CNI and run another.
		CNIPlugin: cmp.Or(shape.cniPlugin, CNIPluginOVN),
		// The same stream those commands installed. Left unset, the controller
		// would fill in its own default and the agent would check for the
		// stream a node image bakes, which is not what this lane installed.
		CNIVersion:   versions[versionCNIOVN],
		OrgID:        forgeEnv.Org.ID,
		SiteID:       site.ID,
		VpcID:        site.VpcID,
		NodePayloads: payloads,
		ControlPlane: nvke.ControlPlane{
			Pools:         forgeNodePoolSpecs(shape.controlPlanePools, "control-plane", "cp", site, controlPlaneType),
			Config:        forgeControlPlaneConfig(shape.controlPlaneConfig, site, controlPlaneType),
			EtcdEncrypted: shape.etcdEncrypted,
			// Empty unless an entry asks for one, which the controller reads as "do not
			// configure kube-vip". The Forge tenants were created without one,
			// and so is every Forge create in this repository. The simulator
			// backends advertise theirs through a BGP speaker the runner
			// hosts, and a real site has no equivalent to borrow from.
			ControlPlaneVIP: shape.controlPlaneVIP,
		},
		Workers: forgeNodePoolSpecs(shape.workerPools, "worker", "w", site, workerType),
	}
}

// forgeControlPlaneConfig fills a cluster-level control-plane config from the
// environment, the same defaults forgeNodePoolSpecs applies to a pool: a
// control-plane node is the same machine as a pool node, so it is chosen by
// the same capability match and attaches the way the site's fabric does.
// Returns nil for an entry that asks for a control-plane node pool instead,
// which the controller reads as "no cluster-level control plane".
func forgeControlPlaneConfig(
	cfg *nvke.ControlPlaneConfigSpec,
	site forge.Site,
	instanceType forge.InstanceType,
) *nvke.ControlPlaneConfigSpec {
	if cfg == nil {
		return nil
	}
	out := *cfg
	out.SystemReservedMemory = cmp.Or(out.SystemReservedMemory, "16Gi")
	out.InstanceTypeID = cmp.Or(out.InstanceTypeID, instanceType.ID)
	// optionalStrings, as for a pool: at a site that attaches by prefix the
	// subnet is unset, and a plain default would send Forge [""].
	out.SubnetIDs = optionalStrings(out.SubnetIDs, site.SubnetID)
	out.VpcPrefixIDs = optionalStrings(out.VpcPrefixIDs, site.VpcPrefixID)
	out.SSHKeyGroupIDs = defaultStrings(out.SSHKeyGroupIDs, site.SSHKeyGroupID)
	return &out
}

// roleArchs is the architecture each role's machines are, named rather than
// ordered.
//
// A struct because the two are both strings and differ only on a
// mixed-architecture cluster, so passing them positionally makes a
// transposition compile, pass every test, and read correctly. What it would
// actually do is boot each role on the other's image, and a worker that boots
// the wrong image never reports, which is indistinguishable from the hardware
// failing to provision.
type roleArchs struct {
	ControlPlane string
	Worker       string
}

// forgeRolePayloads renders one payload set per role. When both roles are the
// same architecture the work is done once and shared, which is the ordinary
// single-architecture cluster.
func forgeRolePayloads(archs roleArchs, kubernetesVersion, cniVersion string) (*nvke.RolePayloads, error) {
	build := func(arch string) (nvke.NodePayloads, error) {
		bootLoaderURL, imageURL, err := requireNodeArtifacts(arch)
		if err != nil {
			return nvke.NodePayloads{}, err
		}
		return nvke.BuildForgeNodePayloads(nvke.NodeInputs{
			BootLoaderURL:     bootLoaderURL,
			ImageURL:          imageURL,
			Arch:              arch,
			KubernetesVersion: kubernetesVersion,
			CNIVersion:        cniVersion,
		})
	}

	controlPlane, err := build(archs.ControlPlane)
	if err != nil {
		return nil, fmt.Errorf("control plane (%s): %w", archs.ControlPlane, err)
	}
	worker := controlPlane
	if archs.Worker != archs.ControlPlane {
		if worker, err = build(archs.Worker); err != nil {
			return nil, fmt.Errorf("workers (%s): %w", archs.Worker, err)
		}
	}
	return &nvke.RolePayloads{ControlPlane: controlPlane, Worker: worker}, nil
}

// forgeNodePoolSpecs fills each pool from the environment. A pool naming its
// own instance type keeps it, which is how a table entry asks for a shape the
// capability match would not have chosen.
func forgeNodePoolSpecs(
	pools []nvke.NodePoolSpec,
	defaultName, defaultNodeNamePrefix string,
	site forge.Site,
	instanceType forge.InstanceType,
) []nvke.NodePoolSpec {
	out := make([]nvke.NodePoolSpec, 0, len(pools))
	for _, pool := range pools {
		out = append(out, nvke.NodePoolSpec{
			Name:                 cmp.Or(pool.Name, defaultName),
			NodeNamePrefix:       cmp.Or(pool.NodeNamePrefix, defaultNodeNamePrefix),
			DesiredNodeCount:     pool.DesiredNodeCount,
			SystemReservedMemory: cmp.Or(pool.SystemReservedMemory, "16Gi"),
			InstanceTypeID:       cmp.Or(pool.InstanceTypeID, instanceType.ID),
			// Whichever of the two the site's fabric uses. The other has
			// to stay an empty slice rather than a slice holding an empty
			// string, which is what a plain default would produce and what
			// Forge would reject as an unknown identifier.
			SubnetIDs:      optionalStrings(pool.SubnetIDs, site.SubnetID),
			VpcPrefixIDs:   optionalStrings(pool.VpcPrefixIDs, site.VpcPrefixID),
			SSHKeyGroupIDs: defaultStrings(pool.SSHKeyGroupIDs, site.SSHKeyGroupID),
		})
	}
	return out
}

func nicoNodePoolSpecs(pools []nvke.NodePoolSpec, defaultName, defaultNodeNamePrefix string) []nvke.NodePoolSpec {
	out := make([]nvke.NodePoolSpec, 0, len(pools))
	for _, pool := range pools {
		out = append(out, nvke.NodePoolSpec{
			Name:                 cmp.Or(pool.Name, defaultName),
			NodeNamePrefix:       cmp.Or(pool.NodeNamePrefix, defaultNodeNamePrefix),
			DesiredNodeCount:     pool.DesiredNodeCount,
			SystemReservedMemory: cmp.Or(pool.SystemReservedMemory, "1Gi"),
			InstanceTypeID:       cmp.Or(pool.InstanceTypeID, nicoInstanceTypeID),
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
	nicoVMSCPConfigProfile = "nico-vms-cp"
	nicoVMSWConfigProfile  = "nico-vms-worker"
	nicoVMSControlPlaneVIP = "172.31.0.10"
)

// nicoVMSInstanceMemory is the memory of the nico-vms instance types the specs
// use, mirroring memory_gib in the same seed.
var nicoVMSInstanceMemory = map[string]string{
	nicoVMSInstanceTypeID:  "2Gi",
	nicoVMSLargeInstanceID: "6Gi",
}

// nicoVMSRelease is the profile the nico-vms specs boot. nico-sim takes the
// guest architecture from the host it runs on, never from the profile, so the
// profile has to match the runner. hack/ci/nico-vms-e2e.sh builds the same name.
var nicoVMSRelease = "nvke-nico-vms-24.04-" + runtime.GOARCH + "-fde"

// nicoVMSSpec builds a nico-vms topology using the local emulator's seed
// identifiers. kube-vip is left enabled (the nico-vms BGP speaker advertises
// the control-plane VIP), unlike the CAPD topology.
func nicoVMSSpec(_ config.Config, name string, shape clusterShape) nvke.ClusterSpec {
	return applyControlPlaneMode(nvke.ClusterSpec{
		Name:              name,
		Backend:           nvke.BackendNicoVMS,
		KubernetesVersion: cmp.Or(shape.kubernetesVersion, defaultKubernetesVersion),
		// OVN-Kubernetes unless the entry pins a CNI or NKE_E2E_NICO_VMS_CNI overrides it.
		CNIPlugin: cmp.Or(shape.cniPlugin, os.Getenv("NKE_E2E_NICO_VMS_CNI"), CNIPluginOVN),
		OrgID:     nicoVMSOrgID,
		SiteID:    nicoVMSSiteID,
		VpcID:     nicoVMSVpcID,
		ControlPlane: nvke.ControlPlane{
			// Control planes default to the large instance. The controller reserves its default
			// 2Gi for the kubelet on them, since controlPlaneConfig cannot carry
			// the pool's 250Mi, and that does not fit in 2 GiB.
			Pools: nicoVMSNodePoolSpecs(
				withDefaultInstanceType(shape.controlPlanePools, nicoVMSLargeInstanceID),
				"control-plane", "cp", nicoVMSRelease, nicoVMSCPConfigProfile,
			),
			ControlPlaneVIP: shape.controlPlaneVIP,
			EtcdEncrypted:   shape.etcdEncrypted,
			Config:          shape.controlPlaneConfig,
		},
		Workers: nicoVMSNodePoolSpecs(shape.workerPools, "worker", "w", nicoVMSRelease, nicoVMSWConfigProfile),
	})
}

// withDefaultInstanceType returns a copy of pools with instanceTypeID set on
// each pool that leaves it unset.
func withDefaultInstanceType(pools []nvke.NodePoolSpec, instanceTypeID string) []nvke.NodePoolSpec {
	out := make([]nvke.NodePoolSpec, len(pools))
	for i, pool := range pools {
		pool.InstanceTypeID = cmp.Or(pool.InstanceTypeID, instanceTypeID)
		out[i] = pool
	}
	return out
}

func nicoVMSNodePoolSpecs(pools []nvke.NodePoolSpec, defaultName, defaultNodeNamePrefix, defaultRelease, defaultConfigProfile string) []nvke.NodePoolSpec {
	out := make([]nvke.NodePoolSpec, 0, len(pools))
	for _, pool := range pools {
		out = append(out, nicoVMSNodePoolSpec(pool, defaultName, defaultNodeNamePrefix, defaultRelease, defaultConfigProfile))
	}
	return out
}

func nicoVMSNodePoolSpec(pool nvke.NodePoolSpec, defaultName, defaultNodeNamePrefix, defaultRelease, defaultConfigProfile string) nvke.NodePoolSpec {
	return nvke.NodePoolSpec{
		Name:                  cmp.Or(pool.Name, defaultName),
		NodeNamePrefix:        cmp.Or(pool.NodeNamePrefix, defaultNodeNamePrefix),
		DesiredNodeCount:      pool.DesiredNodeCount,
		SystemReservedMemory:  cmp.Or(pool.SystemReservedMemory, "250Mi"),
		KubeReservedMemory:    cmp.Or(pool.KubeReservedMemory, "250Mi"),
		InstanceTypeID:        cmp.Or(pool.InstanceTypeID, nicoVMSInstanceTypeID),
		VpcPrefixIDs:          defaultStrings(pool.VpcPrefixIDs, nicoVMSVpcPrefixID),
		SSHKeyGroupIDs:        defaultStrings(pool.SSHKeyGroupIDs, nicoVMSSSHKeyGroupID),
		Release:               cmp.Or(pool.Release, defaultRelease),
		ConfigProfile:         cmp.Or(pool.ConfigProfile, defaultConfigProfile),
		DiskSelectionStrategy: cmp.Or(pool.DiskSelectionStrategy, "smallest"),
	}
}

// nicoDSXSpec is nicoForgeSpec against a DSX datahall.
//
// It builds the same request: the nico-forge backend is named for the
// provider its payloads were first written for, not for the API underneath,
// and buildNicoForgeRequest asks only for an iPXE script and pre-kubeadm
// commands. Both come from BuildForgeNodePayloads, which the DSX provider
// already renders its own site node with.
//
// What differs is where the identifiers come from, and that a DSX fabric is
// FNN: there is no subnet to name, only a prefix.
//
// The control plane takes selection.CPU as it does on Forge. At a datahall
// with no CPU machine that is already the GPU shape -- Resolve fills it from
// matchSiteControllerType, which falls back -- so a GB200 control plane needs
// no special case here, and a datahall that gains CPU boxes stops producing
// one without this file changing.
func nicoDSXSpec(_ config.Config, name string, shape clusterShape) nvke.ClusterSpec {
	GinkgoHelper()

	env, err := runEnvironment()
	Expect(err).NotTo(HaveOccurred(), "should load the environment named by -e2e.environment")
	dsxEnv, err := dsx.Decode(env.Infrastructure)
	Expect(err).NotTo(HaveOccurred(), "should read it as a DSX declaration")
	capabilities, err := runCapabilities()
	Expect(err).NotTo(HaveOccurred(), "should parse -e2e.capabilities")
	// The same resolution the site lane made, so a tenant lands on the
	// datahall hosting it rather than on whichever other one offers a
	// matching shape.
	selection, err := dsxEnv.Resolve(capabilities)
	Expect(err).NotTo(HaveOccurred(), "should resolve a datahall offering the requested shapes")
	site := selection.Site
	controlPlaneType, workerType := selection.CPU, selection.WorkerType()

	versions, err := requireVersions(versionKube, versionCNIOVN)
	Expect(err).NotTo(HaveOccurred(), "should name the Kubernetes and CNI versions under qualification")
	kubernetesVersion := versions[versionKube]

	payloads, err := forgeRolePayloads(roleArchs{
		ControlPlane: controlPlaneType.Arch(),
		Worker:       workerType.Arch(),
	}, kubernetesVersion, versions[versionCNIOVN])
	Expect(err).NotTo(HaveOccurred(), "should render the node payloads")

	return nvke.ClusterSpec{
		Name:              name,
		Backend:           nvke.BackendNicoForge,
		KubernetesVersion: kubernetesVersion,
		CNIPlugin:         cmp.Or(shape.cniPlugin, CNIPluginOVN),
		CNIVersion:        versions[versionCNIOVN],
		OrgID:             dsxEnv.Org.ID,
		SiteID:            site.ID,
		VpcID:             site.VpcID,
		NodePayloads:      payloads,
		ControlPlane: nvke.ControlPlane{
			Pools:         dsxNodePoolSpecs(shape.controlPlanePools, "control-plane", "cp", site, controlPlaneType),
			Config:        dsxControlPlaneConfig(shape.controlPlaneConfig, site, controlPlaneType),
			EtcdEncrypted: shape.etcdEncrypted,
			// Empty, as on Forge, and here it is also the only option: each
			// instance consumes a /31 and lands on the odd address, so the
			// prefix has no spare address for a VIP to take.
			ControlPlaneVIP: shape.controlPlaneVIP,
		},
		Workers: dsxNodePoolSpecs(shape.workerPools, "worker", "w", site, workerType),
	}
}

// dsxNodePoolSpecs fills each pool from the datahall. forgeNodePoolSpecs with
// the subnet half removed: a DSX site declares a prefix and nothing else, so
// there is no choice to make between the two.
func dsxNodePoolSpecs(
	pools []nvke.NodePoolSpec,
	defaultName, defaultNodeNamePrefix string,
	site dsx.Site,
	instanceType dsx.InstanceType,
) []nvke.NodePoolSpec {
	out := make([]nvke.NodePoolSpec, 0, len(pools))
	for _, pool := range pools {
		out = append(out, nvke.NodePoolSpec{
			Name:                 cmp.Or(pool.Name, defaultName),
			NodeNamePrefix:       cmp.Or(pool.NodeNamePrefix, defaultNodeNamePrefix),
			DesiredNodeCount:     pool.DesiredNodeCount,
			SystemReservedMemory: cmp.Or(pool.SystemReservedMemory, "16Gi"),
			InstanceTypeID:       cmp.Or(pool.InstanceTypeID, instanceType.ID),
			VpcPrefixIDs:         defaultStrings(pool.VpcPrefixIDs, site.VpcPrefixID),
			SSHKeyGroupIDs:       defaultStrings(pool.SSHKeyGroupIDs, site.SSHKeyGroupID),
		})
	}
	return out
}

// dsxControlPlaneConfig fills a cluster-level control-plane config from the
// datahall, the same defaults dsxNodePoolSpecs applies to a pool.
func dsxControlPlaneConfig(
	cfg *nvke.ControlPlaneConfigSpec,
	site dsx.Site,
	instanceType dsx.InstanceType,
) *nvke.ControlPlaneConfigSpec {
	if cfg == nil {
		return nil
	}
	out := *cfg
	out.SystemReservedMemory = cmp.Or(out.SystemReservedMemory, "16Gi")
	out.InstanceTypeID = cmp.Or(out.InstanceTypeID, instanceType.ID)
	out.VpcPrefixIDs = defaultStrings(out.VpcPrefixIDs, site.VpcPrefixID)
	out.SSHKeyGroupIDs = defaultStrings(out.SSHKeyGroupIDs, site.SSHKeyGroupID)
	return &out
}

// optionalStrings is defaultStrings for a default that may legitimately be
// unset, where the empty result has to stay empty rather than become a
// one-element slice holding "".
func optionalStrings(s []string, defaultValue string) []string {
	if len(s) == 0 && defaultValue == "" {
		return nil
	}
	return defaultStrings(s, defaultValue)
}

func defaultStrings(s []string, defaultValue string) []string {
	if len(s) == 0 {
		return []string{defaultValue}
	}
	return s
}

func nicoVMSMultiPodCIDRSpec(cfg config.Config, name string, shape clusterShape) nvke.ClusterSpec {
	spec := nicoVMSSpec(cfg, name, shape)
	spec.Name = name + "-multi-pod-cidr"
	spec.Network = &nvke.ClusterNetworkSpec{
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

// validateControlPlaneNodeLabels asserts that the control plane reports the node labels the entry created it with,
// and that every current control-plane node carries them alongside the kubeadm role label. phase names the
// lifecycle step the current nodes came from.
func validateControlPlaneNodeLabels(test *tenantTest, phase string) {
	It("applies the control plane's node labels to its nodes "+phase, func(ctx context.Context) {
		requested := test.spec.ControlPlane.Config.NodeLabels
		Expect(requested).NotTo(BeEmpty(), "the entry must create the control plane with node labels")

		By("Verifying the control plane reports the labels it was created with")
		controlPlane := expectSucceed(test.nvke.GetClusterControlPlane(ctx, test.cluster.ID))
		Expect(controlPlane.Config.NodeLabels).To(Equal(requested))

		By("Verifying every control-plane node carries the labels and the kubeadm role label")
		wantLabels := maps.Clone(requested)
		wantLabels[controlPlaneRoleLabelKey] = ""
		eventuallyNodesHaveMetadata(ctx, test, expectSucceed(test.nvke.ControlPlaneNodeNames(ctx, test.cluster.ID)), wantLabels, nil)
	}, Label("cp-controller", "node-metadata"), NodeTimeout(30*time.Minute))
}
