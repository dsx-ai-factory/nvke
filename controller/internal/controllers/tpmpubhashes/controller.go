// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tpmpubhashes

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
)

// Controller is responsible for reconciling the tpm-pubhashes configmap.
type Controller struct {
	cfg         config.Config
	k8sClient   client.Client
	logger      *slog.Logger
	trustDomain string
}

// NewController creates a new Controller.
func NewController(
	cfg config.Config,
	logger *slog.Logger,
	k8sClient client.Client,
	trustDomain string,
) *Controller {
	return &Controller{
		cfg:         cfg,
		k8sClient:   k8sClient,
		logger:      logger,
		trustDomain: trustDomain,
	}
}

// SetupWithManager wires the controller into controller-runtime.
func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	isTPMPubhashesConfigMap := func(obj client.Object) bool {
		return obj.GetNamespace() == c.cfg.SpireServerNamespace && obj.GetName() == c.cfg.SpireTPMPubhashesConfigMapName
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&spirev1alpha1.ClusterStaticEntry{}).
		// Watch the derived ConfigMap only to repair drift; ClusterStaticEntries remain the source of truth.
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
				if !isTPMPubhashesConfigMap(obj) {
					return nil
				}
				return []reconcile.Request{{NamespacedName: client.ObjectKey{
					Namespace: obj.GetNamespace(),
					Name:      obj.GetName(),
				}}}
			}),
			builder.WithPredicates(predicate.NewPredicateFuncs(isTPMPubhashesConfigMap)),
		).
		Named("tpm-pubhashes").
		Complete(c)
}

// Reconcile the tpm-pubhashes configmap, for each Machine-owned ClusterStaticEntry:
// - If the ClusterStaticEntry has a parentID that is a tpm spire agent path (/spire/agent/tpm/), add the pubhash to the configmap.
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := c.logger.With("requestNamespace", req.Namespace, "requestName", req.Name)
	if err := c.reconcileTpmPubhashesCm(ctx); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to reconcile tpm pubhashes configmap: %w", err)
	}

	logger.Debug("Reconciled TPM pubhashes configmap")
	return reconcile.Result{}, nil
}

func (c *Controller) reconcileTpmPubhashesCm(ctx context.Context) error {
	// List ClusterStaticEntries managed by CWE's machine-spire-entry-controller.
	clusterStaticEntries := &spirev1alpha1.ClusterStaticEntryList{}
	err := c.k8sClient.List(ctx, clusterStaticEntries, client.MatchingLabels{
		constants.LabelKeyNkeManagedBy: constants.FieldManagerMachineSpireEntry,
	})
	if err != nil {
		return fmt.Errorf("failed to list cluster static entries: %w", err)
	}

	desiredPubHashes := make(map[string]struct{})
	for _, clusterStaticEntry := range clusterStaticEntries.Items {
		// Only process entries for our SPIRE installation.
		if clusterStaticEntry.Spec.ClassName != c.cfg.SpireClassName {
			continue
		}

		pubHash := extractTPMPubHash(clusterStaticEntry.Spec.ParentID)
		if pubHash == "" {
			continue
		}
		desiredPubHashes[pubHash] = struct{}{}
	}

	data := make(map[string]string, len(desiredPubHashes))
	for pubHash := range desiredPubHashes {
		// A data value is added for each pubhash, it is not currently used, but will allow more granular control in the future, if necessary.
		// e.g. "<pubhash>": "nke.nvidia.com/managed-by=machine-spire-entry-controller"
		value := fmt.Sprintf("%s=%s", constants.LabelKeyNkeManagedBy, constants.FieldManagerMachineSpireEntry)
		data[pubHash] = value
	}

	configMap := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.cfg.SpireTPMPubhashesConfigMapName,
			Namespace: c.cfg.SpireServerNamespace,
		},
		Data: data,
	}

	c.logger.Debug("Applying TPM pubhashes configmap", "desiredData", configMap.Data, "namespace", configMap.Namespace, "name", configMap.Name)
	if err := c.k8sClient.Patch(
		ctx,
		configMap,
		client.Apply,
		client.FieldOwner(c.cfg.FieldManagerName),
		client.ForceOwnership,
	); err != nil {
		return fmt.Errorf("failed to apply TPM pubhashes configmap: %w", err)
	}

	return nil
}

func extractTPMPubHash(parentID string) string {
	// Parse the parentID as a SPIFFE ID to validate it is valid.
	spiffeID, err := spiffeid.FromString(parentID)
	if err != nil {
		return ""
	}

	spiffePath := spiffeID.Path()

	// Only find entries that have the tpm spire agent path (/spire/agent/tpm/) in the string.
	if !strings.HasPrefix(spiffePath, constants.TPMSpireAgentPath) {
		return ""
	}

	// Takes the substring that starts right after the tpm spire agent path (/spire/agent/tpm/).
	// e.g. spiffe://nke-site-forge-az24-dev7.nke.nvidia.com/spire/agent/tpm/ABCDEF -> ABCDEF
	pubHash := strings.TrimPrefix(spiffePath, constants.TPMSpireAgentPath)

	// Trim any extra path segments after the hash.
	// e.g. ABCDEF/extra/stuff -> ABCDEF
	if slashIndex := strings.Index(pubHash, "/"); slashIndex != -1 {
		pubHash = pubHash[:slashIndex]
	}

	return pubHash
}
