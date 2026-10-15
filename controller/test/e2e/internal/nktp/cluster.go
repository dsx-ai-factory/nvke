// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nktp

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

const (
	probeName      = "nktp-e2e-redaction"
	probeNamespace = "default"
	// nobody, so the probe runs under the restricted Pod Security level.
	probeUser   = 65534
	stopTimeout = 30 * time.Second

	statusField = "status"
	listLimit   = 10
)

// PodsReady holds when every Pod in the chart's namespace is Ready, skipping
// Pods that ran to completion.
func (v *Validator) PodsReady() Check {
	return v.allOf("every Pod in "+chartNamespace+" is Ready", gvr("", "v1", "pods"), chartNamespace, func(pod unstructured.Unstructured) string {
		phase, _, _ := unstructured.NestedString(pod.Object, statusField, "phase")
		if phase == string(corev1.PodSucceeded) || (phase == string(corev1.PodRunning) && pod.GetDeletionTimestamp() == nil && conditionTrue(pod, "Ready")) {
			return ""
		}
		return phase
	})
}

// CertificatesReady holds when cert-manager has issued every certificate in
// the chart's namespace.
func (v *Validator) CertificatesReady() Check {
	return v.allOf("every certificate in "+chartNamespace+" is Ready", gvr("cert-manager.io", "v1", "certificates"), chartNamespace,
		func(certificate unstructured.Unstructured) string {
			if conditionTrue(certificate, "Ready") {
				return ""
			}
			return "not Ready"
		})
}

// HPAsReport fails on an HPA with no current metrics: metrics.k8s.io is
// served but empty for it, so it cannot scale.
func (v *Validator) HPAsReport() Check {
	return v.allOf("every HPA in "+chartNamespace+" reports metrics", gvr("autoscaling", "v2", "horizontalpodautoscalers"), chartNamespace,
		func(hpa unstructured.Unstructured) string {
			if metrics, _, _ := unstructured.NestedSlice(hpa.Object, statusField, "currentMetrics"); len(metrics) > 0 {
				return ""
			}
			return "no current metrics"
		})
}

// NodeMetricsServed holds when metrics-server lists node metrics.
func (v *Validator) NodeMetricsServed() Check {
	return v.allOf("metrics-server serves node metrics", gvr("metrics.k8s.io", "v1beta1", "nodes"), "", func(unstructured.Unstructured) string { return "" })
}

// allOf lists the resource and holds when there is at least one and reason
// returns "" for each. Otherwise it names the rest with their reasons.
func (v *Validator) allOf(name string, resource schema.GroupVersionResource, namespace string, reason func(unstructured.Unstructured) string) Check {
	return Check{Name: name, Probe: func(ctx context.Context) error {
		items, err := v.list(ctx, resource, namespace)
		if err != nil {
			return err
		}
		var failed []string
		for _, item := range items {
			if why := reason(item); why != "" {
				failed = append(failed, item.GetName()+" ("+why+")")
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("%d of %d %s: %s", len(failed), len(items), resource.Resource, truncated(failed))
		}
		return nil
	}}
}

// list fails on an empty list: no check expects one.
func (v *Validator) list(ctx context.Context, resource schema.GroupVersionResource, namespace string) ([]unstructured.Unstructured, error) {
	listed, err := v.dynamic.Resource(resource).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", resource.Resource, err)
	}
	if len(listed.Items) == 0 {
		return nil, fmt.Errorf("no %s in %s", resource.Resource, cmp.Or(namespace, "the cluster"))
	}
	return listed.Items, nil
}

func gvr(group, version, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
}

func conditionTrue(object unstructured.Unstructured, conditionType string) bool {
	conditions, _, _ := unstructured.NestedSlice(object.Object, statusField, "conditions")
	for _, raw := range conditions {
		if condition, ok := raw.(map[string]any); ok && condition["type"] == conditionType {
			return condition[statusField] == string(metav1.ConditionTrue)
		}
	}
	return false
}

// redactionProbe is a Pod outside NKTP that logs a credential, which the node
// Collector should mask. pod names the Pod and marks its line; secret is
// unique to the run, so finding it can only mean this run's line leaked.
type redactionProbe struct {
	pod    string
	secret string
}

// startRedactionProbe logs the line every few seconds rather than once, so a
// Collector that starts reading late still sees one.
func (v *Validator) startRedactionProbe(ctx context.Context) error {
	v.probe = redactionProbe{pod: probeName + "-" + uuid.NewString(), secret: "e2e-" + uuid.NewString()}
	line := fmt.Sprintf("%s Authorization: Bearer %s", v.probe.pod, v.probe.secret)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: v.probe.pod, Namespace: probeNamespace, Labels: map[string]string{"app.kubernetes.io/name": probeName}},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr.To(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To[int64](probeUser),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    "log",
				Image:   kube.KubectlImage,
				Command: []string{"sh", "-c", fmt.Sprintf("while true; do echo '%s'; sleep 5; done", line)},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
	if _, err := v.clientset.CoreV1().Pods(probeNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("starting the NKTP redaction probe %s/%s: %w", probeNamespace, pod.Name, err)
	}
	v.log.Info("started the NKTP redaction probe", "pod", probeNamespace+"/"+pod.Name)
	return nil
}

// stopRedactionProbe runs after the checks, often on an expired context.
func (v *Validator) stopRedactionProbe(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	err := v.clientset.CoreV1().Pods(probeNamespace).Delete(ctx, v.probe.pod, metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
	if err != nil && !apierrors.IsNotFound(err) {
		v.log.Error(err, "could not remove the NKTP redaction probe", "pod", probeNamespace+"/"+v.probe.pod)
	}
}

func truncated(names []string) string {
	if len(names) <= listLimit {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:listLimit], ", "), len(names)-listLimit)
}
