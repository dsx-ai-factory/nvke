// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/nke"
)

// validateControllerManagedControlPlane asserts a cluster provisioned from the
// cluster-level controlPlaneConfig (control-plane controller, no control-plane
// node pool) has no such pool and brings the desired control-plane nodes to Ready
// in both CWE and the tenant API. Requires flag-on CWE (a flag-off CWE rejects
// controlPlaneConfig at create); self-skips otherwise.
func validateControllerManagedControlPlane(test *tenantTest) {
	It("manages the control plane from the cluster-level config without a control-plane node pool", func(ctx context.Context) {
		if !controlPlaneControllerEnabled() {
			Skip("requires CWE deployed with " + controlPlaneControllerEnv + " on")
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
		if !controlPlaneControllerEnabled() {
			Skip("requires CWE deployed with " + controlPlaneControllerEnv + " on")
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

// controlPlaneControllerRollKubernetesVersion is the version the rolling update
// spec upgrades a CAPI control plane to. The control-plane controller treats only
// kubernetesVersion, osProfile and configProfile as replacing fields, and the
// latter two are Nico-only, so the version is the sole roll trigger a CAPI caller
// can reach. It must stay within the minor of the CAPD node image's base
// (KINDEST_NODE_TAG in hack/capd-kindest-node/Dockerfile), because that base
// supplies the kubeadm that has to serve the target.
const controlPlaneControllerRollKubernetesVersion = "1.34.8"

// validateControllerManagedControlPlaneRollingUpdate bumps the control-plane
// config revision and asserts the controller rolls the old nodes onto new ones at
// the same desired count, Ready in both CWE and the tenant API (mirrors the
// node-pool baseline in upgrade_test.go, but via the cluster-level config
// revision). The field it bumps depends on the provider, since the replacing
// fields differ. Runs after the desiredCount update so quorum survives a
// one-at-a-time roll. Requires flag-on CWE; self-skips otherwise.
func validateControllerManagedControlPlaneRollingUpdate(test *tenantTest) {
	It("rolls the controller-managed control plane on a control-plane config revision", func(ctx context.Context) {
		if !controlPlaneControllerEnabled() {
			Skip("requires CWE deployed with " + controlPlaneControllerEnv + " on")
		}

		Expect(test.spec.ControlPlane.Config).NotTo(BeNil(),
			"the control-plane-controller spec must set a cluster-level control plane config")

		By("Capturing the current controller-managed control-plane node set")
		oldNodeIDs := expectSucceed(test.nke.ControlPlaneNodeIDs(ctx, test.cluster.ID))
		desired := len(oldNodeIDs)
		Expect(desired).To(BeNumerically(">=", 3),
			"a config-revision roll needs a multi-node control plane to keep etcd quorum while rolling")

		if test.spec.Backend == nke.BackendNicoVMS {
			By("Changing the control-plane config profile to bump the configuration revision")
			// Forge requires a real config profile; nico-vms-cp is what create uses.
			expectSucceed(test.nke.UpdateControlPlaneConfigProfile(ctx, test.cluster.ID, nicoVMSWConfigProfile))
		} else {
			By("Upgrading the control-plane Kubernetes version to bump the configuration revision")
			expectSucceed(test.nke.UpgradeClusterControlPlane(ctx, test.cluster.ID, controlPlaneControllerRollKubernetesVersion))
		}

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

// controlPlaneControllerDeleteSurgeCount is the size the control plane is grown
// to before a control-plane node is deleted. It is one above the three-node
// fault-tolerant floor, so the delete is the supported replacement-first shape
// rather than one the controller refuses.
const controlPlaneControllerDeleteSurgeCount = 4

// validateControllerManagedControlPlaneDeleteRefused asserts the controller-level
// quorum floor: deleting a control-plane node from a three-node control plane is
// refused at the API with an actionable message, without the teardown ever
// starting. This is the negative half of serialized control-plane delete, and it
// is deterministic because nothing asynchronous has to happen for it to hold.
func validateControllerManagedControlPlaneDeleteRefused(test *tenantTest) {
	It("refuses a control-plane node delete that would leave fewer than three", func(ctx context.Context) {
		if !controlPlaneControllerEnabled() {
			Skip("requires CWE deployed with " + controlPlaneControllerEnv + " on")
		}

		By("Confirming the control plane is at the three-node fault-tolerant floor")
		Eventually(func(ctx context.Context) error {
			return test.nke.AssertControlPlaneNodesReady(ctx, test.cluster.ID, controlPlaneControllerUpdateDesiredCount)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "this spec needs a settled three-node control plane")

		// Assert on Joined as well, so the refusal below is known to come from the
		// three-to-two boundary rather than from an incidentally smaller joined set.
		Expect(test.nke.WaitForJoinedControlPlaneNodes(ctx, test.cluster.ID, controlPlaneControllerUpdateDesiredCount)).To(Succeed(),
			"all three control-plane nodes must report Joined for this to test the floor it claims to")

		victim := expectSucceed(test.nke.ControlPlaneNodeIDs(ctx, test.cluster.ID))
		Expect(victim).To(HaveLen(controlPlaneControllerUpdateDesiredCount))
		var target uuid.UUID
		for id := range victim {
			target = id
			break
		}

		By("Deleting a control-plane node and expecting the quorum floor to refuse it")
		message := expectSucceed(test.nke.DeleteNodeExpectingRefusal(ctx, target))
		Expect(message).To(ContainSubstring("below the minimum of 3"),
			"the refusal should name the fault-tolerant floor it is protecting")

		By("Verifying the refused delete left no trace on the control plane")
		Consistently(func(ctx context.Context) error {
			state, err := test.nke.ControlPlaneDeleteStateAt(ctx, test.cluster.ID)
			if err != nil {
				return err
			}
			if state.InFlight() {
				return fmt.Errorf("a refused delete started anyway: %d deleting, %d requested",
					state.Deleting, state.DeleteRequested)
			}
			if len(state.NodeIDs) != controlPlaneControllerUpdateDesiredCount {
				return fmt.Errorf("control plane has %d nodes; want %d", len(state.NodeIDs), controlPlaneControllerUpdateDesiredCount)
			}
			return nil
		}).WithContext(ctx).WithTimeout(30*time.Second).WithPolling(5*time.Second).
			Should(Succeed(), "a refused delete must not mark the node or start a teardown")
	}, Label("cp-controller", "cp-delete"), NodeTimeout(20*time.Minute))
}

// validateControllerManagedControlPlaneSerializedDelete grows the control plane
// to four, then deletes one node back down to three and asserts the delete is
// owned by the control-plane controller and serialized: never more than one
// teardown in flight, no control-plane node joins while one is on its way out,
// and the cluster-level control-plane status reports the delete while it runs.
//
// Growing first is what makes the delete legal under the quorum floor, and it is
// also the replacement-first shape operators are told to use.
func validateControllerManagedControlPlaneSerializedDelete(test *tenantTest) {
	It("serializes a quorum-safe control-plane node delete and gates joins on it", func(ctx context.Context) {
		if !controlPlaneControllerEnabled() {
			Skip("requires CWE deployed with " + controlPlaneControllerEnv + " on")
		}

		By(fmt.Sprintf("Growing the control plane to %d so the delete clears the quorum floor", controlPlaneControllerDeleteSurgeCount))
		expectSucceed(test.nke.UpdateControlPlaneDesiredCount(ctx, test.cluster.ID, controlPlaneControllerDeleteSurgeCount))
		Eventually(func(ctx context.Context) error {
			return test.nke.AssertControlPlaneNodesReady(ctx, test.cluster.ID, controlPlaneControllerDeleteSurgeCount)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "control plane should grow before the delete")

		// Ready is not enough: the quorum floor counts Joined, which the
		// ProvisionNode workflow writes after the tenant node-status controller has
		// already marked the node KubernetesReady. Deleting inside that window is
		// refused for a control plane that is in fact large enough.
		By("Waiting for every grown control-plane node to report Joined, which is what the quorum floor counts")
		Expect(test.nke.WaitForJoinedControlPlaneNodes(ctx, test.cluster.ID, controlPlaneControllerDeleteSurgeCount)).To(Succeed(),
			"the quorum floor counts joined nodes, so the delete must not race the Joined write")

		By("Lowering the desired count so the delete shrinks the control plane instead of replacing the node")
		expectSucceed(test.nke.UpdateControlPlaneDesiredCount(ctx, test.cluster.ID, controlPlaneControllerUpdateDesiredCount))

		before := expectSucceed(test.nke.ControlPlaneDeleteStateAt(ctx, test.cluster.ID))
		Expect(before.NodeIDs).To(HaveLen(controlPlaneControllerDeleteSurgeCount))
		Expect(before.InFlight()).To(BeFalse(), "no control-plane delete should be in flight before this spec starts one")
		var target uuid.UUID
		for id := range before.JoinedNodeIDs {
			target = id
			break
		}
		Expect(target).NotTo(Equal(uuid.Nil), "the control plane should have a joined node to delete")

		By("Deleting one control-plane node")
		Expect(test.nke.DeleteNode(ctx, target)).To(Succeed(),
			"a delete leaving three joined control-plane nodes should be accepted")

		By("Watching the delete run: one teardown at a time, no joins while it runs, and visible in the cluster status")
		observedDeleteInStatus := false
		joinedWatermark := before.JoinedNodeIDs
		Eventually(func(ctx context.Context) error {
			state, err := test.nke.ControlPlaneDeleteStateAt(ctx, test.cluster.ID)
			if err != nil {
				return err
			}

			// Serialization: the controller starts one teardown at a time.
			Expect(state.Deleting).To(BeNumerically("<=", 1),
				"at most one control-plane teardown may be in flight at a time")

			if state.InFlight() {
				// Observability: the delete must be visible at the cluster level.
				observedDeleteInStatus = true

				// Join gate: while a delete is in flight the joined set must not grow.
				for id := range state.JoinedNodeIDs {
					if _, known := joinedWatermark[id]; !known {
						return StopTrying(fmt.Sprintf(
							"control-plane node %s joined while a control-plane delete was in flight", id,
						))
					}
				}
				return fmt.Errorf("control-plane delete still in flight: %d deleting, %d requested",
					state.Deleting, state.DeleteRequested)
			}

			// Once nothing is in flight the joined set may legitimately grow again.
			joinedWatermark = state.JoinedNodeIDs

			if _, present := state.NodeIDs[target]; present {
				return fmt.Errorf("deleted control-plane node %s is still present", target)
			}
			if len(state.NodeIDs) != controlPlaneControllerUpdateDesiredCount {
				return fmt.Errorf("control plane has %d nodes; want %d",
					len(state.NodeIDs), controlPlaneControllerUpdateDesiredCount)
			}
			if state.ReadyNodeCount != controlPlaneControllerUpdateDesiredCount {
				return fmt.Errorf("control plane has %d nodes KubernetesReady; want %d",
					state.ReadyNodeCount, controlPlaneControllerUpdateDesiredCount)
			}
			return nil
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(2*time.Second).
			Should(Succeed(), "the control plane should settle at three nodes with the deleted node gone")

		Expect(observedDeleteInStatus).To(BeTrue(),
			"the cluster control-plane status should have reported the in-progress delete at least once")

		By("Asserting the tenant cluster reports Ready for the surviving control-plane nodes")
		assertControlPlaneNodesReadyInTenant(ctx, test, controlPlaneControllerUpdateDesiredCount)
	}, Label("cp-controller", "cp-delete"), NodeTimeout(45*time.Minute))
}

// validateControllerManagedControlPlaneUnreachableDelete is the repair path an
// operator needs when a control-plane node dies on a three-node control plane.
// The quorum floor refuses an ordinary delete at that size -- removing one always
// leaves two -- but a node that has stopped reporting healthy is not providing
// fault tolerance, so refusing to remove it would preserve a three-node control
// plane in name only and leave no way to take a failed member out through the API.
//
// The same node is deleted twice: once while healthy, which must be refused, and
// again once it is genuinely unreachable, which must be accepted. Health is the
// only thing that changes between them. Stopping kubelet rather than marking the
// node is what makes drain and the graceful-shutdown wait actually fail, so this
// exercises their tolerations instead of stepping past them, and asserting the
// teardown runs to completion is what the unit tests cannot cover.
func validateControllerManagedControlPlaneUnreachableDelete(test *tenantTest) {
	It("refuses deleting a healthy control-plane node but removes an unreachable one", func(ctx context.Context) {
		if !controlPlaneControllerEnabled() {
			Skip("requires CWE deployed with " + controlPlaneControllerEnv + " on")
		}

		By("Confirming the control plane is settled at the three-node fault-tolerant floor")
		Eventually(func(ctx context.Context) error {
			return test.nke.AssertControlPlaneNodesReady(ctx, test.cluster.ID, controlPlaneControllerUpdateDesiredCount)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "this spec needs a settled three-node control plane")
		Expect(test.nke.WaitForJoinedControlPlaneNodes(ctx, test.cluster.ID, controlPlaneControllerUpdateDesiredCount)).To(Succeed(),
			"all three control-plane nodes must report Joined for this to test the floor it bypasses")

		cpNodes := expectSucceed(test.nke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true))
		Expect(cpNodes).To(HaveLen(controlPlaneControllerUpdateDesiredCount))
		victim := cpNodes[0]

		By("Confirming the delete is refused while the node is healthy")
		message := expectSucceed(test.nke.DeleteNodeExpectingRefusal(ctx, victim.ID))
		Expect(message).To(ContainSubstring("below the minimum of 3"),
			"a healthy node must still be held by the floor")

		By(fmt.Sprintf("Stopping kubelet on %s so the node is genuinely unreachable", victim.Name))
		container := expectSucceed(nodeContainerName(ctx, victim.Name))
		Expect(runDockerCommand(ctx, "exec", container, "systemctl", "stop", "kubelet")).To(Succeed(),
			"stopping kubelet is how this spec makes the node unreachable")

		By("Waiting for CWE to observe the node as no longer ready")
		Eventually(func(ctx context.Context) error {
			state, err := test.nke.ControlPlaneDeleteStateAt(ctx, test.cluster.ID)
			if err != nil {
				return err
			}
			if state.ReadyNodeCount != controlPlaneControllerUpdateDesiredCount-1 {
				return fmt.Errorf("control plane reports %d nodes ready; want %d after stopping one kubelet",
					state.ReadyNodeCount, controlPlaneControllerUpdateDesiredCount-1)
			}
			return nil
		}).WithContext(ctx).WithTimeout(10*time.Minute).WithPolling(5*time.Second).
			Should(Succeed(), "CWE should notice the node stopped being ready")

		By("Deleting the same node now that it is unreachable")
		Expect(test.nke.DeleteNode(ctx, victim.ID)).To(Succeed(),
			"the delete refused a moment ago must be accepted once the node is not serving")

		By("Waiting for the teardown to finish and the node to be gone")
		Eventually(func(ctx context.Context) error {
			ids, err := test.nke.ControlPlaneNodeIDs(ctx, test.cluster.ID)
			if err != nil {
				return err
			}
			if _, present := ids[victim.ID]; present {
				return fmt.Errorf("control-plane node %s (%s) is still present", victim.ID, victim.Name)
			}
			return nil
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "an accepted delete must actually tear the node down, not stall")

		By("Waiting for the controller to replace it and return to three healthy nodes")
		Eventually(func(ctx context.Context) error {
			state, err := test.nke.ControlPlaneDeleteStateAt(ctx, test.cluster.ID)
			if err != nil {
				return err
			}
			if state.InFlight() {
				return fmt.Errorf("a control-plane delete is still in flight: %d deleting, %d requested",
					state.Deleting, state.DeleteRequested)
			}
			if len(state.JoinedNodeIDs) != controlPlaneControllerUpdateDesiredCount {
				return fmt.Errorf("control plane has %d joined nodes; want %d",
					len(state.JoinedNodeIDs), controlPlaneControllerUpdateDesiredCount)
			}
			if state.ReadyNodeCount != controlPlaneControllerUpdateDesiredCount {
				return fmt.Errorf("control plane has %d nodes KubernetesReady; want %d",
					state.ReadyNodeCount, controlPlaneControllerUpdateDesiredCount)
			}
			return nil
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(10*time.Second).
			Should(Succeed(), "the control plane should recover to three healthy nodes on its own")

		By("Asserting the tenant cluster reports Ready for the recovered control plane")
		assertControlPlaneNodesReadyInTenant(ctx, test, controlPlaneControllerUpdateDesiredCount)
	}, Label("cp-controller", "cp-delete"), NodeTimeout(60*time.Minute))
}
