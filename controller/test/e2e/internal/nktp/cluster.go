// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nktp

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

const (
	probeName      = "nktp-e2e-redaction"
	probeNamespace = "default"
	// nobody, so the probe runs under the restricted Pod Security level.
	probeUser   = 65534
	stopTimeout = 30 * time.Second

	listLimit = 10
)

func certificatesResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
}

func collectorsResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "opentelemetry.io", Version: "v1beta1", Resource: "opentelemetrycollectors"}
}

func nodeMetricsResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}
}

// podsReady skips Pods that ran to completion.
func podsReady(ctx context.Context, t *target) error {
	pods, err := t.clientset.CoreV1().Pods(chartNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing Pods in %s: %w", chartNamespace, err)
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no Pods in %s", chartNamespace)
	}
	var notReady []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || kube.PodIsReady(pod) {
			continue
		}
		notReady = append(notReady, fmt.Sprintf("%s (%s)", pod.Name, pod.Status.Phase))
	}
	if len(notReady) > 0 {
		return fmt.Errorf("%d of %d Pods in %s are not Ready: %s", len(notReady), len(pods.Items), chartNamespace, list(notReady))
	}
	return nil
}

func certificatesReady(ctx context.Context, t *target) error {
	certificates, err := t.dynamic.Resource(certificatesResource()).Namespace(chartNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing certificates in %s: %w", chartNamespace, err)
	}
	if len(certificates.Items) == 0 {
		return fmt.Errorf("no certificates in %s", chartNamespace)
	}
	var notReady []string
	for i := range certificates.Items {
		if !conditionTrue(&certificates.Items[i], "Ready") {
			notReady = append(notReady, certificates.Items[i].GetName())
		}
	}
	if len(notReady) > 0 {
		return fmt.Errorf("%d of %d certificates in %s are not Ready: %s", len(notReady), len(certificates.Items), chartNamespace, list(notReady))
	}
	return nil
}

func conditionTrue(object *unstructured.Unstructured, conditionType string) bool {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if ok && condition["type"] == conditionType {
			return condition["status"] == string(metav1.ConditionTrue)
		}
	}
	return false
}

func collectorNames(ctx context.Context, t *target) ([]string, error) {
	collectors, err := t.dynamic.Resource(collectorsResource()).Namespace(chartNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing OpenTelemetryCollectors in %s: %w", chartNamespace, err)
	}
	names := make([]string, 0, len(collectors.Items))
	for i := range collectors.Items {
		names = append(names, collectors.Items[i].GetName())
	}
	slices.Sort(names)
	return names, nil
}

// hpasReport fails on an HPA with no current metrics: metrics.k8s.io is
// served but empty for it, so it cannot scale.
func hpasReport(ctx context.Context, t *target) error {
	hpas, err := t.clientset.AutoscalingV2().HorizontalPodAutoscalers(chartNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing HorizontalPodAutoscalers in %s: %w", chartNamespace, err)
	}
	if len(hpas.Items) == 0 {
		return fmt.Errorf("no HorizontalPodAutoscalers in %s", chartNamespace)
	}
	var blind []string
	for i := range hpas.Items {
		if len(hpas.Items[i].Status.CurrentMetrics) == 0 {
			blind = append(blind, hpas.Items[i].Name)
		}
	}
	if len(blind) > 0 {
		return fmt.Errorf("%d of %d HorizontalPodAutoscalers in %s report no current metrics: %s", len(blind), len(hpas.Items), chartNamespace, list(blind))
	}
	return nil
}

func nodeMetricsServed(ctx context.Context, t *target) error {
	nodes, err := t.dynamic.Resource(nodeMetricsResource()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing node metrics: %w", err)
	}
	if len(nodes.Items) == 0 {
		return errors.New("metrics.k8s.io lists no node metrics")
	}
	return nil
}

func internalCA(ctx context.Context, clientset kubernetes.Interface, namespace, secret string) (*x509.CertPool, error) {
	s, err := clientset.CoreV1().Secrets(namespace).Get(ctx, secret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the internal CA from Secret %s/%s: %w", namespace, secret, err)
	}
	pool := x509.NewCertPool()
	for _, key := range []string{corev1.ServiceAccountRootCAKey, corev1.TLSCertKey} {
		if pool.AppendCertsFromPEM(s.Data[key]) {
			return pool, nil
		}
	}
	return nil, fmt.Errorf("secret %s/%s holds no CA certificate under %s or %s", namespace, secret, corev1.ServiceAccountRootCAKey, corev1.TLSCertKey)
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
func (t *target) startRedactionProbe(ctx context.Context, log logr.Logger) error {
	t.redaction = redactionProbe{pod: probeName + "-" + uuid.NewString(), secret: "e2e-" + uuid.NewString()}
	line := fmt.Sprintf("%s Authorization: Bearer %s", t.redaction.pod, t.redaction.secret)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      t.redaction.pod,
			Namespace: probeNamespace,
			Labels:    map[string]string{"app.kubernetes.io/name": probeName},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: ptr.To[int64](0),
			AutomountServiceAccountToken:  ptr.To(false),
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
	if _, err := t.clientset.CoreV1().Pods(probeNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("starting the NKTP redaction probe %s/%s: %w", probeNamespace, pod.Name, err)
	}
	log.Info("started the NKTP redaction probe", "pod", probeNamespace+"/"+pod.Name)
	return nil
}

// stopRedactionProbe runs after the checks, often on an expired context.
func (t *target) stopRedactionProbe(ctx context.Context, log logr.Logger) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	err := t.clientset.CoreV1().Pods(probeNamespace).Delete(ctx, t.redaction.pod, metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
	if err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "could not remove the NKTP redaction probe", "pod", probeNamespace+"/"+t.redaction.pod)
	}
}

func list(names []string) string {
	if len(names) <= listLimit {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:listLimit], ", "), len(names)-listLimit)
}
