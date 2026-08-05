// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"

	"github.com/spiffe/go-spiffe/v2/spiffeid"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TpmPubhashesCmController is responsible for reconciling the tpm-pubhashes configmap.
type TpmPubhashesCmController struct {
	cfg         config.Config
	k8sClient   client.Client
	logger      *slog.Logger
	trustDomain string
}

func NewTpmPubhashesCmController(
	cfg config.Config,
	k8sClient client.Client,
	logger *slog.Logger,
	trustDomain string,
) *TpmPubhashesCmController {
	return &TpmPubhashesCmController{
		cfg:         cfg,
		k8sClient:   k8sClient,
		logger:      logger,
		trustDomain: trustDomain,
	}
}

// Reconcile the tpm-pubhashes configmap, for each ClusterStaticEntry managed by CWE:
// - If the ClusterStaticEntry has a parentID that is a tpm spire agent path (/spire/agent/tpm/), add the pubhash to the configmap.
func (c *TpmPubhashesCmController) Reconcile(ctx context.Context) error {
	if err := c.reconcileTpmPubhashesCm(ctx); err != nil {
		return fmt.Errorf("failed to reconcile tpm pubhashes configmap: %w", err)
	}

	return nil
}

func (c *TpmPubhashesCmController) reconcileTpmPubhashesCm(ctx context.Context) error {
	// List ClusterStaticEntries managed by CWE.
	clusterStaticEntries := &spirev1alpha1.ClusterStaticEntryList{}
	err := c.k8sClient.List(ctx, clusterStaticEntries, client.MatchingLabels{
		constants.LabelKeyNkeManagedBy: constants.LabelValueNkeManagedBy,
	})
	if err != nil {
		return fmt.Errorf("failed to list cluster static entries: %w", err)
	}

	var errs []error
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
		// e.g. "<pubhash>": "nke.nvidia.com/managed-by=cloud-workflow-engine"
		value := fmt.Sprintf("%s=%s", constants.LabelKeyNkeManagedBy, constants.LabelValueNkeManagedBy)
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
		client.FieldOwner(constants.FieldManagerCWE),
		client.ForceOwnership,
	); err != nil {
		errs = append(errs, fmt.Errorf("failed to apply TPM pubhashes configmap: %w", err))
	}

	return errors.Join(errs...)
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
