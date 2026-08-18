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

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

const (
	coreDNSName              = "coredns"
	kubeDNSServiceName       = "kube-dns"
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

			if err := applyKubeLocalDNSManifest(ctx, tenantClient); err != nil {
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

func applyKubeLocalDNSManifest(ctx context.Context, tenantClient ctrlclient.Client) error {
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
			ctrlclient.FieldOwner(constants.FieldManagerCWE),
			ctrlclient.ForceOwnership,
		); err != nil {
			return fmt.Errorf("failed to apply kube-local-dns %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
}

// ensureCoreDNSClusterDNSLabel adds dns-role=cluster-dns to CoreDNS pods.
// kubeadm installs CoreDNS as a Deployment; some environments use a DaemonSet.
func ensureCoreDNSClusterDNSLabel(ctx context.Context, tenantClient ctrlclient.Client) error {
	key := ctrlclient.ObjectKey{Namespace: metav1.NamespaceSystem, Name: coreDNSName}

	var daemonSet appsv1.DaemonSet
	err := tenantClient.Get(ctx, key, &daemonSet)
	switch {
	case err == nil:
		before := daemonSet.DeepCopy()
		if daemonSet.Spec.Template.Labels[dnsRoleLabelKey] == dnsRoleClusterDNS {
			return nil
		}
		if daemonSet.Spec.Template.Labels == nil {
			daemonSet.Spec.Template.Labels = map[string]string{}
		}
		daemonSet.Spec.Template.Labels[dnsRoleLabelKey] = dnsRoleClusterDNS
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
	if deployment.Spec.Template.Labels[dnsRoleLabelKey] == dnsRoleClusterDNS {
		return nil
	}
	if deployment.Spec.Template.Labels == nil {
		deployment.Spec.Template.Labels = map[string]string{}
	}
	deployment.Spec.Template.Labels[dnsRoleLabelKey] = dnsRoleClusterDNS
	if err := tenantClient.Patch(ctx, &deployment, ctrlclient.MergeFrom(before)); err != nil {
		return fmt.Errorf("failed to patch CoreDNS Deployment labels: %w", err)
	}
	return nil
}

// configureKubeDNSService rewires the kubeadm kube-dns Service for OVN prefer-local
// DNS (shared selector with node-local-dns) and moves Prometheus scrape to kube-dns-metrics.
func configureKubeDNSService(ctx context.Context, tenantClient ctrlclient.Client) error {
	var service corev1.Service
	key := ctrlclient.ObjectKey{Namespace: metav1.NamespaceSystem, Name: kubeDNSServiceName}
	if err := tenantClient.Get(ctx, key, &service); err != nil {
		return fmt.Errorf("failed to get kube-dns Service: %w", err)
	}

	before := service.DeepCopy()
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

	if service.Spec.Selector[dnsRoleLabelKey] != dnsRoleClusterDNS || len(service.Spec.Selector) != 1 {
		service.Spec.Selector = map[string]string{dnsRoleLabelKey: dnsRoleClusterDNS}
		changed = true
	}

	filteredPorts := slices.DeleteFunc(slices.Clone(service.Spec.Ports), func(port corev1.ServicePort) bool {
		return port.Name == metricsPortName
	})
	if len(filteredPorts) != len(service.Spec.Ports) {
		service.Spec.Ports = filteredPorts
		changed = true
	}

	if !changed {
		return nil
	}

	if err := tenantClient.Patch(ctx, &service, ctrlclient.MergeFrom(before)); err != nil {
		return fmt.Errorf("failed to patch kube-dns Service: %w", err)
	}
	return nil
}
