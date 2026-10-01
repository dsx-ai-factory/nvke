// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"maps"
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// assertCAPDControlPlaneFailureDomains checks the live CAPI placement contract
// after the control plane settles. CAPD publishes its spec domains to Cluster
// status, and the controller chooses a domain for each new CAPI Machine.
func assertCAPDControlPlaneFailureDomains(ctx context.Context, test *tenantTest, desired int, want map[string]int) {
	GinkgoHelper()
	Eventually(func(ctx context.Context) error {
		return capdControlPlaneFailureDomainsAt(ctx, test, desired, want)
	}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(5*time.Second).
		Should(Succeed(), "CAPI control-plane Machines should be balanced across eligible failure domains")
}

func capdControlPlaneFailureDomainsAt(ctx context.Context, test *tenantTest, desired int, want map[string]int) error {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"})
	if err := test.k8sClient.Get(ctx, client.ObjectKey{Namespace: "nke-system", Name: test.cluster.Name}, cluster); err != nil {
		return fmt.Errorf("getting CAPI Cluster: %w", err)
	}
	published, found, err := unstructured.NestedSlice(cluster.Object, "status", "failureDomains")
	if err != nil {
		return fmt.Errorf("reading CAPI Cluster failure domains: %w", err)
	}
	if !found {
		return fmt.Errorf("CAPI Cluster has not published failure domains")
	}
	wantPublished := make(map[string]bool, len(test.spec.CAPDFailureDomains))
	eligible := 0
	for _, domain := range test.spec.CAPDFailureDomains {
		wantPublished[domain.Name] = domain.ControlPlane
		if domain.ControlPlane {
			eligible++
		}
	}
	gotPublished := make(map[string]bool, len(published))
	for _, item := range published {
		domain, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("CAPI Cluster published malformed failure domain %T", item)
		}
		name, _ := domain["name"].(string)
		controlPlane, _ := domain["controlPlane"].(bool)
		if name == "" {
			return fmt.Errorf("CAPI Cluster published an unnamed failure domain")
		}
		gotPublished[name] = controlPlane
	}
	if !maps.Equal(gotPublished, wantPublished) {
		return fmt.Errorf("CAPI Cluster published failure domains %v; want %v", gotPublished, wantPublished)
	}

	names, err := test.nke.ControlPlaneNodeNames(ctx, test.cluster.ID)
	if err != nil {
		return err
	}
	if len(names) != desired {
		return fmt.Errorf("CWE reports %d control-plane nodes; want %d", len(names), desired)
	}
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineList"})
	if err := test.k8sClient.List(ctx, machines, client.InNamespace("nke-system"),
		client.MatchingLabels{"cluster.x-k8s.io/cluster-name": test.cluster.Name}); err != nil {
		return fmt.Errorf("listing CAPI Machines: %w", err)
	}
	byName := make(map[string]unstructured.Unstructured, len(machines.Items))
	for _, machine := range machines.Items {
		byName[machine.GetName()] = machine
	}
	got := make(map[string]int, len(want))
	for _, name := range names {
		machine, ok := byName[name]
		if !ok {
			return fmt.Errorf("CWE control-plane node %q has no CAPI Machine", name)
		}
		if _, ok := machine.GetLabels()["cluster.x-k8s.io/control-plane"]; !ok {
			return fmt.Errorf("CAPI Machine %q lacks the control-plane label", name)
		}
		requested, found, err := unstructured.NestedString(machine.Object, "spec", "failureDomain")
		if err != nil {
			return fmt.Errorf("reading CAPI Machine %q requested domain: %w", name, err)
		}
		if (eligible == 0 && found) || (eligible > 0 && (!found || requested == "")) {
			return fmt.Errorf("CAPI Machine %q requested domain %q (present: %t); eligible domains: %d", name, requested, found, eligible)
		}
		if requested != "" && !gotPublished[requested] {
			return fmt.Errorf("CAPI Machine %q was placed in ineligible domain %q", name, requested)
		}
		if requested != "" {
			got[requested]++
		}
	}
	if want != nil && !maps.Equal(got, want) {
		return fmt.Errorf("control-plane Machine placements %v; want %v", got, want)
	}
	if eligible == 0 {
		return nil
	}
	fewest, most := math.MaxInt, 0
	for domain, eligible := range wantPublished {
		if eligible {
			fewest = min(fewest, got[domain])
			most = max(most, got[domain])
		}
	}
	if most-fewest > 1 {
		return fmt.Errorf("control-plane Machine placements are unbalanced: %v", got)
	}
	return nil
}

func capdControlPlaneMachineDomainAt(ctx context.Context, test *tenantTest, name string) (string, error) {
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"})
	if err := test.k8sClient.Get(ctx, client.ObjectKey{Namespace: "nke-system", Name: name}, machine); err != nil {
		return "", fmt.Errorf("getting CAPI Machine %q: %w", name, err)
	}
	if _, ok := machine.GetLabels()["cluster.x-k8s.io/control-plane"]; !ok {
		return "", fmt.Errorf("CAPI Machine %q lacks the control-plane label", name)
	}
	requested, found, err := unstructured.NestedString(machine.Object, "spec", "failureDomain")
	if err != nil {
		return "", fmt.Errorf("reading CAPI Machine %q requested domain: %w", name, err)
	}
	if !found || requested == "" {
		return "", fmt.Errorf("CAPI Machine %q has no requested failure domain", name)
	}
	return requested, nil
}
