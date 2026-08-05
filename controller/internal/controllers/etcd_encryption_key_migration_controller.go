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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// EtcdEncryptionKeyMigrationController copies existing SWE-managed encryption
// key Secrets into the CWE namespace during the provisioning cutover. It is a
// transitional controller; https://jirasw.nvidia.com/browse/NKX-12736 will define the long-term canonical
// namespace for these Secrets.
type EtcdEncryptionKeyMigrationController struct {
	cfg       config.Config
	logger    *slog.Logger
	db        db.DB
	k8sClient ctrlclient.Client
}

func NewEtcdEncryptionKeyMigrationController(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance db.DB,
	k8sClient ctrlclient.Client,
) *EtcdEncryptionKeyMigrationController {
	return &EtcdEncryptionKeyMigrationController{
		cfg:       cfg,
		logger:    logger,
		db:        dbInstance,
		k8sClient: k8sClient,
	}
}

func (c *EtcdEncryptionKeyMigrationController) Reconcile(ctx context.Context, site db.Site) error {
	if c.cfg.EtcdEncryptionKeyMigrationDisabled {
		c.logger.Debug("Etcd encryption key migration is disabled", "siteID", site.ID)
		return nil
	}

	if c.cfg.EtcdEncryptionKeyLegacyNamespace == "" || c.cfg.EtcdEncryptionKeyLegacyNamespace == c.cfg.PodNamespace {
		c.logger.Debug(
			"Skipping etcd encryption key migration because legacy and destination namespaces are not distinct",
			"legacyNamespace", c.cfg.EtcdEncryptionKeyLegacyNamespace,
			"destinationNamespace", c.cfg.PodNamespace,
			"siteID", site.ID,
		)
		return nil
	}

	clusters, err := c.db.ListClusters(ctx, db.ClusterFilters{SiteID: &site.ID})
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}

	var reconcileErrs []error
	for _, cluster := range clusters {
		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue || !cluster.EtcdEncrypted {
			continue
		}
		if err := c.reconcileCluster(ctx, cluster); err != nil {
			c.logger.Error("Failed to migrate etcd encryption key Secret", "error", err, "clusterID", cluster.ID, "clusterName", cluster.Name)
			reconcileErrs = append(reconcileErrs, err)
		}
	}

	return errors.Join(reconcileErrs...)
}

func (c *EtcdEncryptionKeyMigrationController) reconcileCluster(ctx context.Context, cluster db.Cluster) error {
	secretName := constants.EtcdEncryptionKeySecretName(cluster.ID)

	destinationSecret := &corev1.Secret{}
	err := c.k8sClient.Get(ctx, ctrlclient.ObjectKey{Namespace: c.cfg.PodNamespace, Name: secretName}, destinationSecret)
	if err == nil {
		// Destination exists, so this cluster has already been migrated or CWE has
		// created newer key material. Do not overwrite it from the legacy Secret.
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting destination Secret %s/%s: %w", c.cfg.PodNamespace, secretName, err)
	}

	sourceSecret := &corev1.Secret{}
	if err := c.k8sClient.Get(ctx, ctrlclient.ObjectKey{Namespace: c.cfg.EtcdEncryptionKeyLegacyNamespace, Name: secretName}, sourceSecret); err != nil {
		if apierrors.IsNotFound(err) {
			c.logger.Warn(
				"Legacy etcd encryption key Secret is missing for encrypted cluster",
				"clusterID", cluster.ID,
				"clusterName", cluster.Name,
				"sourceNamespace", c.cfg.EtcdEncryptionKeyLegacyNamespace,
				"destinationNamespace", c.cfg.PodNamespace,
				"secretName", secretName,
			)
			return nil
		}
		return fmt.Errorf("getting legacy Secret %s/%s: %w", c.cfg.EtcdEncryptionKeyLegacyNamespace, secretName, err)
	}
	if len(sourceSecret.Data[constants.EtcdEncryptionKeyFieldActive]) == 0 {
		return fmt.Errorf("legacy Secret %s/%s is missing %q", c.cfg.EtcdEncryptionKeyLegacyNamespace, secretName, constants.EtcdEncryptionKeyFieldActive)
	}

	destinationCopy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: c.cfg.PodNamespace,
			Name:      secretName,
		},
		Type: sourceSecret.Type,
		Data: copySecretData(sourceSecret.Data),
	}
	if err := c.k8sClient.Create(ctx, destinationCopy); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating destination Secret %s/%s: %w", c.cfg.PodNamespace, secretName, err)
	}

	c.logger.Info(
		"Migrated etcd encryption key Secret",
		"clusterID", cluster.ID,
		"clusterName", cluster.Name,
		"sourceNamespace", c.cfg.EtcdEncryptionKeyLegacyNamespace,
		"destinationNamespace", c.cfg.PodNamespace,
		"secretName", secretName,
	)
	return nil
}

func copySecretData(data map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(data))
	for key, value := range data {
		out[key] = append([]byte(nil), value...)
	}
	return out
}
