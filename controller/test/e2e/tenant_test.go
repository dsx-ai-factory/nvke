// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/config"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/kube"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/nke"
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

var _ = DescribeTableSubtree("NKE with backend",
	func(spec testSpec) {
		var (
			test    tenantTest
			failed  bool
			deleted bool
		)

		// Record any spec failure so the cluster delete can honor preserveOnFailure.
		AfterEach(func() { failed = failed || CurrentSpecReport().Failed() })

		It("creates the cluster", func(ctx context.Context) {
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

			validateOIDCPostCreate(&test)
			validateNodeReadinessPostCreate(&test)
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
			validatePodCIDRPostCreate(&test)
			if spec.validate.clusterConfig {
				validateClusterConfig(&test)
			}
			if spec.validate.nodeReplacement {
				validateNodeReplacement(&test)
			}
			if spec.validate.manualScaleDown {
				validateManualScaleDownStability(&test)
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

			validateOIDCPostDelete(&test)
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
	Entry("CAPD", Ordered, ContinueOnFailure, Label("capd", "happy-path"), testSpec{
		buildSpec: capdSpec,
		shape: clusterShape{
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3}},
			workerPools:       []nke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options:  nke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
		validate: validations{scale: true, reboot: true},
	}),
	Entry("forge-vms", Ordered, ContinueOnFailure, Label("forge-vms", "happy-path"), testSpec{
		buildSpec: forgeSpec,
		shape: clusterShape{
			kubernetesVersion: "1.34.2",
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3}},
			workerPools:       []nke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options:  nke.Options{Provision: 30 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
		validate: validations{scale: true, reboot: true},
	}),
	Entry("forge-vms k8s upgrade", Ordered, ContinueOnFailure, Label("forge-vms", "happy-path", "upgrade"), testSpec{
		buildSpec: forgeSpec,
		shape: clusterShape{
			kubernetesVersion: upgradeStartingKubernetesVersion,
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3}},
			workerPools:       []nke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options:  nke.Options{Provision: 30 * time.Minute, Upgrade: 90 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
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
	Entry("forge-vms OS profiles", Ordered, ContinueOnFailure, Label("forge-vms", "extended", "os-profiles"), testSpec{
		buildSpec: forgeSpec,
		shape: clusterShape{
			cniPlugin:         CNIPluginCalico, // Simply to make this test run faster (520s vs 1011s).
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 1}},
			workerPools: []nke.NodePoolSpec{
				// Old agent image pins the node-agent version to 0.1.22, which is before the new nkeproto.CheckCloudInitStatus was introduced in version 0.1.24.
				// So this osProfile test exists to prove that CWE can still provision nodePools that may still be pointed at older node-agent versions.
				{Name: "worker-old-agent", NodeNamePrefix: "wold", DesiredNodeCount: 1, OSProfile: "nke-forge-vms-24.04-amd64-fde-old-agent"},
				// FIXME: Remove the larger instance type ID back to the default when the KubeReservedMem is also plumbed as an api parameter.
				// https://jirasw.nvidia.com/browse/NKX-12957
				{Name: "worker-260625-24.04", NodeNamePrefix: "w260625", DesiredNodeCount: 1, OSProfile: "nke-260625-24.04-x86-fde", InstanceTypeID: "a1b2c3d4-0005-4000-8000-000000000002"},
				{Name: "worker-260602-24.04", NodeNamePrefix: "w260602", DesiredNodeCount: 1, OSProfile: "nke-260602-24.04-x86-fde", InstanceTypeID: "a1b2c3d4-0005-4000-8000-000000000002"},
			},
		},
		options: nke.Options{Provision: 30 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
	}),
	Entry("forge-vms multi pod CIDRs", Ordered, ContinueOnFailure, Label("forge-vms", "multi-pod-cidrs"), testSpec{
		buildSpec: forgeMultiPodCIDRSpec,
		shape: clusterShape{
			kubernetesVersion: "1.34.2",
			cniPlugin:         CNIPluginOVN,
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 3, InstanceTypeID: forgeLargeInstanceID}},
			workerPools:       []nke.NodePoolSpec{{DesiredNodeCount: 1, InstanceTypeID: forgeLargeInstanceID}},
		},
		options: nke.Options{Provision: 30 * time.Minute, Delete: 10 * time.Minute, Poll: 30 * time.Second},
	}),
	Entry("forge-vms node pool lifecycle", Ordered, ContinueOnFailure, Label("forge-vms", "extended", "nodepool"), testSpec{
		buildSpec: forgeSpec,
		shape: clusterShape{
			cniPlugin:         CNIPluginOVN, // OVN provisions faster than the Calico default.
			controlPlanePools: []nke.NodePoolSpec{{DesiredNodeCount: 1}},
			workerPools:       []nke.NodePoolSpec{{DesiredNodeCount: 1}},
		},
		options:  nke.Options{Provision: 30 * time.Minute, Delete: 10 * time.Minute, Poll: 10 * time.Second},
		validate: validations{clusterConfig: true, nodeReplacement: true, manualScaleDown: true, nodePoolLifecycle: true, nodePoolCascade: true},
	}),
)

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
}

type validations struct {
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
}

// setupTenantTest builds the per-backend wiring (clients, trust domain, logger)
// and preflights CWE reachability through the apiserver service-proxy. It does
// not create a cluster.
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

	cweClient, err := nke.NewClient(restConfig, timeouts)
	if err != nil {
		return tenantTest{}, fmt.Errorf("building CWE API client: %w", err)
	}

	trustDomain, err := kube.TrustDomain(cfg.Kube.Kubeconfig, cfg.Kube.Context)
	if err != nil {
		GinkgoLogr.Info("SPIRE trust domain could not be derived from kubeconfig context; OIDC validations will skip", "error", err)
	}

	tenantClients, err := kube.BuildTenantClientFactory(restConfig)
	if err != nil {
		return tenantTest{}, fmt.Errorf("building tenant Kubernetes client factory: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := cweClient.API().GetClusterWithResponse(pingCtx, uuid.New())
	if err != nil {
		return tenantTest{}, fmt.Errorf("reaching CWE through the apiserver service-proxy: %w", err)
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

	By(fmt.Sprintf("Creating the %s tenant cluster", t.spec.Backend))
	cluster, err := t.nke.CreateCluster(ctx, t.spec)
	Expect(err).NotTo(HaveOccurred(), "should create the cluster")
	t.log.Info("Created cluster", "name", cluster.Name, "id", cluster.ID)

	By("Waiting for the tenant cluster to be provisioned")
	start := time.Now()
	cluster, err = t.nke.WaitForClusterReady(ctx, cluster.ID)
	Expect(err).NotTo(HaveOccurred(), "should provision the cluster")
	t.log.Info("Cluster provisioned", "name", cluster.Name, "duration", time.Since(start).Round(time.Second).String())

	t.cluster = cluster
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

	forgeMultiPodCIDR        = "10.250.0.0/23,10.252.0.0/22"
	forgeMultiPodServiceCIDR = "10.130.0.0/16"
)

// capdSpec builds a CAPD topology with the requested cluster shape.
func capdSpec(cfg config.Config, name string, shape clusterShape) nke.ClusterSpec {
	return nke.ClusterSpec{
		Name:              name,
		Backend:           nke.BackendCAPD,
		KubernetesVersion: defaultString(shape.kubernetesVersion, defaultKubernetesVersion),
		CNIPlugin:         defaultString(shape.cniPlugin, CNIPluginCalico),
		CAPDNodeImage:     cfg.Cluster.CAPDNodeImage,
		ControlPlane: nke.ControlPlane{
			Pools:          capdNodePoolSpecs(shape.controlPlanePools, "control-plane", "cp"),
			EtcdEncrypted:  shape.etcdEncrypted,
			DisableKubeVip: false,
		},
		Workers: capdNodePoolSpecs(shape.workerPools, "worker", "w"),
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

// forge-vms Forge emulator seed identifiers, mirroring
// cloud-workflow-engine/forge-vms/internal/seed/default-seed.yaml. The local
// forge-vms environment always seeds these deterministic IDs.
const (
	forgeOrgID           = "novxwn552ssc"
	forgeSiteID          = "a1b2c3d4-0001-4000-8000-000000000001"
	forgeVpcID           = "a1b2c3d4-0002-4000-8000-000000000001"
	forgeVpcPrefixID     = "a1b2c3d4-0004-4000-8000-000000000001"
	forgeInstanceTypeID  = "a1b2c3d4-0005-4000-8000-000000000001"
	forgeLargeInstanceID = "a1b2c3d4-0005-4000-8000-000000000003"
	forgeSSHKeyGroupID   = "a1b2c3d4-0006-4000-8000-000000000001"
	forgeOSProfile       = "nke-forge-vms-24.04-amd64-fde"
	forgeCPConfigProfile = "forge-vms-cp"
	forgeWConfigProfile  = "forge-vms-worker"
)

// forgeSpec builds a forge-vms topology using the local emulator's seed
// identifiers. kube-vip is left enabled (the forge-vms BGP speaker advertises
// the control-plane VIP), unlike the CAPD topology.
func forgeSpec(_ config.Config, name string, shape clusterShape) nke.ClusterSpec {
	return nke.ClusterSpec{
		Name:              name,
		Backend:           nke.BackendForgeVMS,
		KubernetesVersion: defaultString(shape.kubernetesVersion, defaultKubernetesVersion),
		CNIPlugin:         defaultString(shape.cniPlugin, CNIPluginOVN),
		OrgID:             forgeOrgID,
		SiteID:            forgeSiteID,
		VpcID:             forgeVpcID,
		ControlPlane: nke.ControlPlane{
			Pools:          forgeNodePoolSpecs(shape.controlPlanePools, "control-plane", "cp", forgeOSProfile, forgeCPConfigProfile),
			DisableKubeVip: false,
			EtcdEncrypted:  shape.etcdEncrypted,
		},
		Workers: forgeNodePoolSpecs(shape.workerPools, "worker", "w", forgeOSProfile, forgeWConfigProfile),
	}
}

func forgeNodePoolSpecs(pools []nke.NodePoolSpec, defaultName, defaultNodeNamePrefix, defaultOSProfile, defaultConfigProfile string) []nke.NodePoolSpec {
	out := make([]nke.NodePoolSpec, 0, len(pools))
	for _, pool := range pools {
		out = append(out, forgeNodePoolSpec(pool, defaultName, defaultNodeNamePrefix, defaultOSProfile, defaultConfigProfile))
	}
	return out
}

func forgeNodePoolSpec(pool nke.NodePoolSpec, defaultName, defaultNodeNamePrefix, defaultOSProfile, defaultConfigProfile string) nke.NodePoolSpec {
	return nke.NodePoolSpec{
		Name:                  defaultString(pool.Name, defaultName),
		NodeNamePrefix:        defaultString(pool.NodeNamePrefix, defaultNodeNamePrefix),
		DesiredNodeCount:      pool.DesiredNodeCount,
		SystemReservedMemory:  defaultString(pool.SystemReservedMemory, "250Mi"),
		InstanceTypeID:        defaultString(pool.InstanceTypeID, forgeInstanceTypeID),
		VpcPrefixIDs:          defaultStrings(pool.VpcPrefixIDs, forgeVpcPrefixID),
		SSHKeyGroupIDs:        defaultStrings(pool.SSHKeyGroupIDs, forgeSSHKeyGroupID),
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

func forgeMultiPodCIDRSpec(cfg config.Config, name string, shape clusterShape) nke.ClusterSpec {
	spec := forgeSpec(cfg, name, shape)
	spec.Name = name + "-multi-pod-cidr"
	spec.Network = &nke.ClusterNetworkSpec{
		PodCIDR:          forgeMultiPodCIDR,
		ServiceCIDR:      forgeMultiPodServiceCIDR,
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
