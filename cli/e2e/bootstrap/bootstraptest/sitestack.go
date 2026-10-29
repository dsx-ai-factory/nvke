// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"context"
	"fmt"
	"os"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// EnvSiteStackNamespaces lists, comma-separated, the namespaces the NVKE stack
// installs on the site cluster. When set, TestE2E_BootstrapSite checks them
// with VerifySiteStack once the bootstrap succeeds.
const EnvSiteStackNamespaces = "NVKECTL_E2E_SITE_STACK_NAMESPACES"

// SpireNamespace is where the NVKE stack installs SPIRE. A bootstrap with mTLS
// disabled installs no SPIRE.
const SpireNamespace = "spire-server"

// defaultStorageClassAnnotation marks the StorageClass a PVC without one binds to.
const defaultStorageClassAnnotation = "storageclass.kubernetes.io/is-default-class"

// SiteStackNamespacesFromEnv returns the namespaces EnvSiteStackNamespaces
// lists, or nil when it is unset. With disableMTLS it leaves out
// SpireNamespace, which such a bootstrap does not install.
func SiteStackNamespacesFromEnv(disableMTLS bool) []string {
	var namespaces []string
	for _, ns := range strings.Split(os.Getenv(EnvSiteStackNamespaces), ",") {
		if ns = strings.TrimSpace(ns); ns != "" && (ns != SpireNamespace || !disableMTLS) {
			namespaces = append(namespaces, ns)
		}
	}
	return namespaces
}

// VerifySiteStack checks, once, that each namespace holds at least one
// Deployment, StatefulSet or DaemonSet, every one of them is rolled out, every
// pod that has not completed is Ready, and the cluster has a default
// StorageClass. It does not wait: it is called after bootstrap site has
// reported the stack healthy, so anything not ready is a failure. The error
// names every problem found.
func VerifySiteStack(ctx context.Context, kube kubernetes.Interface, namespaces []string) error {
	pending, err := siteStackPending(ctx, kube, namespaces)
	if err != nil {
		return fmt.Errorf("checking the NVKE stack on the site cluster: %w", err)
	}
	if len(pending) > 0 {
		return fmt.Errorf("the NVKE stack on the site cluster is not ready: %s", strings.Join(pending, "; "))
	}
	return nil
}

// siteStackPending lists what keeps the stack from being ready.
func siteStackPending(ctx context.Context, kube kubernetes.Interface, namespaces []string) ([]string, error) {
	var pending []string
	for _, ns := range namespaces {
		workloads, notReady, err := namespaceWorkloads(ctx, kube, ns)
		if err != nil {
			return nil, err
		}
		if workloads == 0 {
			pending = append(pending, ns+" has no workloads")
		}
		pending = append(pending, notReady...)

		pods, err := kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing pods in %s: %w", ns, err)
		}
		for i := range pods.Items {
			if pod := &pods.Items[i]; pod.Status.Phase != corev1.PodSucceeded && !podReady(pod) {
				pending = append(pending, fmt.Sprintf("pod %s/%s is not Ready", ns, pod.Name))
			}
		}
	}

	classes, err := kube.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing StorageClasses: %w", err)
	}
	hasDefault := false
	for _, sc := range classes.Items {
		hasDefault = hasDefault || sc.Annotations[defaultStorageClassAnnotation] == "true"
	}
	if !hasDefault {
		pending = append(pending, "no default StorageClass")
	}
	return pending, nil
}

// namespaceWorkloads counts the namespace's Deployments, StatefulSets and
// DaemonSets and names those not rolled out, by the checks `kubectl rollout status` makes.
func namespaceWorkloads(ctx context.Context, kube kubernetes.Interface, ns string) (int, []string, error) {
	var notReady []string
	deployments, err := kube.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, nil, fmt.Errorf("listing deployments in %s: %w", ns, err)
	}
	for i := range deployments.Items {
		if d := &deployments.Items[i]; !deploymentRolledOut(d) {
			notReady = append(notReady, fmt.Sprintf("deployment %s/%s is not rolled out", ns, d.Name))
		}
	}
	statefulSets, err := kube.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, nil, fmt.Errorf("listing statefulsets in %s: %w", ns, err)
	}
	for i := range statefulSets.Items {
		if s := &statefulSets.Items[i]; !statefulSetRolledOut(s) {
			notReady = append(notReady, fmt.Sprintf("statefulset %s/%s is not rolled out", ns, s.Name))
		}
	}
	daemonSets, err := kube.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, nil, fmt.Errorf("listing daemonsets in %s: %w", ns, err)
	}
	for i := range daemonSets.Items {
		if d := &daemonSets.Items[i]; !daemonSetRolledOut(d) {
			notReady = append(notReady, fmt.Sprintf("daemonset %s/%s is not rolled out", ns, d.Name))
		}
	}
	return len(deployments.Items) + len(statefulSets.Items) + len(daemonSets.Items), notReady, nil
}

func deploymentRolledOut(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	return s.ObservedGeneration >= d.Generation && s.UpdatedReplicas == want && s.Replicas == want && s.AvailableReplicas == want
}

func statefulSetRolledOut(s *appsv1.StatefulSet) bool {
	want := int32(1)
	if s.Spec.Replicas != nil {
		want = *s.Spec.Replicas
	}
	st := s.Status
	return st.ObservedGeneration >= s.Generation && st.UpdatedReplicas == want && st.ReadyReplicas == want
}

func daemonSetRolledOut(d *appsv1.DaemonSet) bool {
	s := d.Status
	return s.ObservedGeneration >= d.Generation && s.UpdatedNumberScheduled == s.DesiredNumberScheduled &&
		s.NumberAvailable == s.DesiredNumberScheduled
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
