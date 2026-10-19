// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/runbook"
)

const (
	// CertificateRenewalLegacyRequired selects the legacy executor before any typed work was accepted.
	CertificateRenewalLegacyRequired = "CertificateRenewalLegacyRequired"

	// CertificateRenewalNotDispatched proves the first attempt sent no Start RPC
	// and found no reservation for this node. Other failures still need resolution.
	CertificateRenewalNotDispatched = "CertificateRenewalNotDispatched"

	legacyKubernetesCertificateRenewalRunbook = "k8s-cert-renewal"
	certificateRenewalStatusPollInterval      = 5 * time.Second
	renewalOutcomeMarkerPrefix                = "RENEWAL_OUTCOME="
	// Legacy V1 workflows can overlap before reserving node work. V2 admission
	// instead serializes cycles under the cluster mutation lock.
	certificateRenewalReservationPollInterval = 5 * time.Second
	certificateRenewalReservationWait         = 20 * time.Minute
)

// errCertificateRenewalBusy reports a renewal reservation held by another node.
var errCertificateRenewalBusy = errors.New("certificate renewal is in progress on another node")

// KubernetesCertificateRenewalInput identifies one node-local renewal
// operation. OperationID and renewal parameters must remain stable across retries:
// the agent reuses records by ID, not by comparing request parameters. A positive
// ExpectedEtcdMembers opts into V2 and must come from the workflow's fixed snapshot.
type KubernetesCertificateRenewalInput struct {
	NodeID              uuid.UUID
	OperationID         string
	ThresholdDays       int
	Force               bool
	ExpectedEtcdMembers int // Zero is reserved for V1 workflow replay.
}

// KubernetesCertificateRenewalResult is the bounded controller-facing result
// for either the typed operation or the legacy mixed-fleet fallback. Successful
// V2 calls always populate Outcome; Output is retained for diagnostics only.
type KubernetesCertificateRenewalResult struct {
	Outcome      db.CertificateRenewalOutcome
	Output       string
	ErrorMessage string
	ExitCode     int
}

// KubernetesCertificateRenewalCheckResult selects whether a node needs renewal
// dispatch. Legacy reports pending legacy-runbook intent, which only the legacy
// executor can resume: the typed agent operation never reads its phase file.
// Typed, set only with Legacy, reports that the agent also holds pending intent,
// so the typed executor must run after the legacy one in the same cycle.
type KubernetesCertificateRenewalCheckResult struct {
	Required bool
	Legacy   bool
	Typed    bool
}

// CheckKubernetesCertificateRenewalRequired reads expiry and either executor's
// persisted renewing/restart_pending intent without reserving or starting
// renewal; pending intent requires dispatch to the executor that recorded it so
// its restarts finish. Use only the embedded check-only mode: configured
// overrides may not understand it and must never turn this read into a mutation.
func (a *Activities) CheckKubernetesCertificateRenewalRequired(
	ctx context.Context,
	input KubernetesCertificateRenewalInput,
) (KubernetesCertificateRenewalCheckResult, error) {
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return KubernetesCertificateRenewalCheckResult{}, fmt.Errorf("failed to get node for certificate expiry check: %w", err)
	}
	script, err := runbook.Script("", legacyKubernetesCertificateRenewalRunbook)
	if err != nil {
		return KubernetesCertificateRenewalCheckResult{}, err
	}
	script, err = runbook.InjectEnvironment(map[string]string{
		"CHECK_ONLY": "true", "THRESHOLD_DAYS": fmt.Sprint(input.ThresholdDays),
	}, script)
	if err != nil {
		return KubernetesCertificateRenewalCheckResult{}, fmt.Errorf("failed to inject certificate expiry check environment: %w", err)
	}
	response, err := a.nodeAgentClient.ExecuteScript(ctx, node, legacyKubernetesCertificateRenewalRunbook, script, nil)
	if err != nil {
		return KubernetesCertificateRenewalCheckResult{}, fmt.Errorf("failed to check certificate expiry: %w", err)
	}
	if response == nil || response.GetExitCode() != 0 {
		return KubernetesCertificateRenewalCheckResult{}, errors.New("certificate expiry check failed")
	}
	switch strings.TrimSpace(response.GetOutput()) {
	case "RENEWAL_REQUIRED=false":
		return KubernetesCertificateRenewalCheckResult{}, nil
	case "RENEWAL_REQUIRED=true":
		return KubernetesCertificateRenewalCheckResult{Required: true}, nil
	case "RENEWAL_REQUIRED=legacy":
		return KubernetesCertificateRenewalCheckResult{Required: true, Legacy: true}, nil
	case "RENEWAL_REQUIRED=legacy_then_typed":
		return KubernetesCertificateRenewalCheckResult{Required: true, Legacy: true, Typed: true}, nil
	default:
		return KubernetesCertificateRenewalCheckResult{}, errors.New("certificate expiry check has no recognized result")
	}
}

// RenewKubernetesCertificates renews certificates through the typed node-agent
// operation, polling it to a terminal state. Inputs without an expected etcd
// member count retain the V1 RPC for existing workflow histories; hardened
// inputs use the capability-distinct V2 RPC. V2 reports a definitive first-attempt
// Unimplemented to the workflow so it can durably select the legacy executor.
// Later V2 attempts and status errors never change execution mechanisms.
// Cancellation stops this activity's observation, not accepted node work; the
// workflow must resolve the selected operation before releasing ownership.
func (a *Activities) RenewKubernetesCertificates(
	ctx context.Context,
	input KubernetesCertificateRenewalInput,
) (KubernetesCertificateRenewalResult, error) {
	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return KubernetesCertificateRenewalResult{}, fmt.Errorf("failed to get node: %w", err)
	}
	if input.ThresholdDays <= 0 || input.ThresholdDays > math.MaxInt32 {
		return KubernetesCertificateRenewalResult{}, fmt.Errorf("threshold days must be between 1 and %d", math.MaxInt32)
	}
	if input.ExpectedEtcdMembers < 0 || input.ExpectedEtcdMembers > math.MaxInt32 {
		return KubernetesCertificateRenewalResult{}, fmt.Errorf("expected etcd members must be between 0 and %d", math.MaxInt32)
	}
	if input.Force && input.ExpectedEtcdMembers == 0 {
		return KubernetesCertificateRenewalResult{}, errors.New("forced renewal requires an expected etcd member count")
	}

	if err := a.awaitCertificateRenewalReservation(ctx, node, &input, certificateRenewalReservationPollInterval, certificateRenewalReservationWait); err != nil {
		return KubernetesCertificateRenewalResult{}, err
	}

	var operation *nvkeproto.KubernetesCertificateRenewalOperation
	if input.ExpectedEtcdMembers == 0 {
		var response *nvkeproto.StartKubernetesCertificateRenewalResponse
		response, err = a.nodeAgentClient.StartKubernetesCertificateRenewal(
			ctx,
			node,
			&nvkeproto.StartKubernetesCertificateRenewalRequest{
				OperationId:   input.OperationID,
				ThresholdDays: int32(input.ThresholdDays),
			},
		)
		if response != nil {
			operation = response.GetOperation()
		}
	} else {
		var response *nvkeproto.StartKubernetesCertificateRenewalV2Response
		response, err = a.nodeAgentClient.StartKubernetesCertificateRenewalV2(
			ctx,
			node,
			&nvkeproto.StartKubernetesCertificateRenewalV2Request{
				OperationId:         input.OperationID,
				ThresholdDays:       int32(input.ThresholdDays),
				Force:               input.Force,
				ExpectedEtcdMembers: int32(input.ExpectedEtcdMembers),
			},
		)
		if response != nil {
			operation = response.GetOperation()
		}
	}
	if err != nil {
		if status.Code(err) != codes.Unimplemented {
			// A failed Start may still have been accepted. Retries reattach to the
			// reservation; it is released only once termination is confirmed.
			return KubernetesCertificateRenewalResult{}, fmt.Errorf("failed to start certificate renewal on node %s: %w", node.Name, err)
		}
		if input.ExpectedEtcdMembers > 0 {
			// A prior attempt may have accepted work but lost its response. Even
			// Unimplemented cannot safely select a different executor on retry.
			if activity.GetInfo(ctx).Attempt != 1 {
				return KubernetesCertificateRenewalResult{}, fmt.Errorf("cannot select legacy renewal after an ambiguous typed attempt: %w", err)
			}
			if releaseErr := a.releaseCertificateRenewal(ctx, node, input.OperationID); releaseErr != nil {
				return KubernetesCertificateRenewalResult{}, releaseErr
			}
			return KubernetesCertificateRenewalResult{}, temporal.NewNonRetryableApplicationError(
				"node agent does not implement certificate renewal V2", CertificateRenewalLegacyRequired, err,
			)
		}
		legacy, legacyErr := a.RenewLegacyKubernetesCertificates(ctx, input)
		if legacyErr != nil {
			var runbookErr *temporal.ApplicationError
			if errors.As(legacyErr, &runbookErr) && runbookErr.Type() == "RunbookError" {
				// The script exited, so its termination is confirmed: release, as #1692 does.
				if releaseErr := a.releaseCertificateRenewal(ctx, node, input.OperationID); releaseErr != nil {
					return legacy, errors.Join(legacyErr, releaseErr)
				}
			}
			return legacy, legacyErr
		}
		if releaseErr := a.releaseCertificateRenewal(ctx, node, input.OperationID); releaseErr != nil {
			return KubernetesCertificateRenewalResult{}, releaseErr
		}
		return legacy, nil
	}
	if operation == nil {
		return KubernetesCertificateRenewalResult{}, errors.New("node agent returned an empty certificate renewal operation")
	}

	waitBeforePoll := false
	for {
		result, done, err := certificateRenewalTerminalResult(operation)
		if err != nil || done {
			phase := operation.GetOperation().GetPhase()
			if phase == nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED || phase == nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED {
				if releaseErr := a.releaseCertificateRenewal(ctx, node, input.OperationID); releaseErr != nil {
					return result, releaseErr
				}
			}
			return result, err
		}
		if waitBeforePoll {
			timer := time.NewTimer(certificateRenewalStatusPollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return KubernetesCertificateRenewalResult{}, ctx.Err()
			case <-timer.C:
			}
		}
		statusResponse, statusErr := a.nodeAgentClient.GetKubernetesCertificateRenewalStatus(
			ctx,
			node,
			&nvkeproto.GetKubernetesCertificateRenewalStatusRequest{OperationId: input.OperationID},
		)
		if statusErr != nil {
			return KubernetesCertificateRenewalResult{}, fmt.Errorf(
				"failed to get accepted certificate renewal operation %s from node %s: %w",
				input.OperationID,
				node.Name,
				statusErr,
			)
		}
		if statusResponse == nil || statusResponse.GetOperation() == nil {
			return KubernetesCertificateRenewalResult{}, errors.New("node agent returned an empty certificate renewal status")
		}
		operation = statusResponse.GetOperation()
		waitBeforePoll = true
	}
}

// RenewLegacyKubernetesCertificates stays on the legacy executor selected by a
// V2 workflow (or reached by V1's inline fallback). V2 success requires both an
// outcome marker and a durable receipt verified under the runbook lock, because
// output alone cannot prevent delayed attempts from mutating a later cycle.
// Receipt-unaware overrides cannot satisfy that contract. V1 retains its older
// output-only result without receipt validation.
func (a *Activities) RenewLegacyKubernetesCertificates(ctx context.Context, input KubernetesCertificateRenewalInput) (KubernetesCertificateRenewalResult, error) {
	runbookEnv := map[string]string{
		"FORCE":          fmt.Sprintf("%t", input.Force),
		"OPERATION_ID":   input.OperationID,
		"THRESHOLD_DAYS": fmt.Sprintf("%d", input.ThresholdDays),
	}
	if input.ExpectedEtcdMembers > 0 {
		runbookEnv["EXPECTED_ETCD_MEMBERS"] = fmt.Sprintf("%d", input.ExpectedEtcdMembers)
	}
	legacy, legacyErr := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
		HostID:      input.NodeID,
		RunbookName: legacyKubernetesCertificateRenewalRunbook,
		RunbookEnv:  runbookEnv,
	})
	if legacyErr != nil {
		return KubernetesCertificateRenewalResult{}, fmt.Errorf("failed to execute legacy certificate renewal on node %s: %w", input.NodeID, legacyErr)
	}
	result := KubernetesCertificateRenewalResult{
		Output:       legacy.Output,
		ErrorMessage: legacy.ErrorMessage,
		ExitCode:     legacy.ExitCode,
	}
	if input.ExpectedEtcdMembers > 0 {
		var err error
		result.Outcome, err = certificateRenewalOutcome(result.Output)
		if err != nil {
			return result, temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("invalid legacy certificate renewal result on node %s", input.NodeID),
				"InvalidCertificateRenewalOutcome", err, result,
			)
		}
		// Successful output alone cannot fence delayed legacy attempts. Require
		// the same receipt-backed evidence as failure cleanup before unlocking.
		resolutionCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		// An abandoned receipt fences delayed attempts but is not a completed renewal.
		if err := a.resolveKubernetesCertificateRenewal(resolutionCtx, ResolveKubernetesCertificateRenewalInput{
			NodeID: input.NodeID, OperationID: input.OperationID, Legacy: true,
		}, false, false); err != nil {
			return result, temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("legacy certificate renewal on node %s has no confirmed terminal receipt", input.NodeID),
				"UnconfirmedLegacyCertificateRenewal", err, result,
			)
		}
	}
	return result, nil
}

// ResolveKubernetesCertificateRenewalInput identifies the exact operation and
// executor dispatched by the workflow. Resolution never starts or resumes work.
type ResolveKubernetesCertificateRenewalInput struct {
	NodeID      uuid.UUID
	OperationID string
	Legacy      bool
}

// ResolveKubernetesCertificateRenewal returns nil only after matching terminal
// evidence from the selected executor, without starting or resuming renewal.
// Terminal failure, including failed restoration, can resolve ownership; nil does
// not mean renewal succeeded or the node is healthy. Its caller must impose a
// bounded timeout independent of workflow cancellation and retain ownership on
// error, including missing or unsupported status. Terminal evidence also
// releases the operation's matching node reservation.
func (a *Activities) ResolveKubernetesCertificateRenewal(ctx context.Context, input ResolveKubernetesCertificateRenewalInput) error {
	return a.resolveKubernetesCertificateRenewal(ctx, input, true, true)
}

func (a *Activities) resolveKubernetesCertificateRenewal(ctx context.Context, input ResolveKubernetesCertificateRenewalInput, acceptAbandoned, release bool) error {
	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return fmt.Errorf("failed to get node for certificate-renewal resolution: %w", err)
	}
	if err := a.awaitCertificateRenewalTermination(ctx, node, input, acceptAbandoned); err != nil {
		return err
	}
	if !release {
		return nil
	}
	return a.releaseCertificateRenewal(ctx, node, input.OperationID)
}

// awaitCertificateRenewalTermination returns nil only for matching terminal evidence.
func (a *Activities) awaitCertificateRenewalTermination(ctx context.Context, node db.Node, input ResolveKubernetesCertificateRenewalInput, acceptAbandoned bool) error {
	if input.Legacy {
		return a.resolveLegacyKubernetesCertificateRenewal(ctx, node, input.OperationID, acceptAbandoned)
	}
	for {
		response, err := a.nodeAgentClient.GetKubernetesCertificateRenewalStatus(ctx, node,
			&nvkeproto.GetKubernetesCertificateRenewalStatusRequest{OperationId: input.OperationID})
		if err != nil {
			err = fmt.Errorf("failed to resolve certificate renewal operation %s: %w", input.OperationID, err)
			if ctx.Err() != nil || !transientCertificateRenewalStatusError(err) {
				return err
			}
			// An agent restart or network blip must not retain the lock while the
			// caller's bounded budget remains; keep polling for terminal status.
			activity.GetLogger(ctx).Debug("certificate renewal status temporarily unavailable",
				"nodeID", node.ID, "operationID", input.OperationID, "error", err)
			if waitErr := waitForCertificateRenewalStatusPoll(ctx); waitErr != nil {
				return errors.Join(waitErr, err)
			}
			continue
		}
		operation := response.GetOperation()
		statusOperation := operation.GetOperation()
		if statusOperation == nil || statusOperation.GetOperationId() != input.OperationID {
			return errors.New("certificate renewal status is missing or does not match the operation ID")
		}
		switch statusOperation.GetPhase() {
		case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED:
			_, _, err := certificateRenewalTerminalResult(operation)
			return err
		case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED:
			switch operation.GetOutcome() {
			case nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_BEFORE_RESTART,
				nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED,
				nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_NOT_RESTORED:
				return nil
			case nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNSPECIFIED,
				nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED,
				nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED:
				return errors.New("failed certificate renewal status has an invalid outcome")
			default:
				return errors.New("failed certificate renewal status has an unknown outcome")
			}
		case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING:
			// Only status is polled. NotFound/Unimplemented never select fallback.
		case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_UNSPECIFIED:
			return errors.New("certificate renewal status has an unspecified phase")
		default:
			return errors.New("certificate renewal status has an unknown phase")
		}
		if err := waitForCertificateRenewalStatusPoll(ctx); err != nil {
			return err
		}
	}
}

// transientCertificateRenewalStatusError reports local connection failures,
// which carry no gRPC status, and Unavailable or DeadlineExceeded responses.
// NotFound, Unimplemented and other agent answers remain conclusive.
func transientCertificateRenewalStatusError(err error) bool {
	rpcStatus, ok := status.FromError(err)
	if !ok {
		return true
	}
	return rpcStatus.Code() == codes.Unavailable || rpcStatus.Code() == codes.DeadlineExceeded
}

func waitForCertificateRenewalStatusPoll(ctx context.Context) error {
	timer := time.NewTimer(certificateRenewalStatusPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// resolveLegacyKubernetesCertificateRenewal requires the embedded script's exact
// terminal marker. That script synchronizes and validates an operation-keyed
// receipt while holding the renewal flock, excluding active EXIT restoration.
// Old phase files are resumable but cannot supply receipt-aware provenance, so
// neither an old complete phase nor ordinary success output releases ownership.
// The script records an abandoned receipt for a receipt-aware operation that
// ended without one; acceptAbandoned admits it only as terminal failure.
func (a *Activities) resolveLegacyKubernetesCertificateRenewal(ctx context.Context, node db.Node, operationID string, acceptAbandoned bool) error {
	// Always use the embedded status-only implementation: an older configured
	// runbook override might ignore STATUS_ONLY and start renewal instead.
	script, err := runbook.Script("", legacyKubernetesCertificateRenewalRunbook)
	if err != nil {
		return err
	}
	script, err = runbook.InjectEnvironment(map[string]string{
		"STATUS_ONLY": "true", "OPERATION_ID": operationID, "LOCK_TIMEOUT": "240",
	}, script)
	if err != nil {
		return fmt.Errorf("failed to inject certificate-renewal status environment: %w", err)
	}
	response, err := a.nodeAgentClient.ExecuteScript(ctx, node, legacyKubernetesCertificateRenewalRunbook, script, nil)
	if err != nil {
		return fmt.Errorf("failed to resolve legacy certificate renewal: %w", err)
	}
	if response == nil || response.GetExitCode() != 0 {
		return errors.New("legacy certificate renewal has no matching terminal evidence")
	}
	terminal := "RENEWAL_TERMINAL_OPERATION_ID=" + operationID
	switch strings.TrimSpace(response.GetOutput()) {
	case terminal:
		return nil
	case terminal + "\nRENEWAL_TERMINAL_OUTCOME=abandoned":
		if acceptAbandoned {
			return nil
		}
		return errors.New("legacy certificate renewal was abandoned without a completed receipt")
	default:
		return errors.New("legacy certificate renewal has no matching terminal evidence")
	}
}

// certificateRenewalOutcome accepts plain or timestamped runbook markers. The
// last recognized marker is the requested operation's result after any pending
// operation was resumed; unknown markers fail rather than become a no-op.
func certificateRenewalOutcome(output string) (db.CertificateRenewalOutcome, error) {
	var outcome db.CertificateRenewalOutcome
	for line := range strings.SplitSeq(output, "\n") {
		marker := strings.TrimSpace(line)
		if timestampEnd := strings.Index(marker, "] "); strings.HasPrefix(marker, "[") && timestampEnd >= 0 {
			marker = marker[timestampEnd+2:]
		}
		value, ok := strings.CutPrefix(marker, renewalOutcomeMarkerPrefix)
		if !ok {
			continue
		}
		switch candidate := db.CertificateRenewalOutcome(value); candidate {
		case db.CertificateRenewalOutcomeUnchanged, db.CertificateRenewalOutcomeRenewed:
			outcome = candidate
		default:
			return "", fmt.Errorf("unknown outcome %q", value)
		}
	}
	if outcome == "" {
		return "", errors.New("renewal outcome marker is missing")
	}
	return outcome, nil
}

// certificateRenewalTerminalResult distinguishes ongoing work from a usable
// result. A succeeded phase still needs a recognized successful outcome; failed
// phases preserve restoration diagnostics rather than masquerading as a no-op.
func certificateRenewalTerminalResult(
	operation *nvkeproto.KubernetesCertificateRenewalOperation,
) (KubernetesCertificateRenewalResult, bool, error) {
	statusOperation := operation.GetOperation()
	if statusOperation == nil {
		return KubernetesCertificateRenewalResult{}, true, errors.New("certificate renewal operation status is empty")
	}
	switch statusOperation.GetPhase() {
	case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING:
		return KubernetesCertificateRenewalResult{}, false, nil
	case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED:
		switch operation.GetOutcome() {
		case nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED,
			nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED:
			outcome := db.CertificateRenewalOutcomeUnchanged
			if operation.GetOutcome() == nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED {
				outcome = db.CertificateRenewalOutcomeRenewed
			}
			return KubernetesCertificateRenewalResult{
				Outcome: outcome,
				Output:  fmt.Sprintf("certificate renewal %s", operation.GetOutcome()),
			}, true, nil
		case nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_UNSPECIFIED,
			nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_BEFORE_RESTART,
			nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED,
			nvkeproto.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_NOT_RESTORED:
			return KubernetesCertificateRenewalResult{}, true, fmt.Errorf(
				"certificate renewal operation %s succeeded with invalid outcome %s",
				statusOperation.GetOperationId(),
				operation.GetOutcome(),
			)
		default:
			return KubernetesCertificateRenewalResult{}, true, fmt.Errorf(
				"certificate renewal operation %s succeeded with unknown outcome %d",
				statusOperation.GetOperationId(),
				operation.GetOutcome(),
			)
		}
	case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED:
		return KubernetesCertificateRenewalResult{}, true, fmt.Errorf(
			"certificate renewal operation %s failed at %s with outcome %s: %s",
			statusOperation.GetOperationId(),
			statusOperation.GetStep(),
			operation.GetOutcome(),
			statusOperation.GetErrorMessage(),
		)
	case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_UNSPECIFIED:
		return KubernetesCertificateRenewalResult{}, true, fmt.Errorf(
			"certificate renewal operation %s returned invalid phase %s",
			statusOperation.GetOperationId(),
			statusOperation.GetPhase(),
		)
	default:
		return KubernetesCertificateRenewalResult{}, true, fmt.Errorf(
			"certificate renewal operation %s returned unknown phase %d",
			statusOperation.GetOperationId(),
			statusOperation.GetPhase(),
		)
	}
}

// awaitCertificateRenewalReservation retries the reservation while another
// node holds it, for at most wait, so two V1 RenewK8sCerts workflows on one
// cluster serialize instead of the later one failing. V2 reserve rejections do
// not return the busy sentinel and retain their proven non-dispatch semantics.
func (a *Activities) awaitCertificateRenewalReservation(ctx context.Context, node db.Node, input *KubernetesCertificateRenewalInput, poll, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := a.reserveCertificateRenewal(ctx, node, input)
		if !errors.Is(err, errCertificateRenewalBusy) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waited %s: %w", wait, err)
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// reserveCertificateRenewal admits actual agent work. Merely scheduling the
// Temporal workflow does not block upgrades. Both callers use the cluster row
// so an agent restart cannot begin between this check and the renewal RPC.
func (a *Activities) reserveCertificateRenewal(ctx context.Context, node db.Node, input *KubernetesCertificateRenewalInput) error {
	if !node.IsManagedByControlPlaneController() {
		return nil
	}
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, node.ClusterID)
		if err != nil {
			return err
		}
		locked, err := tx.GetNodeForUpdate(ctx, node.ID)
		if err != nil {
			return err
		}
		active := cluster.CertificateRenewal
		// On retry, even a failed admission may follow a lost Start response.
		// An existing reservation on this node likewise requires status resolution.
		reject := func(message string) error {
			if activity.GetInfo(ctx).Attempt == 1 && (active == nil || active.NodeID != node.ID) {
				return temporal.NewNonRetryableApplicationError(message, CertificateRenewalNotDispatched, nil)
			}
			return errors.New(message)
		}
		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue || nodeAgentRemovalRequested(locked) {
			return reject("node or cluster removal is requested")
		}
		if cluster.NodeAgentUpgrade != nil {
			return reject("node-agent installation is in progress")
		}
		if active != nil {
			if active.NodeID != node.ID {
				if input.ExpectedEtcdMembers == 0 {
					return fmt.Errorf("%w (node %s)", errCertificateRenewalBusy, active.NodeID)
				}
				return reject("certificate renewal is in progress on another node")
			}
			// A later activity attempt reattaches even if its workflow run changed.
			input.OperationID = active.OperationID
			return nil
		}
		cluster.CertificateRenewal = &db.NodeAgentCertificateRenewal{NodeID: node.ID, OperationID: input.OperationID, Fenced: input.ExpectedEtcdMembers > 0}
		_, err = tx.UpdateCluster(ctx, &cluster)
		return err
	})
}

func (a *Activities) releaseCertificateRenewal(ctx context.Context, node db.Node, operationID string) error {
	if !node.IsManagedByControlPlaneController() {
		return nil
	}
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, node.ClusterID)
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if active := cluster.CertificateRenewal; active != nil && active.NodeID == node.ID && active.OperationID == operationID {
			cluster.CertificateRenewal = nil
			_, err = tx.UpdateCluster(ctx, &cluster)
		}
		return err
	})
}
