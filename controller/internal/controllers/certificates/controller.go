// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package certificates reconciles persisted, read-only certificate observations.
package certificates

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	catalog "github.com/dsx-ai-factory/nvke/agent/pkg/certificates"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	inspectionInvalidResponse = "invalid_response"
	inspectionUnsupported     = "unsupported"

	// A failed or incomplete inspection is repeated after inspectionRetryBase,
	// doubling until the retry count reaches maxInspectionRetries and never
	// beyond the inspection interval.
	inspectionRetryBase  = time.Minute
	maxInspectionRetries = 6
)

type inspector interface {
	InspectKubernetesCertificates(ctx context.Context, node db.Node) (*pb.InspectKubernetesCertificatesResponse, error)
}

// Controller keeps network inspection outside Prometheus scrapes.
type Controller struct {
	cfg      config.Config
	database *dbwrapper.DB
	agent    inspector
}

// NewController constructs the certificate observation controller.
func NewController(cfg config.Config, database *dbwrapper.DB, agent inspector) *Controller {
	return &Controller{cfg: cfg, database: database, agent: agent}
}

// SetupWithManager watches only non-deleting nodes.
func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(mgr, "certificate-inspection", c, dbwrapper.NewNodeSource(c.database, dbfilters.Node{Deleting: ptr.To(false)}))
}

// Reconcile persists the latest inspection without modifying credentials.
func (c *Controller) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	node, err := c.database.GetNode(ctx, key.ID)
	if errors.Is(err, db.ErrNotFound) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("get inspection node: %w", err)
	}
	if node.Conditions.Deleting.Status == db.ConditionStatusTrue || node.Conditions.Joined.Status != db.ConditionStatusTrue {
		return reconcile.Result{}, nil
	}
	now := time.Now().UTC()
	if previous := node.CertificateInspection; previous != nil {
		if due := previous.AttemptedAt.Add(c.nextDelay(previous)); now.Before(due) {
			return reconcile.Result{RequeueAfter: due.Sub(now)}, nil
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	response, probeErr := c.agent.InspectKubernetesCertificates(probeCtx, node)
	cancel()
	observation := inspectionObservation(node.CertificateInspection, response, probeErr, node.ControlPlane, time.Now().UTC())
	if needsEarlyRetry(observation) {
		observation.Retries = 1
		if previous := node.CertificateInspection; previous != nil {
			observation.Retries = min(previous.Retries+1, maxInspectionRetries)
		}
	}
	err = c.database.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		locked, err := tx.GetNodeForUpdate(ctx, node.ID)
		if err != nil {
			return err
		}
		if locked.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return nil
		}
		locked.CertificateInspection = observation
		_, err = tx.UpdateNode(ctx, &locked)
		return err
	})
	if errors.Is(err, db.ErrNotFound) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("persist certificate inspection: %w", err)
	}
	return reconcile.Result{RequeueAfter: c.nextDelay(observation)}, nil
}

// nextDelay is how long after an attempt the node is inspected again: the full
// interval after a complete or unsupported result, otherwise a short backoff, so
// a transient failure does not leave the node unavailable for a whole interval.
func (c *Controller) nextDelay(observation *db.CertificateInspection) time.Duration {
	if observation.Retries <= 0 {
		return c.cfg.CertificateInspectionInterval
	}
	return min(inspectionRetryBase<<(observation.Retries-1), c.cfg.CertificateInspectionInterval)
}

// needsEarlyRetry reports a failed probe, or a successful one that found a
// required credential missing or invalid, which a kubelet still bootstrapping
// TLS can produce. An agent without the inspection RPC is not retried early.
func needsEarlyRetry(observation *db.CertificateInspection) bool {
	if !observation.Available {
		return observation.Reason != inspectionUnsupported
	}
	for _, item := range observation.Certificates {
		if item.State != "available" && catalog.Required(item.Credential) {
			return true
		}
	}
	return false
}

func inspectionObservation(previous *db.CertificateInspection, response *pb.InspectKubernetesCertificatesResponse, probeErr error, controlPlane bool, now time.Time) *db.CertificateInspection {
	observation := &db.CertificateInspection{AttemptedAt: now, Reason: "unavailable"}
	if previous != nil {
		observation.ObservedAt = previous.ObservedAt
		observation.Certificates = previous.Certificates
	}
	if status.Code(probeErr) == codes.Unimplemented {
		observation.Reason = inspectionUnsupported
		return observation
	}
	if probeErr != nil || response == nil {
		return observation
	}
	parsed := make([]db.CertificateObservation, 0, len(catalog.Names(controlPlane)))
	seen := map[string]bool{}
	for _, item := range response.GetCertificates() {
		if !catalog.Supports(item.GetCredential(), controlPlane) || seen[item.GetCredential()] {
			observation.Reason = inspectionInvalidResponse
			return observation
		}
		seen[item.GetCredential()] = true
		entry := db.CertificateObservation{Credential: item.GetCredential()}
		switch item.GetState() {
		case pb.KubernetesCertificateObservation_STATE_AVAILABLE:
			if item.GetNotAfterUnixSeconds() <= 0 {
				observation.Reason = inspectionInvalidResponse
				return observation
			}
			if fingerprint := item.GetCertificateSha256(); fingerprint != "" {
				decoded, err := hex.DecodeString(fingerprint)
				if err != nil || len(decoded) != 32 {
					observation.Reason = inspectionInvalidResponse
					return observation
				}
				entry.CertificateSHA256 = fingerprint
			}
			entry.State = "available"
			entry.NotAfter = time.Unix(item.GetNotAfterUnixSeconds(), 0).UTC()
		case pb.KubernetesCertificateObservation_STATE_MISSING:
			entry.State = "missing"
		case pb.KubernetesCertificateObservation_STATE_INVALID:
			entry.State = "invalid"
		case pb.KubernetesCertificateObservation_STATE_UNSPECIFIED:
			observation.Reason = inspectionInvalidResponse
			return observation
		default:
			observation.Reason = inspectionInvalidResponse
			return observation
		}
		parsed = append(parsed, entry)
	}
	if len(parsed) != len(catalog.Names(controlPlane)) {
		observation.Reason = inspectionInvalidResponse
		return observation
	}
	observation.Available = true
	observation.Reason = ""
	observation.ObservedAt = &now
	observation.Certificates = parsed
	return observation
}
