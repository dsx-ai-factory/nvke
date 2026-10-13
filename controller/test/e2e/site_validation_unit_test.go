// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

func TestUnit_SiteNodeProblems(t *testing.T) {
	ready := corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}
	tests := []struct {
		name  string
		nodes []corev1.Node
		want  []string
	}{
		{name: "no nodes", want: []string{"the cluster has no nodes"}},
		{name: "ready and untainted", nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: ready}}},
		{
			name: "PreferNoSchedule does not stop scheduling",
			nodes: []corev1.Node{{
				ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: ready,
				Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "k", Effect: corev1.TaintEffectPreferNoSchedule}}},
			}},
		},
		{
			name: "control-plane taint left on",
			nodes: []corev1.Node{{
				ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: ready,
				Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}},
			}},
			want: []string{"n: tainted node-role.kubernetes.io/control-plane:NoSchedule"},
		},
		{
			name:  "not ready and cordoned",
			nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Spec: corev1.NodeSpec{Unschedulable: true}}},
			want:  []string{"n: not Ready", "n: cordoned"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, nodeProblems(tt.nodes))
		})
	}
}

func TestUnit_SiteWorkloadProblems(t *testing.T) {
	required := []siteNamespace{{name: "a", source: "item a"}, {name: "b", source: "item b"}}
	meta := func(ns, name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 2}
	}
	available := appsv1.Deployment{
		ObjectMeta: meta("a", "ok"),
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](2)},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, UpdatedReplicas: 2, AvailableReplicas: 2},
	}
	// Nothing to schedule is healthy: a DaemonSet selecting no node on the
	// site wants zero pods and has them.
	idle := appsv1.DaemonSet{ObjectMeta: meta("b", "idle"), Status: appsv1.DaemonSetStatus{ObservedGeneration: 2}}

	t.Run("healthy", func(t *testing.T) {
		require.Empty(t, workloadProblems(required, []appsv1.Deployment{available}, nil, []appsv1.DaemonSet{idle}))
	})
	t.Run("a required namespace with nothing in it", func(t *testing.T) {
		require.Equal(t, []string{"namespace b: no workloads (expected from item b)"},
			workloadProblems(required, []appsv1.Deployment{available}, nil, nil))
	})
	t.Run("unset replicas default to one", func(t *testing.T) {
		d := appsv1.Deployment{ObjectMeta: meta("a", "d"), Status: appsv1.DeploymentStatus{ObservedGeneration: 2}}
		require.Equal(t, []string{"Deployment a/d: 0 of 1 available, 0 updated"},
			workloadProblems(required, []appsv1.Deployment{d}, nil, []appsv1.DaemonSet{idle}))
	})
	t.Run("status from an older generation is not trusted", func(t *testing.T) {
		stale := available
		stale.Status.ObservedGeneration = 1
		require.Equal(t, []string{"Deployment a/ok: 2 of 2 available, 2 updated"},
			workloadProblems(required, []appsv1.Deployment{stale}, nil, []appsv1.DaemonSet{idle}))
	})
	t.Run("an unready StatefulSet and DaemonSet outside the required namespaces", func(t *testing.T) {
		st := appsv1.StatefulSet{ObjectMeta: meta("other", "db"), Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 2}}
		ds := appsv1.DaemonSet{ObjectMeta: meta("cni", "ovnkube-node"), Status: appsv1.DaemonSetStatus{ObservedGeneration: 2, DesiredNumberScheduled: 1, NumberReady: 0, NumberAvailable: 0}}
		require.Equal(t, []string{
			"DaemonSet cni/ovnkube-node: 0 of 1 ready, 0 available",
			"StatefulSet other/db: 0 of 1 ready",
		}, workloadProblems(required, []appsv1.Deployment{available}, []appsv1.StatefulSet{st}, []appsv1.DaemonSet{idle, ds}))
	})
}

func TestUnit_SitePodProblems(t *testing.T) {
	running := func(name string, ready bool) corev1.Pod {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
		}
	}
	failedOf := func(name, job string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: job}}},
			Status:     corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Error"},
		}
	}
	complete := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "schema"},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
	}
	running2 := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "migrate"}}
	pending := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
	succeeded := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "s"}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}

	require.Equal(t, []string{
		"ns/migrate-1: Failed Error",
		"ns/p: Pending",
		"ns/unready: Running, not Ready",
	}, podProblems([]corev1.Pod{
		running("ok", true),
		running("unready", false),
		failedOf("schema-1", "schema"),
		failedOf("migrate-1", "migrate"),
		pending,
		succeeded,
	}, []batchv1.Job{complete, running2}))
}

func TestUnit_SiteCertificateProblems(t *testing.T) {
	cert := func(name, status string) unstructured.Unstructured {
		u := unstructured.Unstructured{Object: map[string]any{
			"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": status}}},
		}}
		u.SetNamespace("spire-server")
		u.SetName(name)
		return u
	}
	require.Equal(t, []string{"the cluster has no cert-manager Certificates"}, certificateProblems(nil))
	require.Empty(t, certificateProblems([]unstructured.Unstructured{cert("ca", "True")}))
	require.Equal(t, []string{"Certificate spire-server/pending: not Ready"},
		certificateProblems([]unstructured.Unstructured{cert("ca", "True"), cert("pending", "False")}))
}

func TestUnit_SiteHealthCheckProblems(t *testing.T) {
	healthy := "[+]ping ok\n[+]log ok\n[+]etcd ok\n[+]poststarthook/start-informers ok\nreadyz check passed\n"
	require.NoError(t, healthCheckProblems("/readyz", healthy))

	err := healthCheckProblems("/readyz", "[+]ping ok\n[-]etcd failed: reason withheld\nreadyz check failed\n")
	require.ErrorContains(t, err, "[-]etcd failed")
	require.ErrorContains(t, err, "no passing etcd check")

	require.NoError(t, healthCheckProblems("/livez", "[+]ping ok\nlivez check passed\n"))
	require.ErrorContains(t, healthCheckProblems("/livez", "[+]ping ok\n[-]log failed\n"), "[-]log failed")
}

func TestUnit_SiteVersionProblems(t *testing.T) {
	node := func(name, kubelet string) corev1.Node {
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: kubelet}}}
	}
	require.Empty(t, versionProblems("1.36.3", "v1.36.3", []corev1.Node{node("a", "v1.36.3")}))
	require.Empty(t, versionProblems("v1.36.3", "v1.36.3", []corev1.Node{node("a", "v1.36.3")}))
	require.Empty(t, versionProblems("1.36.3", "v1.36.3+abc", []corev1.Node{node("a", "v1.36.3-vendor.1")}),
		"a build suffix is the same Kubernetes")
	require.Equal(t, []string{"API server: v1.36.2, want 1.36.3", "a: kubelet: v1.35.0, want 1.36.3"},
		versionProblems("1.36.3", "v1.36.2", []corev1.Node{node("a", "v1.35.0")}))
	require.Equal(t, []string{`a: kubelet: version "" does not parse`},
		versionProblems("1.36.3", "v1.36.3", []corev1.Node{node("a", "")}))
	require.Len(t, versionProblems("", "v1.36.3", nil), 1, "an empty wanted version is a problem, not a pass")
}

func TestUnit_SitePodProblemsCatchStuckAndEvicted(t *testing.T) {
	crashLooping := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "loop"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}
	initPull := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pull"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "init", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}},
		},
	}
	controller := true
	evictedOwned := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "evicted-owned", OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "rs", Controller: &controller}}},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"},
	}
	evictedBare := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "evicted-bare"},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"},
	}
	require.Equal(t, []string{
		"ns/evicted-bare: Failed Evicted",
		"ns/loop: container app CrashLoopBackOff",
		"ns/pull: container init ImagePullBackOff",
	}, podProblems([]corev1.Pod{crashLooping, initPull, evictedOwned, evictedBare}, nil))
}

// The deploy path is what the required namespaces are copied from, so a
// renamed or added item has to change the list with it.
func TestUnit_SiteNamespacesCoverTheDeployPath(t *testing.T) {
	const deployPath = "../../../deploy/nico"
	// The backend E2E lanes run this package as a compiled binary from /state
	// or from controller/, so nothing is at the deploy path, and go test runs
	// every Test in it, not just TestE2E. Only the checkout-based unit lane can
	// answer this; there, a missing manifest still fails.
	if _, err := os.Stat(deployPath); os.IsNotExist(err) {
		t.Skipf("no deploy path beside the test binary: %v", err)
	}
	var manifest struct {
		Items []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"items"`
	}
	raw, err := os.ReadFile(filepath.Join(deployPath, "nvke-deploy.yaml"))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(raw, &manifest))
	require.NotEmpty(t, manifest.Items)

	installed := map[string]string{}
	for _, item := range manifest.Items {
		dir := filepath.Join(deployPath, item.Name)
		switch item.Type {
		case "helm":
			var release struct {
				Release struct {
					Namespace string `json:"namespace"`
				} `json:"release"`
			}
			raw, err := os.ReadFile(filepath.Join(dir, "release.yaml"))
			require.NoError(t, err)
			require.NoError(t, yaml.Unmarshal(raw, &release))
			require.NotEmpty(t, release.Release.Namespace, "item %s names no release namespace", item.Name)
			installed[release.Release.Namespace] = item.Name
		case "manifest":
			files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
			require.NoError(t, err)
			for _, file := range files {
				raw, err := os.ReadFile(file)
				require.NoError(t, err)
				for _, doc := range strings.Split(string(raw), "\n---") {
					var object struct {
						Kind     string `json:"kind"`
						Metadata struct {
							Namespace string `json:"namespace"`
						} `json:"metadata"`
					}
					require.NoError(t, yaml.Unmarshal([]byte(doc), &object), "%s", file)
					if slices.Contains([]string{"Deployment", "StatefulSet", "DaemonSet"}, object.Kind) {
						installed[object.Metadata.Namespace] = item.Name
					}
				}
			}
		}
	}

	listed := map[string]bool{}
	for _, ns := range siteNamespaces {
		listed[ns.name] = true
		if strings.HasPrefix(ns.source, "deploy path item") {
			require.Contains(t, installed, ns.name, "%s is listed as coming from the deploy path, which no longer installs into it", ns.name)
		}
	}
	for ns, item := range installed {
		require.True(t, listed[ns], "deploy path item %s installs into %s, which siteNamespaces does not require", item, ns)
	}
}
