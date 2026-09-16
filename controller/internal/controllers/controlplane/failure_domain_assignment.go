// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"

	"k8s.io/utils/ptr"

	"github.com/dsx-ai-factory/nke/controller/internal/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// prepareFailureDomainAssignments persists placement decisions for control-plane nodes.
func (r *reconciler) prepareFailureDomainAssignments(ctx context.Context, controlPlane *controlPlane) error {
	if !r.cfg.UseControlPlaneControllerForControlPlane() {
		return nil
	}

	r.domainAssignments = r.domainAssignments[:0]
	r.unknownDomains = r.unknownDomains[:0]
	r.retryFailureDomains = false
	pending := []*db.Node{}

	for i := range controlPlane.nodes {
		node := &controlPlane.nodes[i]
		if isMarkedForDeletion(*node) {
			continue
		}

		if node.FailureDomain == nil && r.failureDomainSnapshot.discoveryErr == nil {
			if observed := r.failureDomainSnapshot.observedByNodeName[node.Name]; observed != "" {
				node.FailureDomain = ptr.To(observed)
			} else if len(r.failureDomainSnapshot.eligible) == 0 {
				// Empty is a completed decision: this provider does not support
				// control-plane failure domains.
				node.FailureDomain = ptr.To("")
			}
			if node.FailureDomain != nil {
				if _, err := r.db.UpdateNode(ctx, node); err != nil {
					return fmt.Errorf("persist failure domain for node %s: %w", node.ID, err)
				}
			}
		}

		if node.FailureDomain != nil {
			if *node.FailureDomain != "" {
				r.domainAssignments = append(r.domainAssignments, *node.FailureDomain)
			}
			continue
		}
		if isProvisioned(*node) || !isPending(*node) {
			r.unknownDomains = append(r.unknownDomains, node.Name)
			continue
		}
		pending = append(pending, node)
	}

	if r.failureDomainSnapshot.discoveryErr != nil || len(r.unknownDomains) > 0 {
		r.retryFailureDomains = len(pending) > 0 || len(r.unknownDomains) > 0
		return nil
	}
	for _, node := range pending {
		failureDomain, err := r.selectFailureDomain()
		if err != nil {
			return err
		}
		node.FailureDomain = failureDomain
		if _, err := r.db.UpdateNode(ctx, node); err != nil {
			return fmt.Errorf("persist failure domain for pending node %s: %w", node.ID, err)
		}
		if *failureDomain != "" {
			r.domainAssignments = append(r.domainAssignments, *failureDomain)
		}
	}
	return nil
}

// selectFailureDomain chooses the eligible failure domain using the chosen
// FailureDomainSelectionAlgorithm implementation
func (r *reconciler) selectFailureDomain() (*string, error) {
	if r.failureDomainSnapshot.discoveryErr != nil {
		return nil, r.failureDomainSnapshot.discoveryErr
	}
	if len(r.unknownDomains) > 0 {
		return nil, fmt.Errorf("waiting for observed failure domains for existing control-plane nodes: %v", r.unknownDomains)
	}

	domain, err := capi.LeastUsedFailureDomainSelection(r.failureDomainSnapshot.eligible, r.domainAssignments)
	if err != nil {
		return nil, fmt.Errorf("select control-plane failure domain: %w", err)
	}
	return ptr.To(domain), nil
}
