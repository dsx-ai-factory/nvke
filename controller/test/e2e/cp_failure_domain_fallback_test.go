// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/mgmtns"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

const (
	failureDomainFallbackEnabledKey     = "controlPlaneFailureDomainFallback.enabled"
	failureDomainFallbackGracePeriodKey = "controlPlaneFailureDomainFallback.gracePeriod"
	// failureDomainLabelKey is the NICo Machine label nico-sim publishes domains under.
	failureDomainLabelKey = "failure_domain"
	// fullFailureDomain sorts first, so the allocator places the first control-plane node in it.
	fullFailureDomain = "fd-a"
)

var fallbackFailureDomains = []string{fullFailureDomain, "fd-b", "fd-c"}

// nicoSimFailureDomains is the body of nico-sim's /sim/failure-domains.
type nicoSimFailureDomains struct {
	LabelKey    string   `json:"labelKey"`
	Domains     []string `json:"domains"`
	Unavailable []string `json:"unavailable"`
}

var _ = Describe("control-plane failure domain fallback", Ordered, Label("nico-containers", "failure-domain-fallback"), func() {
	var (
		test             tenantTest
		failed           bool
		deleteAccepted   bool
		previousSettings map[string]any
		settingsWritten  bool
		domainsSet       bool
	)

	AfterEach(func() { failed = failed || CurrentSpecReport().Failed() })

	BeforeAll(func(ctx context.Context) {
		test = expectSucceed(setupTenantTest(ctx, expectSucceed(configuredManagement()),
			nvke.Options{Provision: 20 * time.Minute, Delete: 10 * time.Minute, Poll: 5 * time.Second}))
		if test.cfg.Lifecycle.ReuseCluster != "" {
			Skip("the fallback needs a cluster created with a full failure domain")
		}

		By("Enabling the control-plane failure domain fallback with a one-minute grace period")
		var err error
		previousSettings, settingsWritten, err = nvke.SetControllerSettings(ctx, test.k8sClient, map[string]any{
			failureDomainFallbackEnabledKey:     true,
			failureDomainFallbackGracePeriodKey: "1m",
		})
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Publishing NICo failure domains %v with %s out of capacity", fallbackFailureDomains, fullFailureDomain))
		Expect(setNicoSimFailureDomains(ctx, test.k8sClient, nicoSimFailureDomains{
			LabelKey:    failureDomainLabelKey,
			Domains:     fallbackFailureDomains,
			Unavailable: []string{fullFailureDomain},
		})).To(Succeed())
		domainsSet = true
	}, NodeTimeout(15*time.Minute))

	It("moves a control-plane node out of a full failure domain and provisions it", func(ctx context.Context) {
		test.spec = nicoSpec(test.cfg, clusterNamePrefix, clusterShape{controlPlaneConfig: &nvke.ControlPlaneConfigSpec{DesiredCount: 1}})
		test.spec.Name = fmt.Sprintf("%s-%s-fd-fallback", test.spec.Name, test.spec.Backend)
		test.spec.NicoFailureDomainLabelKey = failureDomainLabelKey
		if preferred := preferredControlPlaneVIP(test.spec.Backend); preferred != "" {
			test.spec.ControlPlane.ControlPlaneVIP = expectSucceed(test.availableControlPlaneVIP(ctx, preferred))
		}

		By("Creating the tenant cluster")
		test.cluster = expectSucceed(test.nvke.CreateCluster(ctx, test.spec))

		By(fmt.Sprintf("Waiting for the control-plane Machine to be placed in %s and refused for capacity", fullFailureDomain))
		var pinned *unstructured.Unstructured
		Eventually(func(ctx context.Context) error {
			machine, err := fallbackControlPlaneMachine(ctx, test.k8sClient, test.cluster.Name)
			if err != nil {
				return err
			}
			if requested := machineFailureDomain(machine, "spec"); requested != fullFailureDomain {
				return fmt.Errorf("control-plane Machine %q requests failure domain %q; want %q", machine.GetName(), requested, fullFailureDomain)
			}
			if err := expectFailureDomainUnavailable(ctx, test.k8sClient, machine); err != nil {
				return err
			}
			pinned = machine
			return nil
		}).WithContext(ctx).WithTimeout(10 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		By("Waiting for the controller to move the same Machine to another failure domain")
		Eventually(func(ctx context.Context) error {
			machine, err := getMachine(ctx, test.k8sClient, pinned.GetName())
			if err != nil {
				return err
			}
			if machine.GetUID() != pinned.GetUID() {
				return fmt.Errorf("control-plane Machine %q was replaced rather than moved", pinned.GetName())
			}
			requested := machineFailureDomain(machine, "spec")
			if requested == fullFailureDomain || !slices.Contains(fallbackFailureDomains, requested) {
				return fmt.Errorf("control-plane Machine %q requests failure domain %q; want one of %v other than %s",
					machine.GetName(), requested, fallbackFailureDomains, fullFailureDomain)
			}
			return nil
		}).WithContext(ctx).WithTimeout(10 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		By("Waiting for the tenant cluster to be provisioned")
		ready, err := test.nvke.WaitForClusterReady(ctx, test.cluster.ID)
		if err != nil {
			test.collectDiagnostics(context.WithoutCancel(ctx), test.cluster.ID)
		}
		Expect(err).NotTo(HaveOccurred(), "should provision the cluster")
		test.cluster = ready
		Expect(test.nvke.AssertControlPlaneNodesReady(ctx, test.cluster.ID, 1)).To(Succeed())

		By("Verifying the Machine reports the failure domain it was provisioned in")
		Eventually(func(ctx context.Context) error {
			machine, err := getMachine(ctx, test.k8sClient, pinned.GetName())
			if err != nil {
				return err
			}
			requested, observed := machineFailureDomain(machine, "spec"), machineFailureDomain(machine, "status")
			if observed != requested {
				return fmt.Errorf("control-plane Machine %q requested %q but reports %q", machine.GetName(), requested, observed)
			}
			return nil
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	}, NodeTimeout(45*time.Minute))

	AfterAll(func(ctx context.Context) {
		defer func() {
			if settingsWritten {
				_, _, err := nvke.SetControllerSettings(ctx, test.k8sClient, previousSettings)
				Expect(err).NotTo(HaveOccurred())
			}
		}()
		defer func() {
			if domainsSet {
				Expect(setNicoSimFailureDomains(ctx, test.k8sClient, nicoSimFailureDomains{})).To(Succeed())
			}
		}()
		keep := test.cfg.Lifecycle.SkipDelete || (test.cfg.Lifecycle.PreserveOnFailure && failed)
		if test.cluster != nil && !keep {
			test.deleteClusterAndWait(ctx, &deleteAccepted)
		}
	}, NodeTimeout(20*time.Minute))
})

// setNicoSimFailureDomains configures nico-sim's failure-domain layout through
// the endpoint and token CAPNICo reads from capnico-system/nico-credentials.
func setNicoSimFailureDomains(ctx context.Context, k8sClient client.Client, domains nicoSimFailureDomains) error {
	var secret corev1.Secret
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "capnico-system", Name: "nico-credentials"}, &secret); err != nil {
		return fmt.Errorf("reading the NICo credentials: %w", err)
	}
	body, err := json.Marshal(domains)
	if err != nil {
		return err
	}
	endpoint := strings.TrimSuffix(string(secret.Data["endpoint"]), "/") + "/sim/failure-domains"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(secret.Data["token"]))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("configuring nico-sim failure domains: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("configuring nico-sim failure domains at %s: %s: %s", endpoint, resp.Status, detail)
	}
	return nil
}

var (
	capiMachineGVK = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}
	nicoMachineGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Kind: "NicoMachine"}
)

func fallbackControlPlaneMachine(ctx context.Context, k8sClient client.Client, clusterName string) (*unstructured.Unstructured, error) {
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(capiMachineGVK.GroupVersion().WithKind("MachineList"))
	if err := k8sClient.List(ctx, machines, client.InNamespace(mgmtns.Namespace()),
		client.MatchingLabels{"cluster.x-k8s.io/cluster-name": clusterName},
		client.HasLabels{"cluster.x-k8s.io/control-plane"}); err != nil {
		return nil, fmt.Errorf("listing CAPI control-plane Machines: %w", err)
	}
	if len(machines.Items) != 1 {
		return nil, fmt.Errorf("cluster %s has %d control-plane Machines; want 1", clusterName, len(machines.Items))
	}
	return &machines.Items[0], nil
}

func getMachine(ctx context.Context, k8sClient client.Client, name string) (*unstructured.Unstructured, error) {
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(capiMachineGVK)
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: mgmtns.Namespace(), Name: name}, machine); err != nil {
		return nil, fmt.Errorf("getting CAPI Machine %q: %w", name, err)
	}
	return machine, nil
}

// machineFailureDomain reads failureDomain from the Machine's spec or status.
func machineFailureDomain(machine *unstructured.Unstructured, section string) string {
	domain, _, _ := unstructured.NestedString(machine.Object, section, "failureDomain")
	return domain
}

// expectFailureDomainUnavailable checks that the Machine's NicoMachine reports
// MachineProvisioned=False with reason FailureDomainUnavailable.
func expectFailureDomainUnavailable(ctx context.Context, k8sClient client.Client, machine *unstructured.Unstructured) error {
	name, _, _ := unstructured.NestedString(machine.Object, "spec", "infrastructureRef", "name")
	if name == "" {
		return fmt.Errorf("CAPI Machine %q has no infrastructureRef yet", machine.GetName())
	}
	nicoMachine := &unstructured.Unstructured{}
	nicoMachine.SetGroupVersionKind(nicoMachineGVK)
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: mgmtns.Namespace(), Name: name}, nicoMachine); err != nil {
		return fmt.Errorf("getting NicoMachine %q: %w", name, err)
	}
	conditions, _, _ := unstructured.NestedSlice(nicoMachine.Object, "status", "conditions")
	for _, item := range conditions {
		condition, _ := item.(map[string]any)
		if condition["type"] != "MachineProvisioned" {
			continue
		}
		if condition["status"] == "False" && condition["reason"] == "FailureDomainUnavailable" {
			return nil
		}
		return fmt.Errorf("NicoMachine %q MachineProvisioned is %v/%v: %v", name, condition["status"], condition["reason"], condition["message"])
	}
	return fmt.Errorf("NicoMachine %q reports no MachineProvisioned condition", name)
}
