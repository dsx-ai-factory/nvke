//go:build integration_test

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/api"
	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

//go:embed testdata/cluster.yaml
var clusterFixture []byte

//go:embed testdata/bindings.json
var bindingFixture []byte

const pollInterval = 2 * time.Second

var _ = Describe("NVKECluster through the local NVKE controller API", func() {
	It("persists cluster intent, dispatches Temporal, and creates CAPI resources", func(ctx SpecContext) {
		env := loadEnvironment()
		var cluster internalv1.NVKECluster
		Expect(yaml.UnmarshalStrict(clusterFixture, &cluster)).To(Succeed())
		var bindings []struct {
			Template api.CreateClusterRequest `json:"template"`
		}
		Expect(json.Unmarshal(bindingFixture, &bindings)).To(Succeed())
		Expect(bindings).To(HaveLen(1))
		expected := bindings[0].Template

		By("checking that the deployed binding uses paused local Docker infrastructure")
		var configured corev1.ConfigMap
		Expect(env.host.Get(ctx, client.ObjectKey{
			Namespace: "management-engine-system", Name: "management-controller-bindings",
		}, &configured)).To(Succeed())
		Expect(configured.Data["bindings.json"]).To(MatchJSON(bindingFixture))
		Expect(expected.Nico).To(BeNil())
		Expect(expected.Infra).NotTo(BeNil())
		Expect(expected.Infra.Cluster).To(HaveKeyWithValue("apiVersion", "infrastructure.cluster.x-k8s.io/v1beta2"))
		Expect(expected.Infra.Cluster).To(HaveKeyWithValue("kind", "DockerCluster"))
		Expect(expected.ControlPlaneConfig).NotTo(BeNil())
		Expect(expected.ControlPlaneConfig.Nico).To(BeNil())
		Expect(expected.ControlPlaneConfig.Infra).NotTo(BeNil())
		Expect(expected.ControlPlaneConfig.Infra.MachineTemplate).To(HaveKeyWithValue("apiVersion", "infrastructure.cluster.x-k8s.io/v1beta2"))
		Expect(expected.ControlPlaneConfig.Infra.MachineTemplate).To(HaveKeyWithValue("kind", "DockerMachineTemplate"))
		annotations, _, err := unstructured.NestedStringMap(expected.ControlPlaneConfig.Infra.MachineTemplate,
			"spec", "template", "metadata", "annotations")
		Expect(err).NotTo(HaveOccurred())
		Expect(annotations).To(HaveKeyWithValue(clusterv1.PausedAnnotation, "true"))

		By("creating an NVKECluster in the state cluster")
		Expect(env.state.Create(ctx, &cluster)).To(Succeed())
		remoteName := "nke-" + string(cluster.UID)
		evidence := integrationEvidence{
			Namespace: cluster.Namespace, Name: cluster.Name, UID: string(cluster.UID), RemoteName: remoteName,
			TemporalNamespace: env.temporalNamespace,
		}
		saveEvidence(env.resultsDir, evidence)
		GinkgoWriter.Printf("NVKECluster %s/%s; controller name %s\n", cluster.Namespace, cluster.Name, remoteName)

		By("observing the independent NVKE controller API representation")
		var observedCluster *api.Cluster
		Eventually(ctx, func(g Gomega) {
			response, err := env.controller.ListClustersWithResponse(ctx, &api.ListClustersParams{Name: &remoteName})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(response.StatusCode()).To(Equal(http.StatusOK))
			g.Expect(response.JSON200).NotTo(BeNil())
			g.Expect(response.JSON200.Clusters).To(HaveLen(1))
			observedCluster = &response.JSON200.Clusters[0]
		}).WithTimeout(2 * time.Minute).WithPolling(pollInterval).Should(Succeed())
		Expect(observedCluster.Name).To(Equal(remoteName))
		Expect(observedCluster.ControlPlaneKubernetesVersion).To(Equal("1.36.1"))
		Expect(observedCluster.CniPlugin).To(Equal("ovn-kubernetes"))
		Expect(observedCluster.EtcdEncrypted).To(BeTrue())
		Expect(observedCluster.Network).To(Equal(api.ClusterNetwork{PodCIDR: "192.168.0.0/16", ServiceCIDR: "10.96.0.0/12"}))
		Expect(observedCluster.Nico).To(BeNil())
		Expect(observedCluster.Infra).To(Equal(expected.Infra))
		Expect(observedCluster.ControlPlaneConfig).NotTo(BeNil())
		Expect(observedCluster.ControlPlaneConfig.DesiredCount).To(Equal(1))
		Expect(observedCluster.ControlPlaneConfig.Infra).To(Equal(expected.ControlPlaneConfig.Infra))
		evidence.RemoteID = observedCluster.ID.String()
		evidence.WorkflowID = "provision-cluster-" + evidence.RemoteID
		saveEvidence(env.resultsDir, evidence)

		By("observing the controller's persisted cluster identity and current status")
		Eventually(ctx, func(g Gomega) {
			g.Expect(env.state.Get(ctx, client.ObjectKeyFromObject(&cluster), &cluster)).To(Succeed())
			g.Expect(cluster.Status.Provisioning).NotTo(BeNil())
			g.Expect(cluster.Status.Provisioning.ID).To(Equal(evidence.RemoteID))
			g.Expect(cluster.Status.Provisioning.Generation).To(Equal(cluster.Generation))
			g.Expect(cluster.Status.ObservedGeneration).To(Equal(cluster.Generation))
			condition := meta.FindStatusCondition(cluster.Status.Conditions, internalv1.ControlPlaneReadyCondition)
			g.Expect(condition).NotTo(BeNil())
			g.Expect(condition.Reason).To(Equal("Provisioning"))
		}).WithTimeout(time.Minute).WithPolling(pollInterval).Should(Succeed())

		By("verifying the real Temporal input and successful CAPI apply activity")
		Eventually(ctx, func() error {
			return env.verifyWorkflow(ctx, evidence.WorkflowID, evidence.RemoteID)
		}).WithTimeout(3 * time.Minute).WithPolling(pollInterval).Should(Succeed())
		evidence.CAPIActivityCompleted = true
		saveEvidence(env.resultsDir, evidence)

		By("checking the CAPI Cluster and Docker infrastructure references")
		capiKey := client.ObjectKey{Namespace: "nvke-system", Name: remoteName}
		var capiCluster clusterv1.Cluster
		Expect(env.host.Get(ctx, capiKey, &capiCluster)).To(Succeed())
		Expect(capiCluster.Spec.InfrastructureRef).To(Equal(clusterv1.ContractVersionedObjectReference{
			APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "DockerCluster", Name: remoteName,
		}))
		infra := infrastructureObject("DockerCluster")
		Eventually(ctx, func(g Gomega) {
			g.Expect(env.host.Get(ctx, capiKey, infra)).To(Succeed())
			g.Expect(metav1.IsControlledBy(infra, &capiCluster)).To(BeTrue())
		}).WithTimeout(time.Minute).WithPolling(pollInterval).Should(Succeed())

		By("checking that the control-plane machine remains paused before infrastructure provisioning")
		Eventually(ctx, func(g Gomega) {
			machine, err := env.pausedControlPlaneMachine(ctx, remoteName)
			g.Expect(err).NotTo(HaveOccurred())
			evidence.MachineName = machine.GetName()
		}).WithTimeout(3 * time.Minute).WithPolling(pollInterval).Should(Succeed())
		evidence.Passed = true
		saveEvidence(env.resultsDir, evidence)
		GinkgoWriter.Printf("Verified controller %s -> Temporal %s -> CAPI %s/%s; machine %s is paused.\n",
			evidence.RemoteID, evidence.WorkflowID, capiKey.Namespace, capiKey.Name, evidence.MachineName)
		GinkgoWriter.Println("Resources remain available in Tilt; make dev-nvke-controller-down removes the dedicated environment.")
	}, SpecTimeout(10*time.Minute))
})

func (e environment) pausedControlPlaneMachine(ctx context.Context, clusterName string) (*unstructured.Unstructured, error) {
	var machines clusterv1.MachineList
	if err := e.host.List(ctx, &machines, client.InNamespace("nvke-system"),
		client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName}); err != nil {
		return nil, fmt.Errorf("list CAPI machines: %w", err)
	}
	if len(machines.Items) != 1 {
		return nil, fmt.Errorf("expected one control-plane machine, found %d", len(machines.Items))
	}
	machine := &machines.Items[0]
	if machine.Spec.InfrastructureRef.APIGroup != "infrastructure.cluster.x-k8s.io" ||
		machine.Spec.InfrastructureRef.Kind != "DockerMachine" || machine.Spec.ProviderID != "" {
		return nil, fmt.Errorf("expected an unprovisioned Docker-backed CAPI machine")
	}
	if machine.Spec.ClusterName != clusterName || machine.Spec.Version != "v1.36.1" {
		return nil, fmt.Errorf("CAPI machine must reference the expected cluster and Kubernetes version")
	}
	infra := infrastructureObject("DockerMachine")
	if err := e.host.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace, Name: machine.Spec.InfrastructureRef.Name,
	}, infra); err != nil {
		return nil, fmt.Errorf("get DockerMachine: %w", err)
	}
	if infra.GetAnnotations()[clusterv1.PausedAnnotation] != "true" {
		return nil, fmt.Errorf("DockerMachine is missing its provisioning pause")
	}
	providerID, _, err := unstructured.NestedString(infra.Object, "spec", "providerID")
	if err != nil || providerID != "" {
		return nil, fmt.Errorf("DockerMachine must not have provisioned infrastructure")
	}
	return infra, nil
}

func infrastructureObject(kind string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta2")
	obj.SetKind(kind)
	return obj
}

type integrationEvidence struct {
	Namespace             string `json:"namespace"`
	Name                  string `json:"name"`
	UID                   string `json:"uid"`
	RemoteName            string `json:"remoteName"`
	RemoteID              string `json:"remoteID,omitempty"`
	WorkflowID            string `json:"workflowID,omitempty"`
	TemporalNamespace     string `json:"temporalNamespace"`
	CAPIActivityCompleted bool   `json:"capiActivityCompleted"`
	MachineName           string `json:"machineName,omitempty"`
	Passed                bool   `json:"passed"`
}

func saveEvidence(dir string, evidence integrationEvidence) {
	Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
	data, err := json.MarshalIndent(evidence, "", "  ")
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(filepath.Join(dir, evidence.UID+".json"), append(data, '\n'), 0o600)).To(Succeed())
}
