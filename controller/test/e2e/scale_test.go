// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/sync/errgroup"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/api"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/nke"
)

const drainProbeLabel = "e2e.nvidia.com/drain-probe"

// validateNodePoolScale scales each node pool up by one node and back down: the
// worker pool, then the control-plane pool when the control plane is one. A
// controller-managed control plane has no pool; the cp-controller entries scale
// it instead.
func validateNodePoolScale(test *tenantTest) {
	It("scales the node pools up and down", func(ctx context.Context) {
		pools := []api.NodePool{expectWorkerPool(ctx, test)}
		if test.spec.ControlPlane.Config == nil {
			pools = append(pools, expectControlPlanePool(ctx, test))
		}
		originalNodes := make([]map[openapi_types.UUID]struct{}, len(pools))
		for i, pool := range pools {
			originalNodes[i] = expectNodeIDs(ctx, test, pool.ID)
		}

		By("Incrementing the desired count of each node pool by 1")
		for _, pool := range pools {
			expectSucceed(test.nke.UpdateNodePoolDesiredCount(ctx, pool.ID, pool.DesiredNodeCount+1))
		}

		By("Waiting for each node pool to reach the new desired count")
		waitForPoolCountsInParallel(ctx, test, poolScales(pools, 1)...)
		newNodes := make([]api.Node, len(pools))
		newNodeNames := make([]string, len(pools))
		for i, pool := range pools {
			newNodes[i] = expectNewNode(ctx, test, pool.ID, originalNodes[i])
			newNodeNames[i] = newNodes[i].Name
		}
		validateOVNLifecycleTraffic(ctx, test, "scale-up", newNodeNames...)

		By("Restoring the original desired count of each node pool")
		for _, pool := range pools {
			expectSucceed(test.nke.UpdateNodePoolDesiredCount(ctx, pool.ID, pool.DesiredNodeCount))
		}

		// Scaling the desired count down by itself does not delete any nodes.
		workerNode := newNodes[0]
		if test.spec.Backend == nke.BackendCAPD {
			validateWorkerDrainAndDeletion(ctx, test, workerNode)
		} else {
			By("Deleting the scaled-up worker node")
			Expect(test.nke.DeleteNode(ctx, workerNode.ID)).To(Succeed(), "should delete the scaled-up worker node")
		}
		for _, controlPlaneNode := range newNodes[1:] {
			By("Deleting the scaled-up control-plane node")
			Expect(test.nke.DeleteNode(ctx, controlPlaneNode.ID)).To(Succeed(), "should delete the scaled-up control-plane node")
		}

		By("Waiting for each node pool to return to its original desired count")
		waitForPoolCountsInParallel(ctx, test, poolScales(pools, 0)...)
		validateOVNLifecycleTraffic(ctx, test, "scale-down")
	}, Label("lifecycle", "scale"), NodeTimeout(2*time.Hour))
}

// poolScales returns, for each pool, a target of its desired count plus delta.
func poolScales(pools []api.NodePool, delta int) []poolScale {
	scales := make([]poolScale, 0, len(pools))
	for _, pool := range pools {
		scales = append(scales, poolScale{pool: pool, desired: pool.DesiredNodeCount + delta})
	}
	return scales
}

func validateWorkerDrainAndDeletion(ctx context.Context, test *tenantTest, worker api.Node) {
	GinkgoHelper()

	adminClient := expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
	By("Waiting for the scaled-up worker to be Ready through CWE and the tenant API")
	Eventually(func(ctx context.Context) error {
		return assertNodeReadyForDrain(ctx, test, adminClient, worker)
	}).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())

	By("Placing a PDB-protected workload on the worker selected for scale-down")
	replicaSet, pdb, originalPodUID := createDrainProbe(ctx, adminClient, worker)
	deferDeleteDrainProbeObjects(ctx, adminClient, replicaSet, pdb)

	By("Deleting the scaled-up worker node")
	Expect(test.nke.DeleteNode(ctx, worker.ID)).To(Succeed(), "should delete the scaled-up worker node")

	By("Observing the drain cordon the worker before the PDB permits eviction")
	Eventually(func(ctx context.Context) error {
		return assertDrainBlockedByPDB(ctx, adminClient, worker.Name, originalPodUID, replicaSet.Spec.Selector.MatchLabels)
	}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())

	By("Waiting for the PDB-blocked drain to time out and force-delete the workload")
	Eventually(func(ctx context.Context) error {
		if err := adminClient.Get(ctx, client.ObjectKeyFromObject(pdb), pdb); err != nil {
			return fmt.Errorf("getting PDB while waiting for force drain: %w", err)
		}
		return assertDrainProbeMoved(ctx, adminClient, replicaSet, worker.Name, originalPodUID)
	}).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())

	By("Verifying Kubernetes node deletion removed the tenant Node object")
	Eventually(func(ctx context.Context) error {
		var node corev1.Node
		err := adminClient.Get(ctx, client.ObjectKey{Name: worker.Name}, &node)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("getting scaled-down tenant node %s: %w", worker.Name, err)
		}
		return fmt.Errorf("scaled-down tenant node %s still exists", worker.Name)
	}).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
}

func assertNodeReadyForDrain(ctx context.Context, test *tenantTest, tenantClient client.Client, expected api.Node) error {
	node, err := test.nke.GetNode(ctx, expected.ID)
	if err != nil {
		return fmt.Errorf("getting scaled-up CWE node %s: %w", expected.ID, err)
	}
	if node.Conditions.KubernetesReady.Status != api.ConditionStatusTrue {
		return fmt.Errorf("scaled-up CWE node %s is not KubernetesReady", expected.ID)
	}

	var tenantNode corev1.Node
	if err := tenantClient.Get(ctx, client.ObjectKey{Name: expected.Name}, &tenantNode); err != nil {
		return fmt.Errorf("getting scaled-up tenant Node %s: %w", expected.Name, err)
	}
	if ready, detail := kubernetesNodeReady(&tenantNode); !ready {
		return fmt.Errorf("scaled-up tenant Node %s is not Ready: %s", expected.Name, detail)
	}
	return nil
}

func createDrainProbe(ctx context.Context, tenantClient client.Client, worker api.Node) (*appsv1.ReplicaSet, *policyv1.PodDisruptionBudget, types.UID) {
	GinkgoHelper()

	probeValue := worker.ID.String()
	labels := map[string]string{drainProbeLabel: probeValue}
	minAvailable := intstr.FromInt32(1)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-drain-probe-",
			Namespace:    "default",
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: labels},
		},
	}
	Expect(tenantClient.Create(ctx, pdb)).To(Succeed())

	replicas := int32(1)
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-drain-probe-",
			Namespace:    "default",
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
								Key:      corev1.LabelHostname,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{worker.Name},
							}}}},
						},
					}},
					Containers: []corev1.Container{{
						Name:    "drain-probe",
						Image:   kube.KubectlImage,
						Command: []string{"sh", "-c", "sleep 3600"},
					}},
				},
			},
		},
	}
	Expect(tenantClient.Create(ctx, replicaSet)).To(Succeed())

	var originalPod corev1.Pod
	Eventually(func(ctx context.Context) error {
		var pods corev1.PodList
		if err := tenantClient.List(ctx, &pods, client.InNamespace("default"), client.MatchingLabels(labels)); err != nil {
			return err
		}
		if len(pods.Items) != 1 {
			return fmt.Errorf("found %d drain probe pods, want 1", len(pods.Items))
		}
		originalPod = pods.Items[0]
		if originalPod.Spec.NodeName != worker.Name {
			return fmt.Errorf("drain probe pod scheduled on %q, want %q", originalPod.Spec.NodeName, worker.Name)
		}
		if !podReady(&originalPod) {
			return fmt.Errorf("drain probe pod %s is not Ready", originalPod.Name)
		}
		return nil
	}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
	Eventually(func(ctx context.Context) error {
		if err := tenantClient.Get(ctx, client.ObjectKeyFromObject(pdb), pdb); err != nil {
			return err
		}
		if pdb.Status.CurrentHealthy != 1 || pdb.Status.DesiredHealthy != 1 || pdb.Status.DisruptionsAllowed != 0 {
			return fmt.Errorf("drain probe PDB is not blocking eviction: currentHealthy=%d desiredHealthy=%d disruptionsAllowed=%d",
				pdb.Status.CurrentHealthy, pdb.Status.DesiredHealthy, pdb.Status.DisruptionsAllowed)
		}
		return nil
	}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())

	// ReplicaSets do not roll existing Pods when their template changes. Remove
	// the hard node affinity so the replacement created after eviction can land
	// on any remaining schedulable worker.
	before := replicaSet.DeepCopy()
	replicaSet.Spec.Template.Spec.Affinity = nil
	Expect(tenantClient.Patch(ctx, replicaSet, client.MergeFrom(before))).To(Succeed())
	return replicaSet, pdb, originalPod.UID
}

func assertDrainBlockedByPDB(ctx context.Context, tenantClient client.Client, nodeName string, originalPodUID types.UID, labels map[string]string) error {
	var node corev1.Node
	if err := tenantClient.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return fmt.Errorf("getting worker while drain is PDB-blocked: %w", err)
	}
	if !node.Spec.Unschedulable {
		return fmt.Errorf("worker %s is not cordoned yet", nodeName)
	}

	var pods corev1.PodList
	if err := tenantClient.List(ctx, &pods, client.InNamespace("default"), client.MatchingLabels(labels)); err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if pod.UID == originalPodUID && pod.DeletionTimestamp == nil {
			return nil
		}
	}
	return fmt.Errorf("PDB-protected drain probe pod %s is no longer intact on worker %s", originalPodUID, nodeName)
}

func assertDrainProbeMoved(ctx context.Context, tenantClient client.Client, replicaSet *appsv1.ReplicaSet, oldNodeName string, originalPodUID types.UID) error {
	var pods corev1.PodList
	if err := tenantClient.List(ctx, &pods,
		client.InNamespace(replicaSet.Namespace),
		client.MatchingLabels(replicaSet.Spec.Selector.MatchLabels),
	); err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if pod.UID != originalPodUID && pod.Spec.NodeName != "" && pod.Spec.NodeName != oldNodeName && podReady(&pod) {
			return nil
		}
	}
	return fmt.Errorf("drain probe has not become Ready away from worker %s", oldNodeName)
}

func deferDeleteDrainProbeObjects(ctx context.Context, tenantClient client.Client, objects ...client.Object) {
	GinkgoHelper()
	DeferCleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		for _, object := range objects {
			_ = tenantClient.Delete(cleanupCtx, object)
		}
	})
}

type poolScale struct {
	pool    api.NodePool
	desired int
}

// expectWorkerPool returns the cluster's worker node pool, failing the spec when
// there is none. It works in either control-plane shape.
func expectWorkerPool(ctx context.Context, test *tenantTest) api.NodePool {
	GinkgoHelper()

	pools := expectSucceed(test.nke.ListNodePools(ctx, test.cluster.ID))

	var workerPool *api.NodePool
	for i := range pools {
		if !pools[i].ControlPlane {
			workerPool = &pools[i]
		}
	}
	Expect(workerPool).NotTo(BeNil(), "cluster should have a worker pool")
	return *workerPool
}

func expectNodeIDs(ctx context.Context, test *tenantTest, nodePoolID openapi_types.UUID) map[openapi_types.UUID]struct{} {
	GinkgoHelper()

	nodes, err := test.nke.ListNodePoolNodes(ctx, nodePoolID)
	Expect(err).NotTo(HaveOccurred(), "should list nodes")

	nodeIDs := make(map[openapi_types.UUID]struct{}, len(nodes))
	for _, node := range nodes {
		nodeIDs[node.ID] = struct{}{}
	}
	return nodeIDs
}

func expectNewNode(ctx context.Context, test *tenantTest, nodePoolID openapi_types.UUID, originalNodes map[openapi_types.UUID]struct{}) api.Node {
	GinkgoHelper()

	nodes, err := test.nke.ListNodePoolNodes(ctx, nodePoolID)
	Expect(err).NotTo(HaveOccurred(), "should list nodes")

	for _, node := range nodes {
		if _, existed := originalNodes[node.ID]; !existed {
			return node
		}
	}
	Fail(fmt.Sprintf("expected a new node in node pool %s", nodePoolID))
	return api.Node{}
}

func waitForPoolCountsInParallel(ctx context.Context, test *tenantTest, scales ...poolScale) {
	GinkgoHelper()

	group, groupCtx := errgroup.WithContext(ctx)
	for _, scale := range scales {
		group.Go(func() error {
			if err := test.nke.WaitForProvisionedNodeCount(groupCtx, scale.pool.ID, scale.desired); err != nil {
				return fmt.Errorf("waiting for node pool %s to have %d nodes: %w", scale.pool.Name, scale.desired, err)
			}
			return nil
		})
	}
	Expect(group.Wait()).To(Succeed())
}
