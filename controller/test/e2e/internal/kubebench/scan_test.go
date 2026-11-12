// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const controlPlaneLabel = "node-role.kubernetes.io/control-plane"

func testNode(name string, controlPlane bool) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if controlPlane {
		node.Labels = map[string]string{controlPlaneLabel: ""}
	}
	return node
}

// scanCluster is a fake cluster whose API server names generated objects and
// gives every pod podStatus as it is created.
func scanCluster(t *testing.T, podStatus corev1.PodStatus, nodes ...*corev1.Node) *fake.Clientset {
	t.Helper()
	objects := make([]runtime.Object, 0, len(nodes))
	for _, node := range nodes {
		objects = append(objects, node)
	}
	cs := fake.NewClientset(objects...)
	var generated atomic.Int32
	cs.PrependReactor("create", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj, ok := action.(k8stesting.CreateAction).GetObject().(metav1.Object)
		if ok && obj.GetName() == "" && obj.GetGenerateName() != "" {
			obj.SetName(fmt.Sprintf("%s%05d", obj.GetGenerateName(), generated.Add(1)))
		}
		if pod, isPod := obj.(*corev1.Pod); isPod {
			pod.Status = podStatus
		}
		return false, nil, nil
	})
	return cs
}

// runsPolicies reports whether a scan pod runs the policies target.
func runsPolicies(pod *corev1.Pod) bool {
	return strings.Contains(strings.Join(pod.Spec.Containers[0].Args, " "), "policies")
}

// reportLogs serves each pod a log ending in the fixture for what it runs:
// the policies pod's, or its node's role's.
func reportLogs(t *testing.T, cs *fake.Clientset) func(context.Context, string, string) ([]byte, error) {
	t.Helper()
	controlPlane := fixtureLine(t, "report_control_plane.json")
	worker := fixtureLine(t, "report_worker.json")
	policies := fixtureLine(t, "report_policies.json")
	return func(ctx context.Context, namespace, name string) ([]byte, error) {
		pod, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		node, err := cs.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		report := worker
		if _, ok := node.Labels[controlPlaneLabel]; ok {
			report = controlPlane
		}
		if runsPolicies(pod) {
			report = policies
		}
		return []byte("I1105 10:00:00.000000       1 util.go:1] starting\n" + report + "\n"), nil
	}
}

func testScanner(t *testing.T, cs *fake.Clientset) Scanner {
	t.Helper()
	files := syntheticBenchmark()
	server := benchmarkServer(t, files)
	return Scanner{
		Clientset:    cs,
		Timeout:      5 * time.Second,
		Poll:         10 * time.Millisecond,
		Logs:         reportLogs(t, cs),
		HTTPClient:   server.Client(),
		BenchmarkURL: server.URL + "/cfg/",
		Files:        checksums(files),
	}
}

func succeeded() corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodSucceeded}
}

// created returns the objects of one resource the scan created.
func created[T runtime.Object](cs *fake.Clientset, resource string) []T {
	var objects []T
	for _, action := range cs.Actions() {
		if action.GetVerb() != "create" || action.GetResource().Resource != resource {
			continue
		}
		if obj, ok := action.(k8stesting.CreateAction).GetObject().(T); ok {
			objects = append(objects, obj)
		}
	}
	return objects
}

func assertNothingLeft(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	selector := metav1.ListOptions{LabelSelector: PodLabel + "=" + PodLabelValue}
	pods, err := cs.CoreV1().Pods("default").List(t.Context(), selector)
	require.NoError(t, err)
	assert.Empty(t, pods.Items, "scan pods left behind")
	configMaps, err := cs.CoreV1().ConfigMaps("default").List(t.Context(), selector)
	require.NoError(t, err)
	assert.Empty(t, configMaps.Items, "benchmark ConfigMap left behind")
	serviceAccounts, err := cs.CoreV1().ServiceAccounts("default").List(t.Context(), selector)
	require.NoError(t, err)
	assert.Empty(t, serviceAccounts.Items, "policies service account left behind")
}

func controlIDs(report Report) []string {
	return ids(report.Controls, func(c Control) string { return c.ID })
}

func TestScanOnePodPerNode(t *testing.T) {
	t.Parallel()

	cs := scanCluster(t, succeeded(), testNode("cp-b", true), testNode("cp-a", true), testNode("w-1", false))
	reports, err := testScanner(t, cs).Scan(t.Context())
	require.NoError(t, err)

	roles := map[string]Role{}
	controls := map[string][]string{}
	for _, r := range reports {
		roles[r.Node] = r.Role
		controls[r.Node] = controlIDs(r.Report)
	}
	assert.Equal(t, map[string]Role{"cp-a": RoleControlPlane, "cp-b": RoleControlPlane, "w-1": RoleWorker}, roles)
	assert.Equal(t, map[string][]string{
		"cp-a": {"1", "4", "5"}, // the policies report belongs to the first control-plane node by name
		"cp-b": {"1", "4"},
		"w-1":  {"4"},
	}, controls)

	pods := created[*corev1.Pod](cs, "pods")
	require.Len(t, pods, 4, "the policies pod and one pod per node")
	var policiesNodes []string
	for _, pod := range pods {
		if runsPolicies(pod) {
			policiesNodes = append(policiesNodes, pod.Spec.NodeName)
			assert.False(t, pod.Spec.HostPID, "the policies pod must not share the host's PID namespace")
		}
	}
	assert.Equal(t, []string{"cp-a"}, policiesNodes, "only one policies pod, on the first control-plane node by name")
}

// Section 5 lists the cluster's pods, so the policies pod runs alone: none of
// the hostPID node pods exist yet while it runs.
func TestScanRunsPoliciesAloneFirst(t *testing.T) {
	t.Parallel()

	cs := scanCluster(t, succeeded(), testNode("cp-a", true), testNode("w-1", false))
	_, err := testScanner(t, cs).Scan(t.Context())
	require.NoError(t, err)

	policiesCreate, policiesFinished, firstNodePod := -1, -1, -1
	for i, action := range cs.Actions() {
		if action.GetResource().Resource != "pods" {
			continue
		}
		switch action.GetVerb() {
		case "create":
			pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
			if runsPolicies(pod) {
				policiesCreate = i
			} else if firstNodePod < 0 {
				firstNodePod = i
			}
		case "get":
			if policiesCreate >= 0 && firstNodePod < 0 {
				policiesFinished = i
			}
		}
	}
	require.GreaterOrEqual(t, policiesCreate, 0, "no policies pod was created")
	require.GreaterOrEqual(t, firstNodePod, 0, "no node pod was created")
	assert.Less(t, policiesCreate, policiesFinished, "the scan reads the policies pod before creating node pods")
	assert.Less(t, policiesFinished, firstNodePod, "node pods are created only after the policies pod finished")
}

// 5.1.6 counts a pod as compliant only when its service account mounts no
// token too, so the policies pod runs as its own, which the scan deletes.
func TestScanRunsPoliciesAsItsOwnServiceAccount(t *testing.T) {
	t.Parallel()

	cs := scanCluster(t, succeeded(), testNode("cp-a", true))
	_, err := testScanner(t, cs).Scan(t.Context())
	require.NoError(t, err)

	serviceAccounts := created[*corev1.ServiceAccount](cs, "serviceaccounts")
	require.Len(t, serviceAccounts, 1)
	require.NotNil(t, serviceAccounts[0].AutomountServiceAccountToken)
	assert.False(t, *serviceAccounts[0].AutomountServiceAccountToken)
	for _, pod := range created[*corev1.Pod](cs, "pods") {
		if runsPolicies(pod) {
			assert.True(t, strings.HasPrefix(pod.Spec.ServiceAccountName, "kube-bench-policies-"), pod.Spec.ServiceAccountName)
			assert.Contains(t, deleted(cs, "serviceaccounts"), pod.Spec.ServiceAccountName)
		}
	}
	assertNothingLeft(t, cs)
}

// If kubectl in the policies pod cannot read the cluster, every automated
// section 5 check warns, and the tenant register would except them all. A
// report with no audit rows is an error, not a pass.
func TestScanPoliciesThatReadNothingIsAnError(t *testing.T) {
	t.Parallel()

	cs := scanCluster(t, succeeded(), testNode("cp-a", true))
	scanner := testScanner(t, cs)
	logs := scanner.Logs
	empty := strings.ReplaceAll(fixtureLine(t, "report_policies.json"),
		"**role_name: synthetic-admin role_binding: synthetic-admin subject: ops is_compliant: false", "")
	scanner.Logs = func(ctx context.Context, namespace, name string) ([]byte, error) {
		pod, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil && runsPolicies(pod) {
			return []byte(empty + "\n"), nil
		}
		return logs(ctx, namespace, name)
	}

	_, err := scanner.Scan(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "policies")
	assert.Contains(t, err.Error(), "read nothing from the cluster")
	assertNothingLeft(t, cs)
}

// The tenant API server is reached through a relay; one failed read of a pod
// must not fail a scan that would otherwise finish.
func TestScanPollsThroughTransientErrors(t *testing.T) {
	t.Parallel()

	cs := scanCluster(t, succeeded(), testNode("cp-a", true), testNode("w-1", false))
	var failures atomic.Int32
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failures.Add(1) <= 2 {
			return true, nil, apierrors.NewServiceUnavailable("relay restarting")
		}
		return false, nil, nil
	})

	reports, err := testScanner(t, cs).Scan(t.Context())
	require.NoError(t, err)
	assert.Len(t, reports, 2)
}

func TestScanMountsTheBenchmarkConfigMap(t *testing.T) {
	t.Parallel()

	cs := scanCluster(t, succeeded(), testNode("cp-a", true), testNode("w-1", false))
	_, err := testScanner(t, cs).Scan(t.Context())
	require.NoError(t, err)

	// The fake clientset records a create before the API server names the
	// object, so the name comes from the pods and the delete.
	configMaps := created[*corev1.ConfigMap](cs, "configmaps")
	require.Len(t, configMaps, 1)
	assert.Equal(t, syntheticBenchmark(), benchmarkContent(configMaps[0]))
	mounted := map[string]bool{}
	for _, pod := range created[*corev1.Pod](cs, "pods") {
		for _, volume := range pod.Spec.Volumes {
			if volume.ConfigMap != nil {
				mounted[volume.ConfigMap.Name] = true
			}
		}
	}
	require.Len(t, mounted, 1, "every pod mounts the same ConfigMap")
	for name := range mounted {
		assert.True(t, strings.HasPrefix(name, "kube-bench-cfg-"), name)
		assert.Contains(t, deleted(cs, "configmaps"), name)
	}
}

// deleted returns the names of one resource's objects the scan deleted.
func deleted(cs *fake.Clientset, resource string) []string {
	var names []string
	for _, action := range cs.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource == resource {
			names = append(names, action.(k8stesting.DeleteAction).GetName())
		}
	}
	return names
}

// benchmarkContent is a benchmark ConfigMap's files by path.
func benchmarkContent(configMap *corev1.ConfigMap) map[string][]byte {
	files := map[string][]byte{}
	for key, content := range configMap.Data {
		files[strings.ReplaceAll(key, "_", "/")] = []byte(content)
	}
	return files
}

func TestScanDeletesItsPodsAndConfigMapWhenItSucceeds(t *testing.T) {
	t.Parallel()

	cs := scanCluster(t, succeeded(), testNode("cp-a", true), testNode("w-1", false))
	_, err := testScanner(t, cs).Scan(t.Context())
	require.NoError(t, err)
	assertNothingLeft(t, cs)
}

func TestScanChecksumMismatchCreatesNothing(t *testing.T) {
	t.Parallel()

	cs := scanCluster(t, succeeded(), testNode("cp-a", true))
	scanner := testScanner(t, cs)
	scanner.Files = maps.Clone(scanner.Files)
	scanner.Files["cis-2.0/master.yaml"] = strings.Repeat("0", 64)

	_, err := scanner.Scan(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cis-2.0/master.yaml")
	assert.Empty(t, created[*corev1.ConfigMap](cs, "configmaps"))
	assert.Empty(t, created[*corev1.Pod](cs, "pods"))
}

func imagePullBackOff() corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  PodContainer,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"}},
		}},
	}
}

func TestScanPodThatNeverFinishes(t *testing.T) {
	t.Parallel()

	t.Run("fails the scan naming the node and why", func(t *testing.T) {
		t.Parallel()
		cs := scanCluster(t, imagePullBackOff(), testNode("cp-a", true))
		scanner := testScanner(t, cs)
		scanner.Timeout = 200 * time.Millisecond

		start := time.Now()
		_, err := scanner.Scan(t.Context())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cp-a")
		assert.Contains(t, err.Error(), "ImagePullBackOff")
		assert.Less(t, time.Since(start), time.Second)
	})
	t.Run("deletes its pods and ConfigMap", func(t *testing.T) {
		t.Parallel()
		cs := scanCluster(t, imagePullBackOff(), testNode("cp-a", true), testNode("w-1", false))
		scanner := testScanner(t, cs)
		scanner.Timeout = 200 * time.Millisecond

		_, err := scanner.Scan(t.Context())
		require.Error(t, err)
		assertNothingLeft(t, cs)
	})
}

func TestScanWithNoControlPlaneNodeIsAnError(t *testing.T) {
	t.Parallel()

	cs := scanCluster(t, succeeded(), testNode("w-1", false), testNode("w-2", false))
	_, err := testScanner(t, cs).Scan(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no control-plane node")
	assert.Empty(t, created[*corev1.Pod](cs, "pods"))
}

func TestScanPodThatFailsReportsItsLogTail(t *testing.T) {
	t.Parallel()

	cs := scanCluster(t, corev1.PodStatus{Phase: corev1.PodFailed}, testNode("cp-a", true))
	scanner := testScanner(t, cs)
	scanner.Logs = func(context.Context, string, string) ([]byte, error) {
		return []byte("line one\nfailed to read /etc/kubernetes/manifests\n"), nil
	}

	_, err := scanner.Scan(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cp-a")
	assert.Contains(t, err.Error(), "failed to read /etc/kubernetes/manifests")
	assertNothingLeft(t, cs)
}

func TestCheck(t *testing.T) {
	t.Parallel()

	cs := scanCluster(t, succeeded(), testNode("cp-a", true), testNode("w-1", false))
	scanner := testScanner(t, cs)
	now := time.Date(2026, time.November, 5, 0, 0, 0, 0, time.UTC)
	outDir := filepath.Join(t.TempDir(), "kube-bench")

	out, err := Check(t.Context(), scanner, ClusterTenant, outDir, now)
	require.NoError(t, err)

	reg, err := DefaultRegister()
	require.NoError(t, err)
	controlPlane, err := ParseLog([]byte(fixtureLine(t, "report_control_plane.json")))
	require.NoError(t, err)
	worker, err := ParseLog([]byte(fixtureLine(t, "report_worker.json")))
	require.NoError(t, err)
	policies, err := ParseLog([]byte(fixtureLine(t, "report_policies.json")))
	require.NoError(t, err)
	controlPlane.Controls = append(controlPlane.Controls, policies.Controls...)
	want := Evaluate(ClusterTenant, []NodeReport{
		{Node: "cp-a", Role: RoleControlPlane, Report: controlPlane},
		{Node: "w-1", Role: RoleWorker, Report: worker},
	}, reg, now)
	assert.Equal(t, want, out)

	for _, name := range []string{"results.json", "summary.txt"} {
		_, err := os.Stat(filepath.Join(outDir, name))
		assert.NoError(t, err, name)
	}
}
