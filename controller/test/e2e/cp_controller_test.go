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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

const (
	// controlPlaneTestLabelKey is the label the control-plane-controller entries create the control plane with.
	// It sits outside the domains the controller reserves.
	controlPlaneTestLabelKey = "example.com/cp-controller-e2e"
	// controlPlaneRoleLabelKey is set by kubeadm when a node joins as a control plane, not by the request.
	controlPlaneRoleLabelKey = "node-role.kubernetes.io/control-plane"
)

// validateControllerManagedControlPlane asserts a cluster provisioned from the
// cluster-level controlPlaneConfig (control-plane controller, no control-plane
// node pool) has no such pool and brings the desired control-plane nodes to Ready
// in both the controller and the tenant API.
func validateControllerManagedControlPlane(test *tenantTest) {
	It("manages the control plane from the cluster-level config without a control-plane node pool", func(ctx context.Context) {
		Expect(test.spec.ControlPlane.Config).NotTo(BeNil(),
			"the control-plane-controller spec must set a cluster-level control plane config")
		desired := test.spec.ControlPlane.Config.DesiredCount

		By("Verifying the cluster has no control-plane node pool")
		pools := expectSucceed(test.nvke.ListNodePools(ctx, test.cluster.ID))
		for _, pool := range pools {
			Expect(pool.ControlPlane).To(BeFalse(),
				"a control-plane-controller cluster must have no control-plane node pool (found %q)", pool.Name)
		}

		By("Waiting for the desired cluster-level control-plane nodes to become KubernetesReady")
		Eventually(func(ctx context.Context) error {
			return test.nvke.AssertControlPlaneNodesReady(ctx, test.cluster.ID, desired)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "cluster-level control-plane nodes should become ready")

		By("Asserting the tenant cluster reports Ready for the controller-managed control-plane nodes")
		assertControlPlaneNodesReadyInTenant(ctx, test, desired)
		want := expectedControlPlaneFailureDomainCounts(test.spec, desired)
		By("Verifying the control-plane Machines requested the expected failure domains")
		assertCAPDControlPlaneFailureDomains(ctx, test, desired, want)
	}, Label("cp-controller", "readiness"), NodeTimeout(90*time.Minute))
}

// controlPlaneControllerUpdateDesiredCount is the desired control-plane node
// count the update spec scales to. It is an odd value so the scaled control
// plane keeps a valid etcd quorum (the entries create the cluster with a single
// control-plane node, so this exercises a 1 -> 3 scale-up).
const controlPlaneControllerUpdateDesiredCount = 3

// validateControllerManagedControlPlaneUpdate PATCHes the cluster-level
// controlPlaneConfig desiredCount and waits for the control plane to converge on
// the new count in the controller and the tenant API. The controller-managed control plane
// has no node pool, so it can't be scaled through the node pool API.
func validateControllerManagedControlPlaneUpdate(test *tenantTest) {
	It("scales the controller-managed control plane to a new desired count", func(ctx context.Context) {
		Expect(test.spec.ControlPlane.Config).NotTo(BeNil(),
			"the control-plane-controller spec must set a cluster-level control plane config")
		desired := controlPlaneControllerUpdateDesiredCount

		By(fmt.Sprintf("Updating the cluster-level control-plane desired count to %d", desired))
		expectSucceed(test.nvke.UpdateControlPlaneDesiredCount(ctx, test.cluster.ID, desired))

		By("Waiting for the updated control-plane node count to become KubernetesReady in the controller")
		Eventually(func(ctx context.Context) error {
			return test.nvke.AssertControlPlaneNodesReady(ctx, test.cluster.ID, desired)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "control plane should scale to the new desired count in the controller")

		By("Asserting the tenant cluster reports Ready for the updated control-plane nodes")
		assertControlPlaneNodesReadyInTenant(ctx, test, desired)
		By("Verifying scale-up balanced CAPI Machines across the eligible failure domains")
		assertCAPDControlPlaneFailureDomains(ctx, test, desired, nil)
	}, Label("cp-controller", "update"), NodeTimeout(90*time.Minute))
}

// controlPlaneControllerRollKubernetesVersion is the version the rolling update
// spec upgrades a CAPI control plane to, and the version the no-VIP spec asks for
// and expects refused. The control-plane controller treats only
// kubernetesVersion, release and configProfile as replacing fields, and the
// latter two are Nico-only, so the version is the sole roll trigger a CAPI caller
// can reach. It must stay within the minor of the CAPD node image's base
// (KINDEST_NODE_TAG in hack/capd-kindest-node/Dockerfile), because that base
// supplies the kubeadm that has to serve the target.
const controlPlaneControllerRollKubernetesVersion = "1.36.4"

// validateControllerManagedControlPlaneRollingUpdate bumps the control-plane
// config revision and asserts the controller rolls the old nodes onto new ones at
// the same desired count, Ready in both the controller and the tenant API (mirrors the
// node-pool baseline in upgrade_test.go, but via the cluster-level config
// revision). The field it bumps depends on the provider, since the replacing
// fields differ. Runs after the desiredCount update so quorum survives a
// one-at-a-time roll.
func validateControllerManagedControlPlaneRollingUpdate(test *tenantTest) {
	It("rolls the controller-managed control plane on a control-plane config revision", func(ctx context.Context) {
		Expect(test.spec.ControlPlane.Config).NotTo(BeNil(),
			"the control-plane-controller spec must set a cluster-level control plane config")

		By("Capturing the current controller-managed control-plane node set")
		oldNodeIDs := expectSucceed(test.nvke.ControlPlaneNodeIDs(ctx, test.cluster.ID))
		desired := len(oldNodeIDs)
		Expect(desired).To(BeNumerically(">=", 3),
			"a config-revision roll needs a multi-node control plane to keep etcd quorum while rolling")

		if test.spec.Backend == nvke.BackendNicoVMS {
			By("Changing the control-plane config profile to bump the configuration revision")
			// Forge requires a real config profile; nico-vms-cp is what create uses.
			expectSucceed(test.nvke.UpdateControlPlaneConfigProfile(ctx, test.cluster.ID, nicoVMSWConfigProfile))
		} else {
			By("Upgrading the control-plane Kubernetes version to bump the configuration revision")
			expectSucceed(test.nvke.UpgradeClusterControlPlane(ctx, test.cluster.ID, controlPlaneControllerRollKubernetesVersion))
		}

		By("Waiting for every old control-plane node to be replaced and KubernetesReady in the controller")
		// A config-revision roll replaces the control plane one node at a time to
		// keep etcd quorum, so it costs one provision window per node, not the
		// single-create Provision the other waits use. Budget for the serial
		// replacement; the NodeTimeout below is sized to contain it.
		rollBudget := time.Duration(desired) * test.timeouts.Provision
		Expect(test.nvke.WaitForControlPlaneNodesRolled(ctx, test.cluster.ID, desired, oldNodeIDs, rollBudget)).To(Succeed(),
			"control plane should roll onto new nodes at the same desired count")

		By("Asserting the tenant cluster reports Ready for the rolled control-plane nodes")
		assertControlPlaneNodesReadyInTenant(ctx, test, desired)
		By("Verifying replacement CAPI Machines retained balanced failure-domain placement")
		assertCAPDControlPlaneFailureDomains(ctx, test, desired, nil)
	}, Label("cp-controller", "rolling-update"), NodeTimeout(2*time.Hour+30*time.Minute))
}

// assertControlPlaneNodesReadyInTenant builds an authenticated tenant client and
// eventually asserts that the controller-managed control-plane nodes the controller reports
// (NodePoolID == uuid.Nil) are present and Ready in the tenant Kubernetes API. It
// mirrors validateNodeReadinessPostCreate's tenant-side node-list pattern, but
// keys off the controller-reported control-plane node names rather than node pools,
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
	names, err := test.nvke.ControlPlaneNodeNames(ctx, test.cluster.ID)
	if err != nil {
		return err
	}
	if len(names) != desired {
		return fmt.Errorf("controller reports %d controller-managed control-plane nodes; want %d", len(names), desired)
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
		By("Confirming the control plane is at the three-node fault-tolerant floor")
		Eventually(func(ctx context.Context) error {
			return test.nvke.AssertControlPlaneNodesReady(ctx, test.cluster.ID, controlPlaneControllerUpdateDesiredCount)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "this spec needs a settled three-node control plane")

		// Assert on Joined as well, so the refusal below is known to come from the
		// three-to-two boundary rather than from an incidentally smaller joined set.
		Expect(test.nvke.WaitForJoinedControlPlaneNodes(ctx, test.cluster.ID, controlPlaneControllerUpdateDesiredCount)).To(Succeed(),
			"all three control-plane nodes must report Joined for this to test the floor it claims to")

		victim := expectSucceed(test.nvke.ControlPlaneNodeIDs(ctx, test.cluster.ID))
		Expect(victim).To(HaveLen(controlPlaneControllerUpdateDesiredCount))
		var target uuid.UUID
		for id := range victim {
			target = id
			break
		}

		By("Deleting a control-plane node and expecting the quorum floor to refuse it")
		message := expectSucceed(test.nvke.DeleteNodeExpectingRefusal(ctx, target))
		Expect(message).To(ContainSubstring("below the minimum of 3"),
			"the refusal should name the fault-tolerant floor it is protecting")

		By("Verifying the refused delete left no trace on the control plane")
		Consistently(func(ctx context.Context) error {
			state, err := test.nvke.ControlPlaneDeleteStateAt(ctx, test.cluster.ID)
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
		By(fmt.Sprintf("Growing the control plane to %d so the delete clears the quorum floor", controlPlaneControllerDeleteSurgeCount))
		expectSucceed(test.nvke.UpdateControlPlaneDesiredCount(ctx, test.cluster.ID, controlPlaneControllerDeleteSurgeCount))
		Eventually(func(ctx context.Context) error {
			return test.nvke.AssertControlPlaneNodesReady(ctx, test.cluster.ID, controlPlaneControllerDeleteSurgeCount)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "control plane should grow before the delete")

		// Ready is not enough: the quorum floor counts Joined, which the
		// ProvisionNode workflow writes after the tenant node-status controller has
		// already marked the node KubernetesReady. Deleting inside that window is
		// refused for a control plane that is in fact large enough.
		By("Waiting for every grown control-plane node to report Joined, which is what the quorum floor counts")
		Expect(test.nvke.WaitForJoinedControlPlaneNodes(ctx, test.cluster.ID, controlPlaneControllerDeleteSurgeCount)).To(Succeed(),
			"the quorum floor counts joined nodes, so the delete must not race the Joined write")
		By("Verifying the four-node control plane is balanced before scale-down")
		assertCAPDControlPlaneFailureDomains(ctx, test, controlPlaneControllerDeleteSurgeCount, nil)

		// Lowering the desired count would otherwise start automatic scale-down,
		// which races this spec's own delete for the one node above the floor.
		By("Disabling automatic control-plane scale-down so this spec's delete is the only one")
		cluster := expectSucceed(test.nvke.GetCluster(ctx, test.cluster.ID))
		Expect(cluster.ControlPlaneConfig).NotTo(BeNil(), "the cluster should report its control-plane config")
		originalAutoScaleDownDisabled := ptr.Deref(cluster.ControlPlaneConfig.AutoScaleDownDisabled, false)
		// Registered first so a PATCH that commits but loses its response is still
		// undone; restoring the original value is harmless if it never applied.
		DeferCleanup(func(ctx context.Context) {
			expectSucceed(test.nvke.SetControlPlaneAutoScaleDownDisabled(ctx, test.cluster.ID, originalAutoScaleDownDisabled))
		}, NodeTimeout(time.Minute))
		expectSucceed(test.nvke.SetControlPlaneAutoScaleDownDisabled(ctx, test.cluster.ID, true))

		By("Lowering the desired count so the delete shrinks the control plane instead of replacing the node")
		expectSucceed(test.nvke.UpdateControlPlaneDesiredCount(ctx, test.cluster.ID, controlPlaneControllerUpdateDesiredCount))

		before := expectSucceed(test.nvke.ControlPlaneDeleteStateAt(ctx, test.cluster.ID))
		Expect(before.NodeIDs).To(HaveLen(controlPlaneControllerDeleteSurgeCount))
		Expect(before.InFlight()).To(BeFalse(), "no control-plane delete should be in flight before this spec starts one")
		var target uuid.UUID
		for id := range before.JoinedNodeIDs {
			target = id
			break
		}
		Expect(target).NotTo(Equal(uuid.Nil), "the control plane should have a joined node to delete")

		By("Deleting one control-plane node")
		Expect(test.nvke.DeleteNode(ctx, target)).To(Succeed(),
			"a delete leaving three joined control-plane nodes should be accepted")

		By("Watching the delete run: one teardown at a time, no joins while it runs, and visible in the cluster status")
		observedDeleteInStatus := false
		joinedWatermark := before.JoinedNodeIDs
		Eventually(func(ctx context.Context) error {
			state, err := test.nvke.ControlPlaneDeleteStateAt(ctx, test.cluster.ID)
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
		By("Verifying the three-node control plane remains balanced after scale-down")
		assertCAPDControlPlaneFailureDomains(ctx, test, controlPlaneControllerUpdateDesiredCount, nil)
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
		By("Confirming the control plane is settled at the three-node fault-tolerant floor")
		Eventually(func(ctx context.Context) error {
			return test.nvke.AssertControlPlaneNodesReady(ctx, test.cluster.ID, controlPlaneControllerUpdateDesiredCount)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "this spec needs a settled three-node control plane")
		Expect(test.nvke.WaitForJoinedControlPlaneNodes(ctx, test.cluster.ID, controlPlaneControllerUpdateDesiredCount)).To(Succeed(),
			"all three control-plane nodes must report Joined for this to test the floor it bypasses")

		cpNodes := expectSucceed(test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true))
		Expect(cpNodes).To(HaveLen(controlPlaneControllerUpdateDesiredCount))
		victim := cpNodes[0]
		var victimDomain string
		originalNames := make(map[string]struct{}, len(cpNodes))
		if test.spec.Backend == nvke.BackendCAPD {
			By("Selecting the Machine in the less-used failure domain for deletion")
			domainsByNode := assertCAPDControlPlaneFailureDomains(ctx, test, controlPlaneControllerUpdateDesiredCount, nil)
			Expect(controlPlaneEligibleDomains(test.spec)).To(Equal(2),
				"the replacement-domain check requires exactly two eligible domains")
			counts := make(map[string]int)
			for _, node := range cpNodes {
				originalNames[node.Name] = struct{}{}
				domain := domainsByNode[node.Name]
				counts[domain]++
			}
			for _, node := range cpNodes {
				if counts[domainsByNode[node.Name]] == 1 {
					victim = node
					victimDomain = domainsByNode[node.Name]
					break
				}
			}
			Expect(victimDomain).NotTo(BeEmpty(), "a balanced three-node control plane should have one Machine in a less-used domain")
		}

		By("Confirming the delete is refused while the node is healthy")
		message := expectSucceed(test.nvke.DeleteNodeExpectingRefusal(ctx, victim.ID))
		Expect(message).To(ContainSubstring("below the minimum of 3"),
			"a healthy node must still be held by the floor")

		By(fmt.Sprintf("Stopping kubelet on %s so the node is genuinely unreachable", victim.Name))
		container := expectSucceed(nodeContainerName(ctx, victim.Name))
		Expect(runDockerCommand(ctx, "exec", container, "systemctl", "stop", "kubelet")).To(Succeed(),
			"stopping kubelet is how this spec makes the node unreachable")

		By("Waiting for the controller to observe the node as no longer ready")
		Eventually(func(ctx context.Context) error {
			state, err := test.nvke.ControlPlaneDeleteStateAt(ctx, test.cluster.ID)
			if err != nil {
				return err
			}
			if state.ReadyNodeCount != controlPlaneControllerUpdateDesiredCount-1 {
				return fmt.Errorf("control plane reports %d nodes ready; want %d after stopping one kubelet",
					state.ReadyNodeCount, controlPlaneControllerUpdateDesiredCount-1)
			}
			return nil
		}).WithContext(ctx).WithTimeout(10*time.Minute).WithPolling(5*time.Second).
			Should(Succeed(), "the controller should notice the node stopped being ready")

		By("Deleting the same node now that it is unreachable")
		Expect(test.nvke.DeleteNode(ctx, victim.ID)).To(Succeed(),
			"the delete refused a moment ago must be accepted once the node is not serving")

		By("Waiting for the teardown to finish and the node to be gone")
		Eventually(func(ctx context.Context) error {
			ids, err := test.nvke.ControlPlaneNodeIDs(ctx, test.cluster.ID)
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
			state, err := test.nvke.ControlPlaneDeleteStateAt(ctx, test.cluster.ID)
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
		if test.spec.Backend == nvke.BackendCAPD {
			By("Verifying the replacement refilled the deleted Machine's failure domain")
			names := expectSucceed(test.nvke.ControlPlaneNodeNames(ctx, test.cluster.ID))
			var replacementNames []string
			for _, name := range names {
				if _, existed := originalNames[name]; !existed {
					replacementNames = append(replacementNames, name)
				}
			}
			Expect(replacementNames).To(HaveLen(1), "the deleted control-plane node should have exactly one replacement")
			replacementDomains := assertCAPDControlPlaneFailureDomains(ctx, test, controlPlaneControllerUpdateDesiredCount, nil)
			replacementDomain := replacementDomains[replacementNames[0]]
			Expect(replacementDomain).To(Equal(victimDomain), "the replacement should refill the deleted Machine's domain")
		}
	}, Label("cp-controller", "cp-delete"), NodeTimeout(60*time.Minute))
}

// validateControllerManagedControlPlaneAutoScaleDown grows the control plane to
// four, then lowers the desired count back to three and asserts the controller
// sheds exactly one surplus node on its own -- with no explicit node delete --
// through the same serialized teardown path: at most one teardown in flight, no
// control-plane join while the shed runs, the cluster-level status reporting the
// shed while it runs, and the surviving three all drawn from the original four
// (a shed, not a replacement).
//
// This is the automatic counterpart to
// validateControllerManagedControlPlaneSerializedDelete, which turns auto
// scale-down off and drives the shrink with an explicit delete; here auto
// scale-down itself is the actor, so the drop in desiredCount is the only input.
// Growing first is what makes the shrink legal under the quorum floor.
func validateControllerManagedControlPlaneAutoScaleDown(test *tenantTest) {
	It("sheds a surplus control-plane node automatically when the desired count drops", func(ctx context.Context) {
		By(fmt.Sprintf("Growing the control plane to %d so the drop to %d leaves one surplus node to shed",
			controlPlaneControllerDeleteSurgeCount, controlPlaneControllerUpdateDesiredCount))
		expectSucceed(test.nvke.UpdateControlPlaneDesiredCount(ctx, test.cluster.ID, controlPlaneControllerDeleteSurgeCount))
		Eventually(func(ctx context.Context) error {
			return test.nvke.AssertControlPlaneNodesReady(ctx, test.cluster.ID, controlPlaneControllerDeleteSurgeCount)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "control plane should grow before the automatic scale-down")

		// Ready is not enough: the quorum floor counts Joined, which the
		// ProvisionNode workflow writes after the tenant node-status controller has
		// already marked the node KubernetesReady. Dropping the desired count before
		// the surge node is Joined would let the floor refuse the shed.
		By("Waiting for every grown control-plane node to report Joined, which is what the quorum floor counts")
		Expect(test.nvke.WaitForJoinedControlPlaneNodes(ctx, test.cluster.ID, controlPlaneControllerDeleteSurgeCount)).To(Succeed(),
			"the quorum floor counts joined nodes, so the scale-down must not race the Joined write")
		By("Verifying the four-node control plane is balanced before scale-down")
		assertCAPDControlPlaneFailureDomains(ctx, test, controlPlaneControllerDeleteSurgeCount, nil)

		// Auto scale-down is on by default (AutoScaleDownDisabled unset), but set it
		// explicitly so this spec does not depend on the cluster's created default
		// and reads as its own precondition. The specs share one cluster, so record
		// and restore the original value rather than leaving it flipped for the next.
		By("Ensuring automatic control-plane scale-down is enabled")
		cluster := expectSucceed(test.nvke.GetCluster(ctx, test.cluster.ID))
		Expect(cluster.ControlPlaneConfig).NotTo(BeNil(), "the cluster should report its control-plane config")
		originalAutoScaleDownDisabled := ptr.Deref(cluster.ControlPlaneConfig.AutoScaleDownDisabled, false)
		DeferCleanup(func(ctx context.Context) {
			expectSucceed(test.nvke.SetControlPlaneAutoScaleDownDisabled(ctx, test.cluster.ID, originalAutoScaleDownDisabled))
		}, NodeTimeout(time.Minute))
		expectSucceed(test.nvke.SetControlPlaneAutoScaleDownDisabled(ctx, test.cluster.ID, false))

		before := expectSucceed(test.nvke.ControlPlaneDeleteStateAt(ctx, test.cluster.ID))
		Expect(before.NodeIDs).To(HaveLen(controlPlaneControllerDeleteSurgeCount))
		Expect(before.InFlight()).To(BeFalse(), "no control-plane delete should be in flight before this spec lowers the count")

		By(fmt.Sprintf("Lowering the desired count to %d so automatic scale-down sheds the surplus node",
			controlPlaneControllerUpdateDesiredCount))
		expectSucceed(test.nvke.UpdateControlPlaneDesiredCount(ctx, test.cluster.ID, controlPlaneControllerUpdateDesiredCount))

		By("Watching the controller shed one node on its own: one teardown at a time, no joins while it runs, and visible in the cluster status")
		observedShedInStatus := false
		joinedWatermark := before.JoinedNodeIDs
		Eventually(func(ctx context.Context) error {
			state, err := test.nvke.ControlPlaneDeleteStateAt(ctx, test.cluster.ID)
			if err != nil {
				return err
			}

			// Serialization: automatic scale-down starts one teardown at a time.
			Expect(state.Deleting).To(BeNumerically("<=", 1),
				"at most one control-plane teardown may be in flight at a time")

			if state.InFlight() {
				// Observability: the shed must be visible at the cluster level.
				observedShedInStatus = true

				// Join gate: while a delete is in flight the joined set must not grow.
				for id := range state.JoinedNodeIDs {
					if _, known := joinedWatermark[id]; !known {
						return StopTrying(fmt.Sprintf(
							"control-plane node %s joined while an automatic scale-down was in flight", id,
						))
					}
				}
				return fmt.Errorf("automatic control-plane scale-down still in flight: %d deleting, %d requested",
					state.Deleting, state.DeleteRequested)
			}

			// Once nothing is in flight the joined set may legitimately grow again.
			joinedWatermark = state.JoinedNodeIDs

			if len(state.NodeIDs) != controlPlaneControllerUpdateDesiredCount {
				return fmt.Errorf("control plane has %d nodes; want %d",
					len(state.NodeIDs), controlPlaneControllerUpdateDesiredCount)
			}
			// A shed removes a node rather than replacing it: every survivor must be
			// one of the original four, with no fresh node joined in its place.
			for id := range state.NodeIDs {
				if _, original := before.NodeIDs[id]; !original {
					return StopTrying(fmt.Sprintf(
						"control-plane node %s is new; automatic scale-down must shed a node, not replace one", id,
					))
				}
			}
			if state.ReadyNodeCount != controlPlaneControllerUpdateDesiredCount {
				return fmt.Errorf("control plane has %d nodes KubernetesReady; want %d",
					state.ReadyNodeCount, controlPlaneControllerUpdateDesiredCount)
			}
			return nil
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(2*time.Second).
			Should(Succeed(), "the control plane should settle at three nodes with one original node shed")

		Expect(observedShedInStatus).To(BeTrue(),
			"the cluster control-plane status should have reported the in-progress scale-down at least once")

		By("Asserting the tenant cluster reports Ready for the surviving control-plane nodes")
		assertControlPlaneNodesReadyInTenant(ctx, test, controlPlaneControllerUpdateDesiredCount)
		By("Verifying the three-node control plane remains balanced after scale-down")
		assertCAPDControlPlaneFailureDomains(ctx, test, controlPlaneControllerUpdateDesiredCount, nil)
	}, Label("cp-controller", "cp-scaledown"), NodeTimeout(75*time.Minute))
}

// validateControllerManagedControlPlaneRebalance drives one steady-state
// instance-type rebalance round and asserts the controller converges placement
// toward preference without ever breaking quorum: it puts a more-preferred
// instance type at the front of the control-plane preference, then watches the
// controller surge one more-preferred replacement, join it, and shed a
// less-preferred node through the serialized delete path, leaving the control
// plane at its desired count with the surge demoted to an ordinary member.
//
// It surges a VM that has to boot and join before the old node sheds, so it is
// heavy; it carries the "rebalance" label, which is in no CI lane, so it runs
// only when selected explicitly.
//
// Rebalance ranks nodes by the NICo provider's instanceTypePreference, which
// only the Forge-native (nico-vms) control-plane provider carries; the CAPI
// provider the container backends use has no ranker, so this is a nico-vms spec.
// It is bounded to a single round by disabling rebalancing once the surge is in
// flight: an in-flight surge still resolves (it joins, then sheds) when
// rebalancing is turned off, but no further surge starts -- which also exercises
// that resolve-when-disabled path. A full convergence would repeat the round
// once per remaining less-preferred node.
func validateControllerManagedControlPlaneRebalance(test *tenantTest) {
	It("rebalances the controller-managed control plane onto a more-preferred instance type", func(ctx context.Context) {
		if test.spec.Backend != nvke.BackendNicoVMS {
			Skip("control-plane rebalance ranks on the NICo instanceTypePreference, which only the nico-vms control-plane provider carries")
		}
		if nicoVMSLargeInstanceID == nicoVMSPreferredInstanceID {
			Skip("on macOS the large instance type is already the preferred 8 GiB one, so there is no more-preferred type to rebalance onto")
		}

		desired := controlPlaneControllerUpdateDesiredCount

		By("Confirming the control plane is settled at the desired count, all Joined on the current instance type")
		Eventually(func(ctx context.Context) error {
			return test.nvke.AssertControlPlaneNodesReady(ctx, test.cluster.ID, desired)
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "this spec needs a settled control plane before it rebalances")
		Expect(test.nvke.WaitForJoinedControlPlaneNodes(ctx, test.cluster.ID, desired)).To(Succeed(),
			"the control plane must be fully joined before the rebalance, since the shed depends on the quorum floor counting joined nodes")

		originalNodes := expectSucceed(test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true))
		Expect(originalNodes).To(HaveLen(desired))
		originalIDs := make(map[uuid.UUID]struct{}, len(originalNodes))
		for _, node := range originalNodes {
			originalIDs[node.ID] = struct{}{}
			Expect(nodeNicoInstanceType(node)).To(Equal(nicoVMSLargeInstanceID),
				"the control plane should start on the large instance type before the preference changes")
		}

		By("Putting a more-preferred instance type at the front of the control-plane preference and enabling rebalance")
		preference := []uuid.UUID{uuid.MustParse(nicoVMSPreferredInstanceID), uuid.MustParse(nicoVMSLargeInstanceID)}
		expectSucceed(test.nvke.EnableControlPlaneRebalanceWithPreference(ctx, test.cluster.ID, preference))

		By("Waiting for the controller to surge one more-preferred replacement above the desired count")
		// Detect the surge by the extra control-plane node the round adds, not by
		// catching the controlPlanePreferredReplacement marker: that marker is set
		// only while the surge is in flight and clears the instant it joins, a
		// window far too narrow to poll reliably. The marker clearing and the
		// final instance-type split are asserted in the convergence watch below;
		// here we only assert the marker's placement invariant -- an original node
		// must never be flagged as the surge.
		var surgeID uuid.UUID
		Eventually(func(ctx context.Context) error {
			cpNodes, err := test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true)
			if err != nil {
				return err
			}
			for _, node := range cpNodes {
				if _, original := originalIDs[node.ID]; original {
					if isControlPlaneRebalanceSurge(node) {
						return StopTrying(fmt.Sprintf("original control-plane node %s is marked as a rebalance surge", node.ID))
					}
					continue
				}
				surgeID = node.ID
				return nil
			}
			return fmt.Errorf("no control-plane rebalance surge yet (%d control-plane nodes)", len(cpNodes))
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "the controller should surge a more-preferred replacement when a joined node is below preference")

		By("Disabling rebalance so only this round runs; the in-flight surge still resolves")
		expectSucceed(test.nvke.DisableControlPlaneRebalance(ctx, test.cluster.ID))

		By("Watching the surge join and the less-preferred node shed, one delete at a time, back to the desired count")
		Eventually(func(ctx context.Context) error {
			state, err := test.nvke.ControlPlaneDeleteStateAt(ctx, test.cluster.ID)
			if err != nil {
				return err
			}
			// Serialization: the shed is the only teardown, and it runs one at a time.
			Expect(state.Deleting).To(BeNumerically("<=", 1),
				"at most one control-plane teardown may be in flight at a time")
			if state.InFlight() {
				return fmt.Errorf("rebalance shed still in flight: %d deleting, %d requested", state.Deleting, state.DeleteRequested)
			}

			cpNodes, err := test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true)
			if err != nil {
				return err
			}
			if len(cpNodes) != desired {
				return fmt.Errorf("control plane has %d nodes; want %d", len(cpNodes), desired)
			}
			var preferred, large, marked, joinedReady int
			surgeStillPresent := false
			for _, node := range cpNodes {
				switch nodeNicoInstanceType(node) {
				case nicoVMSPreferredInstanceID:
					preferred++
				case nicoVMSLargeInstanceID:
					large++
				}
				if isControlPlaneRebalanceSurge(node) {
					marked++
				}
				if node.Conditions.Joined.Status == api.ConditionStatusTrue && node.Conditions.KubernetesReady.Status == api.ConditionStatusTrue {
					joinedReady++
				}
				if node.ID == surgeID {
					surgeStillPresent = true
				}
			}
			if !surgeStillPresent {
				return StopTrying(fmt.Sprintf("the surge node %s is gone; a round must shed the less-preferred node, not the surge", surgeID))
			}
			if marked != 0 {
				return fmt.Errorf("%d control-plane nodes still carry the rebalance marker; it should clear once the surge is an ordinary member", marked)
			}
			if joinedReady != desired {
				return fmt.Errorf("%d control-plane nodes are Joined and KubernetesReady; want %d", joinedReady, desired)
			}
			if preferred != 1 || large != desired-1 {
				return fmt.Errorf("instance-type split is %d preferred / %d large; want 1 preferred / %d large after one round", preferred, large, desired-1)
			}
			return nil
		}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
			Should(Succeed(), "one rebalance round should leave the control plane at the desired count with a single node moved to the preferred type")

		By("Asserting the tenant cluster reports Ready for the rebalanced control plane")
		assertControlPlaneNodesReadyInTenant(ctx, test, desired)
	}, Label("cp-controller", "rebalance"), NodeTimeout(45*time.Minute))
}

// nodeNicoInstanceType returns the node's provisioned NICo instance-type ID, or
// the empty string when the node carries no NICo placement yet.
func nodeNicoInstanceType(node api.Node) string {
	if node.Nico == nil {
		return ""
	}
	return node.Nico.InstanceTypeID.String()
}

// isControlPlaneRebalanceSurge reports whether the node carries the rebalance
// surge marker (controlPlanePreferredReplacement=True), set while the controller
// is moving the control plane onto a more-preferred instance type and cleared
// once the surge becomes an ordinary member.
func isControlPlaneRebalanceSurge(node api.Node) bool {
	cond := node.Conditions.ControlPlanePreferredReplacement
	return cond != nil && cond.Status == api.ConditionStatusTrue
}
