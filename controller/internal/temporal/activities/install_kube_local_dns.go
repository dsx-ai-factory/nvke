// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

const (
	coreDNSName              = "coredns"
	kubeDNSServiceName       = "kube-dns"
	k8sAppLabelKey           = "k8s-app"
	kubeDNSAppLabel          = "kube-dns"
	dnsRoleLabelKey          = "dns-role"
	dnsRoleClusterDNS        = "cluster-dns"
	preferLocalAnnotationKey = "k8s.ovn.org/prefer-local-with-fallback"
	preferLocalAnnotationVal = "node-local-dns"
	prometheusPortAnnotation = "prometheus.io/port"
	prometheusScrapeAnnot    = "prometheus.io/scrape"
	metricsPortName          = "metrics"
)

//go:embed kube_local_dns.yaml
var kubeLocalDNSManifest []byte

type InstallKubeLocalDNSInput struct {
	ClusterID uuid.UUID

	ClusterProvisioningConditionMessage string
}

// InstallKubeLocalDNS applies the node-local DNS resources directly to a tenant cluster,
// then patches kubeadm-managed CoreDNS and the kube-dns Service for OVN prefer-local DNS.
func (a *Activities) InstallKubeLocalDNS(ctx context.Context, input InstallKubeLocalDNSInput) error {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[struct{}]{
		Message:         input.ClusterProvisioningConditionMessage,
		UpdateCondition: a.clusterProvisioningConditionMessageUpdater(input.ClusterID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (struct{}, error) {
			cluster, err := a.db.GetCluster(ctx, input.ClusterID)
			if err != nil {
				if errors.Is(err, db.ErrNotFound) {
					return struct{}{}, temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
				}
				return struct{}{}, fmt.Errorf("failed to get cluster %s: %w", input.ClusterID, err)
			}

			tenantClient, err := a.tenantClientForCluster(ctx, cluster)
			if err != nil {
				return struct{}{}, err
			}

			if err := applyKubeLocalDNSManifest(ctx, tenantClient, a.cfg.FieldManagerName); err != nil {
				return struct{}{}, err
			}
			if err := ensureCoreDNSClusterDNSLabel(ctx, tenantClient); err != nil {
				return struct{}{}, err
			}
			if err := configureKubeDNSService(ctx, tenantClient); err != nil {
				return struct{}{}, err
			}

			return struct{}{}, nil
		},
	}

	_, err := conditionUpdateWrapper.Run(ctx)
	return err
}

func applyKubeLocalDNSManifest(ctx context.Context, tenantClient ctrlclient.Client, fieldManagerName string) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(kubeLocalDNSManifest), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("failed to decode kube-local-dns manifest: %w", err)
		}
		if len(obj.Object) == 0 {
			continue
		}

		if err := tenantClient.Apply(
			ctx,
			ctrlclient.ApplyConfigurationFromUnstructured(obj),
			ctrlclient.FieldOwner(fieldManagerName),
			ctrlclient.ForceOwnership,
		); err != nil {
			return fmt.Errorf("failed to apply kube-local-dns %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
}

// ensureCoreDNSClusterDNSLabel adds dns-role=cluster-dns to the CoreDNS
// workload template and to already-running CoreDNS pods. Template-only updates
// roll out asynchronously; in-place pod labels keep kube-dns backends matching
// as soon as those pods are Ready.
func ensureCoreDNSClusterDNSLabel(ctx context.Context, tenantClient ctrlclient.Client) error {
	if err := ensureCoreDNSTemplateClusterDNSLabel(ctx, tenantClient); err != nil {
		return err
	}
	return labelRunningCoreDNSPods(ctx, tenantClient)
}

func ensureCoreDNSTemplateClusterDNSLabel(ctx context.Context, tenantClient ctrlclient.Client) error {
	key := ctrlclient.ObjectKey{Namespace: metav1.NamespaceSystem, Name: coreDNSName}

	var daemonSet appsv1.DaemonSet
	err := tenantClient.Get(ctx, key, &daemonSet)
	switch {
	case err == nil:
		before := daemonSet.DeepCopy()
		labels, changed := withClusterDNSLabel(daemonSet.Spec.Template.Labels)
		if !changed {
			return nil
		}
		daemonSet.Spec.Template.Labels = labels
		if err := tenantClient.Patch(ctx, &daemonSet, ctrlclient.MergeFrom(before)); err != nil {
			return fmt.Errorf("failed to patch CoreDNS DaemonSet labels: %w", err)
		}
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("failed to get CoreDNS DaemonSet: %w", err)
	}

	var deployment appsv1.Deployment
	if err := tenantClient.Get(ctx, key, &deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("CoreDNS DaemonSet/Deployment %q not found in %s", coreDNSName, metav1.NamespaceSystem)
		}
		return fmt.Errorf("failed to get CoreDNS Deployment: %w", err)
	}

	before := deployment.DeepCopy()
	labels, changed := withClusterDNSLabel(deployment.Spec.Template.Labels)
	if !changed {
		return nil
	}
	deployment.Spec.Template.Labels = labels
	if err := tenantClient.Patch(ctx, &deployment, ctrlclient.MergeFrom(before)); err != nil {
		return fmt.Errorf("failed to patch CoreDNS Deployment labels: %w", err)
	}
	return nil
}

func labelRunningCoreDNSPods(ctx context.Context, tenantClient ctrlclient.Client) error {
	var pods corev1.PodList
	if err := tenantClient.List(ctx, &pods,
		ctrlclient.InNamespace(metav1.NamespaceSystem),
		ctrlclient.MatchingLabels{k8sAppLabelKey: kubeDNSAppLabel},
	); err != nil {
		return fmt.Errorf("failed to list CoreDNS pods: %w", err)
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		before := pod.DeepCopy()
		labels, changed := withClusterDNSLabel(pod.Labels)
		if !changed {
			continue
		}
		pod.Labels = labels
		if err := tenantClient.Patch(ctx, pod, ctrlclient.MergeFrom(before)); err != nil {
			return fmt.Errorf("failed to patch CoreDNS pod %s labels: %w", pod.Name, err)
		}
	}
	return nil
}

func withClusterDNSLabel(labels map[string]string) (map[string]string, bool) {
	if labels[dnsRoleLabelKey] == dnsRoleClusterDNS {
		return labels, false
	}
	if labels == nil {
		labels = map[string]string{}
	}
	labels[dnsRoleLabelKey] = dnsRoleClusterDNS
	return labels, true
}

// configureKubeDNSService rewires the kubeadm kube-dns Service for OVN prefer-local
// DNS (shared selector with node-local-dns) and moves Prometheus scrape to kube-dns-metrics.
// The selector is rewritten only after at least one Ready dns-role=cluster-dns pod exists
// so kube-dns does not lose all endpoints during the CoreDNS / node-local-dns rollout.
func configureKubeDNSService(ctx context.Context, tenantClient ctrlclient.Client) error {
	var service corev1.Service
	key := ctrlclient.ObjectKey{Namespace: metav1.NamespaceSystem, Name: kubeDNSServiceName}
	if err := tenantClient.Get(ctx, key, &service); err != nil {
		return fmt.Errorf("failed to get kube-dns Service: %w", err)
	}

	before := service.DeepCopy()
	changed := overlayKubeDNSService(&service)
	rewritten := kubeDNSSelectorRewritten(&service)

	// Selector cutover already happened on a prior run: nothing left to do
	// but make sure the annotation/port overlay above is persisted.
	if rewritten && !changed {
		return nil
	}
	if rewritten {
		return patchKubeDNSService(ctx, tenantClient, &service, before)
	}

	// Selector not yet cut over. Only allowed once a dns-role=cluster-dns
	// pod is Ready, so kube-dns never loses all its endpoints.
	ready, err := hasReadyClusterDNSPod(ctx, tenantClient)
	if err != nil {
		return err
	}

	// Not ready yet: still persist the overlay (safe, doesn't affect
	// routing) so a later retry has less left to do, then bail out with
	// the "not ready" error so Temporal retries this activity.
	if !ready && changed {
		if err := patchKubeDNSService(ctx, tenantClient, &service, before); err != nil {
			return err
		}
	}
	if !ready {
		return fmt.Errorf("waiting for a Ready pod with %s=%s before rewriting kube-dns selector", dnsRoleLabelKey, dnsRoleClusterDNS)
	}

	// A Ready cluster-dns pod exists: safe to cut kube-dns over to it.
	service.Spec.Selector = map[string]string{dnsRoleLabelKey: dnsRoleClusterDNS}
	return patchKubeDNSService(ctx, tenantClient, &service, before)
}

func overlayKubeDNSService(service *corev1.Service) bool {
	changed := false
	if service.Annotations == nil {
		service.Annotations = map[string]string{}
	}
	if _, ok := service.Annotations[prometheusPortAnnotation]; ok {
		delete(service.Annotations, prometheusPortAnnotation)
		changed = true
	}
	if _, ok := service.Annotations[prometheusScrapeAnnot]; ok {
		delete(service.Annotations, prometheusScrapeAnnot)
		changed = true
	}
	if service.Annotations[preferLocalAnnotationKey] != preferLocalAnnotationVal {
		service.Annotations[preferLocalAnnotationKey] = preferLocalAnnotationVal
		changed = true
	}
	if len(service.Annotations) == 0 {
		service.Annotations = nil
	}

	filteredPorts := slices.DeleteFunc(slices.Clone(service.Spec.Ports), func(port corev1.ServicePort) bool {
		return port.Name == metricsPortName
	})
	if len(filteredPorts) != len(service.Spec.Ports) {
		service.Spec.Ports = filteredPorts
		changed = true
	}
	return changed
}

func kubeDNSSelectorRewritten(service *corev1.Service) bool {
	return service.Spec.Selector[dnsRoleLabelKey] == dnsRoleClusterDNS && len(service.Spec.Selector) == 1
}

func hasReadyClusterDNSPod(ctx context.Context, tenantClient ctrlclient.Client) (bool, error) {
	var pods corev1.PodList
	if err := tenantClient.List(ctx, &pods,
		ctrlclient.InNamespace(metav1.NamespaceSystem),
		ctrlclient.MatchingLabels{dnsRoleLabelKey: dnsRoleClusterDNS},
	); err != nil {
		return false, fmt.Errorf("failed to list cluster-dns pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		if podReady(pod) {
			return true, nil
		}
	}
	return false, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func patchKubeDNSService(ctx context.Context, tenantClient ctrlclient.Client, service, before *corev1.Service) error {
	if err := tenantClient.Patch(ctx, service, ctrlclient.MergeFrom(before)); err != nil {
		return fmt.Errorf("failed to patch kube-dns Service: %w", err)
	}
	return nil
}
