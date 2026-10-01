// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

const (
	certificateRenewalFailureMessageLimit = 1024
)

// BeginCertificateRenewalInput binds cluster ownership to a stable workflow ID.
// ScheduledFor identifies the due-time cycle; forced cycles leave it nil.
type BeginCertificateRenewalInput struct {
	ClusterID    uuid.UUID
	CycleID      string
	Forced       bool
	ScheduledFor *time.Time
}

// BeginCertificateRenewalResult lets redelivery of a completed cycle return its
// recorded outcome without dispatching node work again.
type BeginCertificateRenewalResult struct {
	AlreadyCompleted bool
	Outcome          db.CertificateRenewalOutcome
}

// BeginCertificateRenewal acquires the cluster mutation lock and records cycle
// acceptance in one row-locked transaction. Repeating the same cycle may reuse
// its acceptance or success, but a failed cycle is not reopened. Callers must
// resolve this activity's result even after cancellation: acceptance may already
// have committed and require failure cleanup.
func (a *Activities) BeginCertificateRenewal(
	ctx context.Context,
	input BeginCertificateRenewalInput,
) (BeginCertificateRenewalResult, error) {
	var result BeginCertificateRenewalResult
	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, input.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}

		if status := cluster.Conditions.CertificateRenewal; status != nil && status.CycleID == input.CycleID {
			switch status.State {
			case db.CertificateRenewalStateAccepted:
				if update := cluster.Conditions.UpdateInProgress; update != nil && update.Type == db.OperationTypeCertRenewal {
					return nil
				}
				return certificateRenewalPreconditionError("certificate-renewal cycle lost its cluster mutation lock")
			case db.CertificateRenewalStateSucceeded:
				result.AlreadyCompleted = true
				result.Outcome = status.Outcome
				return nil
			case db.CertificateRenewalStateFailed:
				return certificateRenewalPreconditionError("certificate-renewal cycle already failed")
			}
		}

		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return certificateRenewalPreconditionError("cannot renew certificates while cluster deletion is in progress")
		}
		if cluster.Conditions.Provisioned.Status != db.ConditionStatusTrue {
			return certificateRenewalPreconditionError("cannot renew certificates until the cluster is provisioned")
		}
		if update := cluster.Conditions.UpdateInProgress; update != nil {
			return certificateRenewalPreconditionError(
				fmt.Sprintf("cannot renew certificates while %s is in progress", update.Type),
			)
		}
		if cluster.Conditions.ControlPlaneUpgrading.Status == db.ConditionStatusTrue {
			return certificateRenewalPreconditionError("cannot renew certificates while a control-plane upgrade is in progress")
		}
		if cluster.Conditions.CPNodesRecycling.Status == db.ConditionStatusTrue {
			return certificateRenewalPreconditionError("cannot renew certificates while control-plane node recycling is in progress")
		}

		startedAt := time.Now().UTC()
		db.SetUpdateInProgress(&cluster, db.OperationTypeCertRenewal, "renewing Kubernetes certificates", startedAt)
		cluster.Conditions.CertificateRenewal = &db.CertificateRenewalStatus{
			CycleID:      input.CycleID,
			State:        db.CertificateRenewalStateAccepted,
			Forced:       input.Forced,
			ScheduledFor: input.ScheduledFor,
			StartedAt:    startedAt,
		}
		if _, err := tx.UpdateCluster(ctx, &cluster); err != nil {
			return fmt.Errorf("failed to mark certificate renewal accepted: %w", err)
		}
		return nil
	})
	if err != nil {
		return BeginCertificateRenewalResult{}, err
	}
	return result, nil
}

func certificateRenewalPreconditionError(message string) error {
	return temporal.NewNonRetryableApplicationError(message, "CertificateRenewalPrecondition", nil)
}

// ValidateCertificateRenewalHealthInput carries the cycle's fixed node snapshot.
// PostflightNodeID adds mirror-pod checks for the node just renewed; omitting it
// selects preflight checks, not permission to refresh the snapshot.
type ValidateCertificateRenewalHealthInput struct {
	ClusterID        uuid.UUID
	ExpectedNodeIDs  []uuid.UUID
	PostflightNodeID *uuid.UUID
}

// ValidateCertificateRenewalHealth gates the rolling sequence on unchanged node
// membership, capacity to lose one voter (waived for a single control-plane
// node, whose renewal is inherently disruptive), DB and live Kubernetes readiness, and
// the shared local-API rollout threshold. Postflight also checks the renewed
// node's mirror pods. Exact etcd voter membership and endpoint health are checked
// by the node executor; this check alone does not prove dispatched work terminal.
func (a *Activities) ValidateCertificateRenewalHealth(
	ctx context.Context,
	input ValidateCertificateRenewalHealthInput,
) error {
	nodes, err := a.controlPlaneNodes(ctx, input.ClusterID)
	if err != nil {
		return err
	}
	if err := validateCertificateRenewalNodeSet(nodes, input.ExpectedNodeIDs); err != nil {
		return err
	}

	// A single control-plane node has no peer quorum to protect: renewing it
	// necessarily restarts its only etcd and kube-apiserver.
	quorum := len(nodes)/2 + 1
	if len(nodes) > 1 && len(nodes)-1 < quorum {
		return fmt.Errorf(
			"etcd quorum cannot tolerate one unavailable control-plane node: voters=%d quorum=%d",
			len(nodes),
			quorum,
		)
	}
	for _, node := range nodes {
		if node.IP == "" ||
			node.Conditions.Joined.Status != db.ConditionStatusTrue ||
			node.Conditions.KubernetesReady.Status != db.ConditionStatusTrue ||
			node.Conditions.NodeAgentReady.Status != db.ConditionStatusTrue ||
			node.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return fmt.Errorf("control-plane node %s is not healthy for certificate renewal", node.Name)
		}
	}

	cluster, err := a.db.GetCluster(ctx, input.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster %s for certificate-renewal health check: %w", input.ClusterID, err)
	}
	tenantClient, err := a.tenantClientForCluster(ctx, cluster)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if err := requireKubernetesNodeReady(ctx, tenantClient, node.Name); err != nil {
			return err
		}
	}
	if input.PostflightNodeID != nil {
		nodeName := ""
		for _, node := range nodes {
			if node.ID == *input.PostflightNodeID {
				nodeName = node.Name
				break
			}
		}
		if nodeName == "" {
			return fmt.Errorf("postflight node %s is not in the control-plane snapshot", *input.PostflightNodeID)
		}
		if err := requireStaticMirrorPodsReady(ctx, tenantClient, nodeName); err != nil {
			return err
		}
	}

	if err := a.probeControlPlaneAPIHealth(ctx, nodes); err != nil {
		return err
	}
	return nil
}

func validateCertificateRenewalNodeSet(nodes []db.Node, expected []uuid.UUID) error {
	if len(nodes) == 0 {
		return errors.New("no control-plane nodes available for certificate renewal")
	}
	if len(nodes) != len(expected) {
		return fmt.Errorf("control-plane node set changed during certificate renewal: expected %d nodes, found %d", len(expected), len(nodes))
	}
	expectedSet := make(map[uuid.UUID]struct{}, len(expected))
	for _, id := range expected {
		expectedSet[id] = struct{}{}
	}
	for _, node := range nodes {
		if _, ok := expectedSet[node.ID]; !ok {
			return fmt.Errorf("control-plane node set changed during certificate renewal: unexpected node %s", node.ID)
		}
	}
	return nil
}

func requireKubernetesNodeReady(ctx context.Context, tenantClient client.Client, nodeName string) error {
	node := &corev1.Node{}
	if err := tenantClient.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return fmt.Errorf("failed to get Kubernetes control-plane node %s: %w", nodeName, err)
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return nil
		}
	}
	return fmt.Errorf("kubernetes control-plane node %s is not ready", nodeName)
}

// requireStaticMirrorPodsReady requires both Running and Ready: restoring a
// manifest or observing a replacement container is not enough to advance rollout.
func requireStaticMirrorPodsReady(ctx context.Context, tenantClient client.Client, nodeName string) error {
	for _, component := range []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler"} {
		podName := component + "-" + nodeName
		pod := &corev1.Pod{}
		if err := tenantClient.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: podName}, pod); err != nil {
			return fmt.Errorf("failed to get static mirror pod %s: %w", podName, err)
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if pod.Status.Phase != corev1.PodRunning || !ready {
			return fmt.Errorf("static mirror pod %s is not Running and Ready", podName)
		}
	}
	return nil
}

// CompleteCertificateRenewalInput carries the aggregate successful outcome after
// all node results and postflight gates have passed, not merely RPC acceptance.
type CompleteCertificateRenewalInput struct {
	ClusterID uuid.UUID
	CycleID   string
	Outcome   db.CertificateRenewalOutcome
}

// CompleteCertificateRenewalResult acknowledges finalization; the durable outcome
// and next due time belong to the cluster record rather than the activity result.
type CompleteCertificateRenewalResult struct{}

// CompleteCertificateRenewal atomically records success, advances the due time
// from completion and releases this cycle's mutation lock. An unchanged cycle
// also advances the schedule. Repeated completion of the same successful cycle
// leaves its original completion time and due time intact.
func (a *Activities) CompleteCertificateRenewal(
	ctx context.Context,
	input CompleteCertificateRenewalInput,
) (CompleteCertificateRenewalResult, error) {
	if input.Outcome != db.CertificateRenewalOutcomeUnchanged && input.Outcome != db.CertificateRenewalOutcomeRenewed {
		return CompleteCertificateRenewalResult{}, certificateRenewalPreconditionError("invalid certificate-renewal outcome")
	}
	if a.cfg.CertRenewalInterval <= 0 {
		return CompleteCertificateRenewalResult{}, errors.New("certificate-renewal interval must be positive")
	}

	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, input.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}
		status := cluster.Conditions.CertificateRenewal
		if status != nil && status.CycleID == input.CycleID && status.State == db.CertificateRenewalStateSucceeded {
			return nil
		}
		if status == nil || status.CycleID != input.CycleID || status.State != db.CertificateRenewalStateAccepted {
			return certificateRenewalPreconditionError("certificate-renewal status is not owned by this cycle")
		}
		if update := cluster.Conditions.UpdateInProgress; update == nil || update.Type != db.OperationTypeCertRenewal {
			return certificateRenewalPreconditionError("certificate-renewal cluster mutation lock is not held")
		}

		completedAt := time.Now().UTC()
		status.State = db.CertificateRenewalStateSucceeded
		status.Outcome = input.Outcome
		status.CompletedAt = &completedAt
		status.FailedNodeID = nil
		status.Phase = ""
		status.Message = ""
		cluster.NextCertRenewalDue = timePointer(completedAt.Add(a.cfg.CertRenewalInterval))
		db.ClearUpdateInProgress(&cluster)
		if _, err := tx.UpdateCluster(ctx, &cluster); err != nil {
			return fmt.Errorf("failed to mark certificate renewal succeeded: %w", err)
		}
		return nil
	})
	if err != nil {
		return CompleteCertificateRenewalResult{}, err
	}
	return CompleteCertificateRenewalResult{}, nil
}

// FailCertificateRenewalInput records a failure independently of whether it is
// safe to release ownership. RPC or workflow cancellation is not terminal proof.
type FailCertificateRenewalInput struct {
	// RetainMutationLock must be true unless no node work was dispatched or the
	// selected executor supplied matching terminal evidence.
	RetainMutationLock bool
	ClusterID          uuid.UUID
	CycleID            string
	FailedNodeID       *uuid.UUID
	Phase              string
	Message            string
}

// FailCertificateRenewalResult carries no ownership decision. Callers must read
// cluster conditions to distinguish released from retained locks; an already
// deleted cluster also permits cleanup to succeed without a record to inspect.
type FailCertificateRenewalResult struct{}

// FailCertificateRenewal leaves the due time unchanged and records diagnostics
// only for the owning accepted cycle. When termination is unconfirmed it retains
// the mutation lock, even though the cycle is failed. Redelivery cannot revise
// that ownership decision; releasing a retained lock needs operator recovery.
func (a *Activities) FailCertificateRenewal(
	ctx context.Context,
	input FailCertificateRenewalInput,
) (FailCertificateRenewalResult, error) {
	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, input.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}
		status := cluster.Conditions.CertificateRenewal
		if status != nil && status.CycleID == input.CycleID && status.State == db.CertificateRenewalStateFailed {
			return nil
		}
		if status == nil || status.CycleID != input.CycleID || status.State != db.CertificateRenewalStateAccepted {
			return certificateRenewalPreconditionError("certificate-renewal status is not owned by this cycle")
		}
		if update := cluster.Conditions.UpdateInProgress; update == nil || update.Type != db.OperationTypeCertRenewal {
			return certificateRenewalPreconditionError("certificate-renewal cluster mutation lock is not held")
		}

		completedAt := time.Now().UTC()
		status.State = db.CertificateRenewalStateFailed
		status.CompletedAt = &completedAt
		status.FailedNodeID = input.FailedNodeID
		status.Phase = input.Phase
		status.Message = truncateCertificateRenewalMessage(input.Message)
		if !input.RetainMutationLock {
			db.ClearUpdateInProgress(&cluster)
		}
		if _, err := tx.UpdateCluster(ctx, &cluster); err != nil {
			return fmt.Errorf("failed to mark certificate renewal failed: %w", err)
		}
		return nil
	})
	if err != nil {
		return FailCertificateRenewalResult{}, err
	}
	return FailCertificateRenewalResult{}, nil
}

func truncateCertificateRenewalMessage(message string) string {
	runes := []rune(strings.TrimSpace(strings.ToValidUTF8(message, "�")))
	if len(runes) <= certificateRenewalFailureMessageLimit {
		return string(runes)
	}
	return string(runes[:certificateRenewalFailureMessageLimit])
}

func timePointer(value time.Time) *time.Time {
	return &value
}
