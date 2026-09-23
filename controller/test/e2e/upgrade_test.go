// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nke/controller/api"
)

const upgradeStartingKubernetesVersion = "1.34.2"

var upgradeKubernetesVersionChain = []string{"1.35.3", "1.36.0"}

// upgradeHops returns the leading part of the chain a run walks. NKE_E2E_UPGRADE_HOPS
// caps it; unset, zero or invalid means the whole chain. Each hop recycles every
// control-plane node, about 45 minutes on the nico-vms lane.
func upgradeHops() []string {
	n, err := strconv.Atoi(os.Getenv("NKE_E2E_UPGRADE_HOPS"))
	if err != nil || n <= 0 || n >= len(upgradeKubernetesVersionChain) {
		return upgradeKubernetesVersionChain
	}
	return upgradeKubernetesVersionChain[:n]
}

func validateK8sUpgrade(test *tenantTest) {
	for _, targetVersion := range upgradeHops() {
		It(fmt.Sprintf("upgrades the control plane Kubernetes version to %s", targetVersion),
			upgradeControlPlaneTo(test, targetVersion),
			Label("lifecycle", "upgrade"), NodeTimeout(2*time.Hour+15*time.Minute))
	}
}

func upgradeControlPlaneTo(test *tenantTest, targetVersion string) func(context.Context) {
	return func(ctx context.Context) {
		currentVersion := test.cluster.ControlPlaneKubernetesVersion
		Expect(currentVersion).NotTo(Equal(targetVersion), "upgrade validation requires the cluster to be below the target version")

		controlPlanePool, err := expectControlPlanePool(ctx, test)
		Expect(err).NotTo(HaveOccurred(), "should find the control-plane pool")

		originalDesiredCount := controlPlanePool.DesiredNodeCount
		originalControlPlaneNodes := expectNodeIDs(ctx, test, controlPlanePool.ID)

		By(fmt.Sprintf("Upgrading the control plane from %s to %s", currentVersion, targetVersion))
		upgraded, err := test.nke.UpgradeClusterControlPlane(ctx, test.cluster.ID, targetVersion)
		Expect(err).NotTo(HaveOccurred(), "should request the control-plane upgrade")
		Expect(upgraded.ControlPlaneKubernetesVersion).To(Equal(targetVersion))

		By("Observing the control-plane upgrade start")
		Eventually(func(ctx context.Context) bool {
			currentCluster, getErr := test.nke.GetCluster(ctx, test.cluster.ID)
			if getErr != nil {
				return false
			}
			return conditionIsTrue(currentCluster.Conditions.ControlPlaneUpgrading) || conditionIsTrue(currentCluster.Conditions.CpNodesRecycling)
		}).WithContext(ctx).WithTimeout(30*time.Minute).WithPolling(test.timeouts.Poll).
			Should(BeTrue(), "control-plane upgrade should enter an in-progress state")

		By("Waiting for the control-plane upgrade to complete")
		cluster, err := test.nke.WaitForControlPlaneUpgradeComplete(ctx, test.cluster.ID, targetVersion)
		Expect(err).NotTo(HaveOccurred(), "control-plane upgrade should complete")
		test.cluster = cluster

		By("Verifying recycled control-plane nodes were replaced")
		currentControlPlaneNodes, err := test.nke.ListNodePoolNodes(ctx, controlPlanePool.ID)
		Expect(err).NotTo(HaveOccurred(), "should list control-plane nodes after upgrade")
		Expect(currentControlPlaneNodes).To(HaveLen(originalDesiredCount), "control-plane pool should retain its desired node count")

		for _, node := range currentControlPlaneNodes {
			if _, existed := originalControlPlaneNodes[node.ID]; existed {
				Fail(fmt.Sprintf("control-plane node %s should have been recycled during upgrade", node.ID))
			}
			Expect(node.Conditions.Provisioned.Status).To(Equal(api.ConditionStatusTrue), "replacement control-plane node %s should be provisioned", node.Name)
			Expect(node.Conditions.KubernetesReady.Status).To(Equal(api.ConditionStatusTrue), "replacement control-plane node %s should be KubernetesReady", node.Name)
		}

		By("Verifying kubeadm-config records the target Kubernetes version after the replacement nodes are Ready")
		tenantClient := expectSucceed(test.tenantClients.GetClient(ctx, test.cluster.Name))
		Eventually(func(ctx context.Context) error {
			return assertKubeadmKubernetesVersion(ctx, tenantClient, targetVersion)
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
	}
}

func assertKubeadmKubernetesVersion(ctx context.Context, tenantClient client.Client, expected string) error {
	var configMap corev1.ConfigMap
	if err := tenantClient.Get(ctx, client.ObjectKey{Namespace: kubeSystemNamespace, Name: kubeadmConfigMapName}, &configMap); err != nil {
		return fmt.Errorf("getting kubeadm ConfigMap: %w", err)
	}
	clusterConfiguration := configMap.Data[kubeadmConfigDataKey]
	if strings.TrimSpace(clusterConfiguration) == "" {
		return fmt.Errorf("kubeadm ConfigMap is missing non-empty %q data", kubeadmConfigDataKey)
	}

	configJSON, err := yaml.YAMLToJSON([]byte(clusterConfiguration))
	if err != nil {
		return fmt.Errorf("converting kubeadm ClusterConfiguration to JSON: %w", err)
	}
	var config struct {
		KubernetesVersion string `json:"kubernetesVersion"`
	}
	if err := json.Unmarshal(configJSON, &config); err != nil {
		return fmt.Errorf("decoding kubeadm ClusterConfiguration: %w", err)
	}
	actual := strings.TrimPrefix(strings.TrimSpace(config.KubernetesVersion), "v")
	want := strings.TrimPrefix(strings.TrimSpace(expected), "v")
	if actual != want {
		return fmt.Errorf("kubeadm-config kubernetesVersion = %q, want %q", config.KubernetesVersion, expected)
	}
	return nil
}

func expectControlPlanePool(ctx context.Context, test *tenantTest) (api.NodePool, error) {
	GinkgoHelper()

	pools, err := test.nke.ListNodePools(ctx, test.cluster.ID)
	if err != nil {
		return api.NodePool{}, err
	}
	for i := range pools {
		if pools[i].ControlPlane {
			return pools[i], nil
		}
	}
	return api.NodePool{}, fmt.Errorf("cluster %s has no control-plane pool", test.cluster.ID)
}

func conditionIsTrue(condition *api.Condition) bool {
	return condition != nil && condition.Status == api.ConditionStatusTrue
}
