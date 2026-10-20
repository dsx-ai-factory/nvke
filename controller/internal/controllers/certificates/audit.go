// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certificates

import (
	"context"
	"slices"
	"time"

	catalog "github.com/dsx-ai-factory/nvke/agent/pkg/certificates"

	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	inspectionCertificateAvailable = "available"
)

// recordInspectionAudit emits independent observations, not renewal completion
// evidence. A failed probe must never report retained fingerprints as fresh.
// An observation that repeats the previous inspection's outcome, reason and
// fingerprints for the same credential is not recorded again, so an unchanged
// node, including one whose probe keeps failing the same way such as an agent
// without the inspection RPC, does not add rows every inspection interval.
func (c *Controller) recordInspectionAudit(ctx context.Context, node db.Node, previous, inspection *db.CertificateInspection) {
	if c.audit == nil || inspection == nil {
		return
	}
	var before []credentialaudit.Event
	// Every failed probe maps to the same audit reason, so a change of the
	// inspection's own reason, for example from unsupported to unavailable, is
	// recorded as a new observation.
	if previous != nil && previous.Reason == inspection.Reason {
		before = inspectionAuditEvents(node, previous)
	}
	for _, event := range inspectionAuditEvents(node, inspection) {
		if slices.ContainsFunc(before, func(prior credentialaudit.Event) bool {
			return sameInspectionObservation(prior, event)
		}) {
			continue
		}
		c.audit.Record(ctx, event)
	}
}

func sameInspectionObservation(a, b credentialaudit.Event) bool {
	return a.Credential == b.Credential && a.Outcome == b.Outcome && a.Reason == b.Reason &&
		slices.Equal(sortedFingerprints(a.Fingerprints), sortedFingerprints(b.Fingerprints))
}

func sortedFingerprints(fingerprints []string) []string {
	sorted := slices.Clone(fingerprints)
	slices.Sort(sorted)
	return sorted
}

func inspectionAuditEvents(node db.Node, inspection *db.CertificateInspection) []credentialaudit.Event {
	target := credentialaudit.Target{ClusterID: node.ClusterID.String(), NodeID: node.ID.String()}
	base := credentialaudit.Event{
		Timestamp: inspection.AttemptedAt, Action: credentialaudit.ActionCheck, Plane: credentialaudit.PlaneT2,
		Target: target, Actor: credentialaudit.Actor{Kind: credentialaudit.ActorAutomatic},
		Outcome: credentialaudit.OutcomeSuccess, Reason: credentialaudit.ReasonObserved,
		Correlation: credentialaudit.Correlation{OperationHash: credentialaudit.Hash("operation",
			node.ClusterID.String()+"/"+node.ID.String()+"/"+inspection.AttemptedAt.Format(time.RFC3339Nano))},
	}
	serving := base
	serving.Credential = credentialaudit.TypeServing
	serving.Outcome, serving.Reason = credentialaudit.OutcomeFailure, credentialaudit.ReasonUnavailable
	leaves := base
	leaves.Credential = credentialaudit.TypeKubeadm
	if !inspection.Available {
		leaves.Outcome, leaves.Reason = credentialaudit.OutcomeFailure, credentialaudit.ReasonUnavailable
	} else {
		for _, certificate := range inspection.Certificates {
			switch certificate.Credential {
			case "kubelet-serving":
				if certificate.State == inspectionCertificateAvailable {
					serving.Outcome, serving.Reason = credentialaudit.OutcomeSuccess, credentialaudit.ReasonObserved
					if certificate.CertificateSHA256 != "" {
						serving.Fingerprints = []string{certificate.CertificateSHA256}
					}
				}
			case "kubelet-client", "kubernetes-ca", "front-proxy-ca", "etcd-ca":
				// These are inspected for metrics but are outside this renewed leaf bundle.
			default:
				if certificate.State != inspectionCertificateAvailable && catalog.Required(certificate.Credential) {
					leaves.Outcome, leaves.Reason = credentialaudit.OutcomeFailure, credentialaudit.ReasonUnavailable
				}
				if certificate.State == inspectionCertificateAvailable && certificate.CertificateSHA256 != "" {
					leaves.Fingerprints = append(leaves.Fingerprints, certificate.CertificateSHA256)
				}
			}
		}
	}
	if node.ControlPlane {
		return []credentialaudit.Event{leaves, serving}
	}
	return []credentialaudit.Event{serving}
}
