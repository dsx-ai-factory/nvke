// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/test/e2e/internal/nke"
)

// validateControllerManagedControlPlane asserts a cluster provisioned from the
// cluster-level controlPlaneConfig (control-plane controller, no control-plane
// node pool) has no such pool and brings the desired control-plane nodes to Ready
// in both CWE and the tenant API. Requires flag-on CWE (a flag-off CWE rejects
// controlPlaneConfig at create); self-skips otherwise.
func validateControllerManagedControlPlane(test *tenantTest) {
	It("manages the control plane from the cluster-level config without a control-plane node pool", func(ctx context.Context) {
		if os.Getenv("PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER") != "true" {
			Skip("requires CWE deployed with PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=true")
		}

		Expect(test.spec.ControlPlane.Config).NotTo(BeNil(),
			"the control-plane-controller spec must set a cluster-level control plane config")
		desired := test.spec.ControlPlane.Config.DesiredCount

		By("Verifying the cluster has no control-plane node pool")
		pools := expectSucceed(test.nke.ListNodePools(ctx, test.cluster.ID))
		for _, pool := range pools {
			Expect(pool.ControlPlane).To(BeFalse(),
				"a control-plane-controller cluster must have no control-plane node pool (found %q)", pool.Name)
		}

		By("Waiting for the desired cluster-level control-plane nodes to become KubernetesReady")
		Eventually(func(ctx context.Context) error {
			return test.nke.AssertControlPlaneNodesReady(ctx, test.cluster.ID, desired)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "cluster-level control-plane nodes should become ready")

		By("Asserting the tenant cluster reports Ready for the controller-managed control-plane nodes")
		assertControlPlaneNodesReadyInTenant(ctx, test, desired)
	}, Label("cp-controller", "readiness"), NodeTimeout(90*time.Minute))
}

// controlPlaneControllerUpdateDesiredCount is the desired control-plane node
// count the update spec scales to. It is an odd value so the scaled control
// plane keeps a valid etcd quorum (the entries create the cluster with a single
// control-plane node, so this exercises a 1 -> 3 scale-up).
const controlPlaneControllerUpdateDesiredCount = 3

// validateControllerManagedControlPlaneUpdate PATCHes the cluster-level
// controlPlaneConfig desiredCount and waits for the control plane to converge on
// the new count in CWE and the tenant API. The controller-managed control plane
// has no node pool, so it can't be scaled through the node pool API. Requires
// flag-on CWE; self-skips otherwise.
func validateControllerManagedControlPlaneUpdate(test *tenantTest) {
	It("scales the controller-managed control plane to a new desired count", func(ctx context.Context) {
		if os.Getenv("PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER") != "true" {
			Skip("requires CWE deployed with PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=true")
		}

		Expect(test.spec.ControlPlane.Config).NotTo(BeNil(),
			"the control-plane-controller spec must set a cluster-level control plane config")
		desired := controlPlaneControllerUpdateDesiredCount

		By(fmt.Sprintf("Updating the cluster-level control-plane desired count to %d", desired))
		expectSucceed(test.nke.UpdateControlPlaneDesiredCount(ctx, test.cluster.ID, desired))

		By("Waiting for the updated control-plane node count to become KubernetesReady in CWE")
		Eventually(func(ctx context.Context) error {
			return test.nke.AssertControlPlaneNodesReady(ctx, test.cluster.ID, desired)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "control plane should scale to the new desired count in CWE")

		By("Asserting the tenant cluster reports Ready for the updated control-plane nodes")
		assertControlPlaneNodesReadyInTenant(ctx, test, desired)
	}, Label("cp-controller", "update"), NodeTimeout(90*time.Minute))
}

// controlPlaneControllerRollConfigProfile is the config profile the rolling
// update spec switches the control plane to. Its value only needs to differ from
// the created config to bump the control-plane configuration revision; a missing
// config profile file is tolerated as empty at provisioning time, and CAPI nodes
// carry no config profile, so any distinct name is a safe roll trigger for CAPD.
const controlPlaneControllerRollConfigProfile = "e2e-cp-controller-roll"

// validateControllerManagedControlPlaneRollingUpdate PATCHes a new config profile
// to bump the control-plane config revision and asserts the controller rolls the
// old nodes onto new ones at the same desired count, Ready in both CWE and the
// tenant API (mirrors the node-pool baseline in upgrade_test.go, but via the
// cluster-level config revision). Runs after the desiredCount update so quorum
// survives a one-at-a-time roll. Requires flag-on CWE; self-skips otherwise.
func validateControllerManagedControlPlaneRollingUpdate(test *tenantTest) {
	It("rolls the controller-managed control plane on a control-plane config revision", func(ctx context.Context) {
		if os.Getenv("PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER") != "true" {
			Skip("requires CWE deployed with PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=true")
		}

		Expect(test.spec.ControlPlane.Config).NotTo(BeNil(),
			"the control-plane-controller spec must set a cluster-level control plane config")

		By("Capturing the current controller-managed control-plane node set")
		oldNodeIDs := expectSucceed(test.nke.ControlPlaneNodeIDs(ctx, test.cluster.ID))
		desired := len(oldNodeIDs)
		Expect(desired).To(BeNumerically(">=", 3),
			"a config-revision roll needs a multi-node control plane to keep etcd quorum while rolling")

		By("Changing the control-plane config profile to bump the configuration revision")
		rollProfile := controlPlaneControllerRollConfigProfile
		if test.spec.Backend == nke.BackendNicoVMS {
			// Forge requires a real config profile; nico-vms-cp is what create uses.
			rollProfile = nicoVMSWConfigProfile
		}
		expectSucceed(test.nke.UpdateControlPlaneConfigProfile(ctx, test.cluster.ID, rollProfile))

		By("Waiting for every old control-plane node to be replaced and KubernetesReady in CWE")
		Expect(test.nke.WaitForControlPlaneNodesRolled(ctx, test.cluster.ID, desired, oldNodeIDs)).To(Succeed(),
			"control plane should roll onto new nodes at the same desired count")

		By("Asserting the tenant cluster reports Ready for the rolled control-plane nodes")
		assertControlPlaneNodesReadyInTenant(ctx, test, desired)
	}, Label("cp-controller", "rolling-update"), NodeTimeout(90*time.Minute))
}

// assertControlPlaneNodesReadyInTenant builds an authenticated tenant client and
// eventually asserts that the controller-managed control-plane nodes CWE reports
// (NodePoolID == uuid.Nil) are present and Ready in the tenant Kubernetes API. It
// mirrors validateNodeReadinessPostCreate's tenant-side node-list pattern, but
// keys off the CWE-reported control-plane node names rather than node pools,
// since the controller-managed control plane has no owning pool.
func assertControlPlaneNodesReadyInTenant(ctx context.Context, test *tenantTest, desired int) {
	GinkgoHelper()

	var tenantClient client.Client
	Eventually(func(ctx context.Context) error {
		var err error
		tenantClient, err = test.tenantClients.GetClient(ctx, test.cluster.Name)
		return err
	}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())

	Eventually(func(ctx context.Context) error {
		return controlPlaneNodesReadyInTenant(ctx, test, tenantClient, desired)
	}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
		Should(Succeed(), "controller-managed control-plane nodes should be Ready in the tenant API")
}

func controlPlaneNodesReadyInTenant(ctx context.Context, test *tenantTest, tenantClient client.Client, desired int) error {
	names, err := test.nke.ControlPlaneNodeNames(ctx, test.cluster.ID)
	if err != nil {
		return err
	}
	if len(names) != desired {
		return fmt.Errorf("CWE reports %d controller-managed control-plane nodes; want %d", len(names), desired)
	}

	var tenantNodes corev1.NodeList
	if err := tenantClient.List(ctx, &tenantNodes); err != nil {
		return fmt.Errorf("listing tenant Kubernetes nodes: %w", err)
	}
	for _, name := range names {
		nodeIndex := slices.IndexFunc(tenantNodes.Items, func(node corev1.Node) bool {
			return node.Name == name
		})
		if nodeIndex < 0 {
			return fmt.Errorf("tenant cluster does not report control-plane node %q", name)
		}
		if ready, detail := kubernetesNodeReady(&tenantNodes.Items[nodeIndex]); !ready {
			return fmt.Errorf("tenant control-plane node %q is not Ready: %s", name, detail)
		}
	}
	return nil
}
