// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// controlPlaneNodeLabel marks a control-plane node.
	controlPlaneNodeLabel = "node-role.kubernetes.io/control-plane"

	// auditRowMarker is in every row the cis-2.0 policies audits print for an
	// object they list, compliant or not.
	auditRowMarker = "is_compliant:"

	defaultNamespace     = "default"
	defaultTimeout       = 10 * time.Minute
	defaultPoll          = 2 * time.Second
	defaultFetchTimeout  = 2 * time.Minute
	cleanupTimeout       = time.Minute
	scanPodTerminalPhase = "Succeeded or Failed"
)

// Scanner runs kube-bench on every node of a cluster.
type Scanner struct {
	// Clientset reaches the cluster under test.
	Clientset kubernetes.Interface
	// Namespace holds the scan's pods and ConfigMap; "default" when empty.
	Namespace string
	// Timeout bounds the whole scan; 10 minutes when zero.
	Timeout time.Duration
	// Poll is how often the scan checks its pods; 2 seconds when zero.
	Poll time.Duration
	// Logs reads a scan pod's PodContainer log; the clientset's when nil.
	Logs func(ctx context.Context, namespace, pod string) ([]byte, error)
	// HTTPClient fetches the benchmark files; one with a 2-minute timeout
	// when nil.
	HTTPClient *http.Client
	// BenchmarkURL is where the benchmark files are fetched from;
	// the package's BenchmarkURL when empty.
	BenchmarkURL string
	// Files maps each benchmark file's path to its SHA-256; BenchmarkFiles()
	// when nil.
	Files map[string]string
}

// scanNode is one node the scan runs on.
type scanNode struct {
	name     string
	role     Role
	policies bool
}

func (s Scanner) withDefaults() Scanner {
	if s.Namespace == "" {
		s.Namespace = defaultNamespace
	}
	if s.Timeout == 0 {
		s.Timeout = defaultTimeout
	}
	if s.Poll == 0 {
		s.Poll = defaultPoll
	}
	if s.Logs == nil {
		clientset := s.Clientset
		s.Logs = func(ctx context.Context, namespace, pod string) ([]byte, error) {
			return clientset.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: PodContainer}).DoRaw(ctx)
		}
	}
	if s.HTTPClient == nil {
		s.HTTPClient = &http.Client{Timeout: defaultFetchTimeout}
	}
	if s.BenchmarkURL == "" {
		s.BenchmarkURL = BenchmarkURL
	}
	if s.Files == nil {
		s.Files = BenchmarkFiles()
	}
	return s
}

// Scan fetches and checks the benchmark files, runs the policies checks, then
// one kube-bench pod per node, and returns every node's report; the first
// control-plane node's includes the policies checks. It deletes what it
// created on every path, including a canceled or timed-out scan.
//
// The policies checks list the cluster's pods, so they run first and alone,
// in a pod that carries none of what they flag: the node pods share the
// host's PID namespace and would otherwise be reported against the cluster.
func (s Scanner) Scan(ctx context.Context) ([]NodeReport, error) {
	s = s.withDefaults()
	cleanupCtx := context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	files, err := FetchBenchmark(ctx, s.HTTPClient, s.BenchmarkURL, s.Files)
	if err != nil {
		return nil, err
	}
	nodes, err := s.nodes(ctx)
	if err != nil {
		return nil, err
	}

	var pods []*corev1.Pod
	var configMap *corev1.ConfigMap
	var serviceAccount *corev1.ServiceAccount
	defer func() { s.cleanup(cleanupCtx, configMap, serviceAccount, pods) }()

	created, err := s.Clientset.CoreV1().ConfigMaps(s.Namespace).Create(ctx, BenchmarkConfigMap(s.Namespace, files), metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating the kube-bench benchmark ConfigMap: %w", err)
	}
	configMap = created
	volume := BenchmarkVolume(configMap.Name, files)

	policiesNode := nodes[slices.IndexFunc(nodes, func(n scanNode) bool { return n.policies })].name
	createdSA, err := s.Clientset.CoreV1().ServiceAccounts(s.Namespace).Create(ctx, PoliciesServiceAccount(s.Namespace), metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating the kube-bench policies service account: %w", err)
	}
	serviceAccount = createdSA
	policiesPod, err := s.Clientset.CoreV1().Pods(s.Namespace).Create(ctx, NewPoliciesPod(s.Namespace, policiesNode, serviceAccount.Name, volume), metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating the kube-bench policies pod on node %s: %w", policiesNode, err)
	}
	pods = append(pods, policiesPod)
	finishedPolicies, err := s.wait(ctx, []*corev1.Pod{policiesPod})
	if err != nil {
		return nil, err
	}
	policies, err := s.policiesReport(ctx, finishedPolicies[0])
	if err != nil {
		return nil, err
	}

	nodePods := make([]*corev1.Pod, 0, len(nodes))
	for _, node := range nodes {
		pod, createErr := s.Clientset.CoreV1().Pods(s.Namespace).Create(ctx, NewPod(s.Namespace, node.name, volume, node.role), metav1.CreateOptions{})
		if createErr != nil {
			return nil, fmt.Errorf("creating the kube-bench pod for node %s: %w", node.name, createErr)
		}
		pods = append(pods, pod)
		nodePods = append(nodePods, pod)
	}
	finished, err := s.wait(ctx, nodePods)
	if err != nil {
		return nil, err
	}
	reports, err := s.reports(ctx, nodes, finished)
	if err != nil {
		return nil, err
	}
	for i := range reports {
		if reports[i].Node == policiesNode {
			reports[i].Report.Controls = append(reports[i].Report.Controls, policies.Controls...)
		}
	}
	return reports, nil
}

// policiesReport reads the policies pod's report and checks that its audits
// read the cluster. If kubectl in the pod cannot reach the API server, every
// automated section 5 check warns, which looks like ordinary findings and
// which the tenant register would except; a report in which no audit returned
// a row is an error instead.
func (s Scanner) policiesReport(ctx context.Context, pod *corev1.Pod) (Report, error) {
	node := pod.Spec.NodeName
	log, err := s.Logs(ctx, s.Namespace, pod.Name)
	if err != nil {
		return Report{}, fmt.Errorf("reading the kube-bench policies log on node %s: %w", node, err)
	}
	if pod.Status.Phase == corev1.PodFailed {
		return Report{}, fmt.Errorf("the kube-bench policies checks failed on node %s; the log ends:\n%s", node, logTail(log))
	}
	report, err := ParseLog(log)
	if err != nil {
		return Report{}, fmt.Errorf("the kube-bench policies checks on node %s: %w", node, err)
	}
	for _, r := range results(report) {
		if strings.Contains(r.ActualValue, auditRowMarker) {
			return report, nil
		}
	}
	return Report{}, fmt.Errorf("the kube-bench policies checks on node %s read nothing from the cluster: no audit returned a row, "+
		"so kubectl in the pod could not list the cluster through %s; the log ends:\n%s", node, adminKubeconfig, logTail(log))
}

// nodes lists the cluster's nodes by name, giving the first control-plane
// node the policies target.
func (s Scanner) nodes(ctx context.Context) ([]scanNode, error) {
	list, err := s.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	items := slices.Clone(list.Items)
	slices.SortFunc(items, func(a, b corev1.Node) int { return strings.Compare(a.Name, b.Name) })

	nodes := make([]scanNode, 0, len(items))
	policiesGiven := false
	for _, n := range items {
		node := scanNode{name: n.Name, role: RoleWorker}
		if _, ok := n.Labels[controlPlaneNodeLabel]; ok {
			node.role = RoleControlPlane
			node.policies = !policiesGiven
			policiesGiven = true
		}
		nodes = append(nodes, node)
	}
	if !policiesGiven {
		return nil, fmt.Errorf("no control-plane node found: no node carries %s, so the policies checks cannot run", controlPlaneNodeLabel)
	}
	return nodes, nil
}

// wait polls the pods until each has finished, and returns them. When the scan
// times out first, the error names each unfinished pod's node and why it has
// not finished.
func (s Scanner) wait(ctx context.Context, pods []*corev1.Pod) ([]*corev1.Pod, error) {
	ticker := time.NewTicker(s.Poll)
	defer ticker.Stop()
	for {
		current := make([]*corev1.Pod, 0, len(pods))
		var waiting []string
		for _, pod := range pods {
			got, err := s.Clientset.CoreV1().Pods(s.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			if err != nil {
				if ctx.Err() != nil {
					return nil, s.timedOut(waiting, pods, current)
				}
				// A failed read, such as a relay restarting, is retried at the
				// next poll; it fails the scan only if it lasts to the timeout.
				waiting = append(waiting, fmt.Sprintf("%s: could not read the pod: %v", pod.Spec.NodeName, err))
				continue
			}
			current = append(current, got)
			if got.Status.Phase != corev1.PodSucceeded && got.Status.Phase != corev1.PodFailed {
				waiting = append(waiting, fmt.Sprintf("%s: %s", got.Spec.NodeName, notFinishedBecause(got)))
			}
		}
		if len(waiting) == 0 {
			return current, nil
		}
		select {
		case <-ctx.Done():
			return nil, s.timedOut(waiting, pods, current)
		case <-ticker.C:
		}
	}
}

func (s Scanner) timedOut(waiting []string, pods, current []*corev1.Pod) error {
	if len(waiting) == 0 {
		for _, pod := range pods[len(current):] {
			waiting = append(waiting, pod.Spec.NodeName+": not read before the timeout")
		}
	}
	return fmt.Errorf("kube-bench pods did not reach %s within %s: %s", scanPodTerminalPhase, s.Timeout, strings.Join(waiting, "; "))
}

// notFinishedBecause is the most specific reason a pod has not finished: its
// container's waiting reason, the pod's own reason, an unmet condition, or its
// phase.
func notFinishedBecause(pod *corev1.Pod) string {
	for _, status := range pod.Status.ContainerStatuses {
		if w := status.State.Waiting; w != nil && w.Reason != "" {
			if w.Message != "" {
				return w.Reason + " (" + w.Message + ")"
			}
			return w.Reason
		}
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason
	}
	for _, c := range pod.Status.Conditions {
		if c.Status == corev1.ConditionFalse && c.Reason != "" {
			return fmt.Sprintf("%s: %s", c.Type, c.Reason)
		}
	}
	if pod.Status.Phase == "" {
		return "no status yet"
	}
	return string(pod.Status.Phase)
}

// reports reads every finished pod's log. A failed pod's error quotes the end
// of its log; every node's problem is reported, not only the first.
func (s Scanner) reports(ctx context.Context, nodes []scanNode, pods []*corev1.Pod) ([]NodeReport, error) {
	roles := make(map[string]Role, len(nodes))
	for _, n := range nodes {
		roles[n.name] = n.role
	}
	reports := make([]NodeReport, 0, len(pods))
	var errs []error
	for _, pod := range pods {
		node := pod.Spec.NodeName
		log, err := s.Logs(ctx, s.Namespace, pod.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading the kube-bench log on node %s: %w", node, err))
			continue
		}
		if pod.Status.Phase == corev1.PodFailed {
			errs = append(errs, fmt.Errorf("kube-bench failed on node %s; its log ends:\n%s", node, logTail(log)))
			continue
		}
		report, err := ParseLog(log)
		if err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node, err))
			continue
		}
		reports = append(reports, NodeReport{Node: node, Role: roles[node], Report: report})
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return reports, nil
}

// cleanup deletes the scan's pods, ConfigMap and service account. It runs on
// a context the caller's cancellation does not reach, so a canceled spec
// still cleans up.
func (s Scanner) cleanup(ctx context.Context, configMap *corev1.ConfigMap, serviceAccount *corev1.ServiceAccount, pods []*corev1.Pod) {
	ctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	for _, pod := range pods {
		if err := s.Clientset.CoreV1().Pods(s.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
			slog.Warn("deleting a kube-bench pod", "pod", pod.Name, "node", pod.Spec.NodeName, "err", err)
		}
	}
	if configMap != nil {
		if err := s.Clientset.CoreV1().ConfigMaps(s.Namespace).Delete(ctx, configMap.Name, metav1.DeleteOptions{}); err != nil {
			slog.Warn("deleting the kube-bench benchmark ConfigMap", "configMap", configMap.Name, "err", err)
		}
	}
	if serviceAccount != nil {
		if err := s.Clientset.CoreV1().ServiceAccounts(s.Namespace).Delete(ctx, serviceAccount.Name, metav1.DeleteOptions{}); err != nil {
			slog.Warn("deleting the kube-bench policies service account", "serviceAccount", serviceAccount.Name, "err", err)
		}
	}
}

// Check scans the cluster, judges the reports against the embedded register,
// and, when outDir is not empty, writes the artifacts there.
func Check(ctx context.Context, s Scanner, cluster Cluster, outDir string, now time.Time) (Outcome, error) {
	nodes, err := s.Scan(ctx)
	if err != nil {
		return Outcome{}, err
	}
	reg, err := DefaultRegister()
	if err != nil {
		return Outcome{}, err
	}
	out := Evaluate(cluster, nodes, reg, now)
	if outDir != "" {
		if err := WriteArtifacts(outDir, nodes, out); err != nil {
			return out, err
		}
	}
	return out, nil
}
