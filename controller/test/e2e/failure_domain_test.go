// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"maps"
	"math"
	"os/exec"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/mgmtns"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

// controlPlaneEligibleDomains counts the advertised domains that can host
// control-plane Machines.
func controlPlaneEligibleDomains(spec nvke.ClusterSpec) int {
	eligible := 0
	for _, domain := range spec.CAPDFailureDomains {
		if domain.ControlPlane {
			eligible++
		}
	}
	return eligible
}

// expectedControlPlaneFailureDomainCounts models placements starting from an
// empty control plane, using the allocator's lexicographic tie-break.
func expectedControlPlaneFailureDomainCounts(spec nvke.ClusterSpec, desired int) map[string]int {
	var names []string
	for _, domain := range spec.CAPDFailureDomains {
		if domain.ControlPlane {
			names = append(names, domain.Name)
		}
	}
	slices.Sort(names)
	counts := make(map[string]int)
	if len(names) == 0 {
		return counts
	}
	for i := range desired {
		counts[names[i%len(names)]]++
	}
	return counts
}

func validateCAPDFailureDomainReuse(spec nvke.ClusterSpec, controllerManaged bool, reuseCluster string) error {
	if spec.Backend == nvke.BackendCAPD && controllerManaged && reuseCluster != "" {
		return fmt.Errorf("lifecycle.reuseCluster is unsupported for CAPD control-plane failure-domain fixtures; each row requires its own advertised domains")
	}
	return nil
}

// assertCAPDControlPlaneFailureDomains validates settled placement and returns
// the node-to-domain snapshot. No-op on non-CAPD backends.
func assertCAPDControlPlaneFailureDomains(ctx context.Context, test *tenantTest, desired int, want map[string]int) map[string]string {
	GinkgoHelper()
	if test.spec.Backend != nvke.BackendCAPD {
		return nil
	}
	var domains map[string]string
	Eventually(func(ctx context.Context) error {
		var err error
		domains, err = capdControlPlaneFailureDomainsAt(ctx, test, desired, want)
		return err
	}).WithContext(ctx).WithTimeout(2*time.Minute).WithPolling(5*time.Second).
		Should(Succeed(), "CAPI control-plane Machines should be balanced across eligible failure domains")
	return domains
}

func capdControlPlaneFailureDomainsAt(ctx context.Context, test *tenantTest, desired int, want map[string]int) (map[string]string, error) {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"})
	if err := test.k8sClient.Get(ctx, client.ObjectKey{Namespace: mgmtns.Namespace(), Name: test.cluster.Name}, cluster); err != nil {
		return nil, fmt.Errorf("getting CAPI Cluster: %w", err)
	}
	published, found, err := unstructured.NestedSlice(cluster.Object, "status", "failureDomains")
	if err != nil {
		return nil, fmt.Errorf("reading CAPI Cluster failure domains: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("CAPI Cluster has not published failure domains")
	}
	wantPublished := make(map[string]bool, len(test.spec.CAPDFailureDomains))
	for _, domain := range test.spec.CAPDFailureDomains {
		wantPublished[domain.Name] = domain.ControlPlane
	}
	eligible := controlPlaneEligibleDomains(test.spec)
	gotPublished := make(map[string]bool, len(published))
	for _, item := range published {
		domain, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("CAPI Cluster published malformed failure domain %T", item)
		}
		name, _ := domain["name"].(string)
		controlPlane, _ := domain["controlPlane"].(bool)
		if name == "" {
			return nil, fmt.Errorf("CAPI Cluster published an unnamed failure domain")
		}
		gotPublished[name] = controlPlane
	}
	if !maps.Equal(gotPublished, wantPublished) {
		return nil, fmt.Errorf("CAPI Cluster published failure domains %v; want %v", gotPublished, wantPublished)
	}

	nodes, err := test.nvke.ListClusterNodesByControlPlane(ctx, test.cluster.ID, true)
	if err != nil {
		return nil, err
	}
	if len(nodes) != desired {
		return nil, fmt.Errorf("CWE reports %d control-plane nodes; want %d", len(nodes), desired)
	}
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineList"})
	if err = test.k8sClient.List(ctx, machines, client.InNamespace(mgmtns.Namespace()),
		client.MatchingLabels{"cluster.x-k8s.io/cluster-name": test.cluster.Name}); err != nil {
		return nil, fmt.Errorf("listing CAPI Machines: %w", err)
	}
	byName := make(map[string]unstructured.Unstructured, len(machines.Items))
	for _, machine := range machines.Items {
		byName[machine.GetName()] = machine
	}
	containers, err := capdContainerFailureDomains(ctx)
	if err != nil {
		return nil, err
	}
	placements := make(map[string]string, len(nodes))
	got := make(map[string]int, len(want))
	for _, node := range nodes {
		machine, ok := byName[node.Name]
		if !ok {
			return nil, fmt.Errorf("CWE control-plane node %q has no CAPI Machine", node.Name)
		}
		requested, err := capdMachineDomainAt(machine, gotPublished, containers)
		if err != nil {
			return nil, err
		}
		if reported := ptr.Deref(node.FailureDomain, ""); reported != requested {
			return nil, fmt.Errorf("CWE reports node %q in failure domain %q; want %q", node.Name, reported, requested)
		}
		placements[node.Name] = requested
		if requested != "" {
			got[requested]++
		}
	}
	if want != nil && !maps.Equal(got, want) {
		return nil, fmt.Errorf("control-plane Machine placements %v; want %v", got, want)
	}
	if err := cweFailureDomainStatusAt(ctx, test, got, gotPublished); err != nil {
		return nil, err
	}
	if eligible == 0 {
		return placements, nil
	}
	fewest, most := math.MaxInt, 0
	for domain, canHostControlPlane := range wantPublished {
		if canHostControlPlane {
			fewest = min(fewest, got[domain])
			most = max(most, got[domain])
		}
	}
	if most-fewest > 1 {
		return nil, fmt.Errorf("control-plane Machine placements are unbalanced: %v", got)
	}
	return placements, nil
}

// cweFailureDomainStatusAt checks that the control-plane status reports the
// placement CAPD shows: per-domain node counts, and the spread condition. A
// domain holding a node is reported even when it is not published.
func cweFailureDomainStatusAt(ctx context.Context, test *tenantTest, got map[string]int, published map[string]bool) error {
	cp, err := test.nvke.GetClusterControlPlane(ctx, test.cluster.ID)
	if err != nil {
		return err
	}
	wantCounts := maps.Clone(got)
	for name, controlPlane := range published {
		if controlPlane {
			wantCounts[name] = got[name]
		}
	}
	gotCounts := make(map[string]int)
	for _, domain := range ptr.Deref(cp.Status.FailureDomains, nil) {
		gotCounts[domain.Name] = domain.NodeCount
	}
	if !maps.Equal(gotCounts, wantCounts) {
		return fmt.Errorf("CWE reports control-plane nodes per failure domain %v; want %v", gotCounts, wantCounts)
	}
	wantSpread := api.ConditionStatusUnknown
	if len(wantCounts) > 0 {
		wantSpread = api.ConditionStatusTrue
		for _, n := range got {
			if n > 1 {
				wantSpread = api.ConditionStatusFalse
			}
		}
	}
	spread := cp.Status.FailureDomainSpread
	if spread == nil || spread.Status != wantSpread {
		return fmt.Errorf("CWE reports failureDomainSpread %+v; want status %s", spread, wantSpread)
	}
	return nil
}

// capdContainerFailureDomains snapshots running containers in one Docker call.
func capdContainerFailureDomains(ctx context.Context) (map[string]string, error) {
	command := exec.CommandContext(ctx, "docker", "ps", "--format",
		`{{.Names}}\t{{.Label "io.x-k8s.cluster.failureDomain"}}`)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, commandError(command, err, output)
	}
	domains := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSuffix(string(output), "\n"), "\n") {
		if line == "" {
			continue
		}
		name, domain, found := strings.Cut(line, "\t")
		if !found || name == "" {
			return nil, fmt.Errorf("malformed Docker container failure-domain row %q", line)
		}
		domains[name] = domain
	}
	return domains, nil
}

func capdMachineDomainAt(machine unstructured.Unstructured, published map[string]bool, containers map[string]string) (string, error) {
	name := machine.GetName()
	if _, ok := machine.GetLabels()["cluster.x-k8s.io/control-plane"]; !ok {
		return "", fmt.Errorf("CAPI Machine %q lacks the control-plane label", name)
	}
	requested, _, err := unstructured.NestedString(machine.Object, "spec", "failureDomain")
	if err != nil {
		return "", fmt.Errorf("reading CAPI Machine %q requested domain: %w", name, err)
	}
	observed, _, err := unstructured.NestedString(machine.Object, "status", "failureDomain")
	if err != nil {
		return "", fmt.Errorf("reading CAPI Machine %q observed domain: %w", name, err)
	}
	actual, found := containers[name]
	if !found {
		return "", fmt.Errorf("CAPI Machine %q has no running Docker container", name)
	}
	eligible := false
	for _, controlPlane := range published {
		eligible = eligible || controlPlane
	}
	if (requested == "" && eligible) || (requested != "" && !published[requested]) {
		return "", fmt.Errorf("CAPI Machine %q requested ineligible domain %q", name, requested)
	}
	if actual != requested {
		return "", fmt.Errorf("CAPI Machine %q requested domain %q, but its Docker container has domain %q", name, requested, actual)
	}
	if observed != "" && observed != actual {
		return "", fmt.Errorf("CAPI Machine %q reports observed domain %q, but its Docker container has domain %q", name, observed, actual)
	}
	return actual, nil
}
