// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package credentialaudit defines allowlisted credential lifecycle evidence.
package credentialaudit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// SchemaVersion identifies the event contract, independent of controller releases.
	SchemaVersion = "nke.credential-audit.v1"
	ActionCheck   = "check"
	ActionApprove = "approve"
	ActionRenew   = "renew"
	ActionRotate  = "rotate"
	ActionVerify  = "verify"

	TypeKubeadm = "kubeadm_leaf_bundle"
	TypeServing = "kubelet_serving"
	TypeEtcdKey = "etcd_encryption_key"

	PlaneT1 = "t1"
	PlaneT2 = "t2"

	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeNoop    = "no_op"

	ReasonAccepted     = "accepted"
	ReasonCompleted    = "completed"
	ReasonUnchanged    = "unchanged"
	ReasonFailed       = "operation_failed"
	ReasonRejected     = "identity_not_approved"
	ReasonApprovalFail = "approval_update_failed"
	ReasonHealthFailed = "health_check_failed"
	ReasonVerified     = "verified"
	ReasonObserved     = "observed"
	ReasonUnavailable  = "observation_unavailable"

	ActorAutomatic            = "automatic"
	ActorOperatorUnattributed = "operator_unattributed"
	ActorOperatorReported     = "operator_reported"
)

// Target contains managed UUIDs or hashes of externally managed T1 identities.
// Untrusted request usernames, CSR names, DNS names and SANs are never copied.
type Target struct {
	ClusterID   string `json:"cluster_id,omitempty"`
	ClusterHash string `json:"cluster_hash,omitempty"`
	NodeID      string `json:"node_id,omitempty"`
	NodeHash    string `json:"node_hash,omitempty"`
}

// Actor records attribution supported by the invocation path. SubjectHash is
// operator self-report, not an assertion of authentication or authorization.
type Actor struct {
	Kind        string `json:"kind"`
	SubjectHash string `json:"subject_hash,omitempty"`
}

// Correlation permits joins against known workflow/request identifiers without
// persisting potentially user-controlled identifiers in the audit ledger.
type Correlation struct {
	OperationHash string `json:"operation_hash"`
	WorkflowHash  string `json:"workflow_hash,omitempty"`
	RunHash       string `json:"run_hash,omitempty"`
	ActivityHash  string `json:"activity_hash,omitempty"`
	Attempt       int32  `json:"attempt,omitempty"`
}

// Event is an immutable observation. It intentionally has no free-form message,
// output, credential contents, secret identifiers or arbitrary metadata map.
type Event struct {
	SchemaVersion string      `json:"schema_version"`
	ID            string      `json:"event_id"`
	Timestamp     time.Time   `json:"timestamp"`
	Action        string      `json:"action"`
	Credential    string      `json:"credential_type"`
	Plane         string      `json:"plane"`
	Target        Target      `json:"target"`
	Actor         Actor       `json:"actor"`
	Outcome       string      `json:"outcome"`
	Reason        string      `json:"reason_code"`
	Phase         string      `json:"phase,omitempty"`
	Correlation   Correlation `json:"correlation"`
	Fingerprints  []string    `json:"certificate_sha256,omitempty"`
}

// Hash returns a domain-separated SHA-256 digest of an opaque identifier. This
// protects against accidental credential-like identifiers, not dictionary attacks.
func Hash(domain, value string) string {
	sum := sha256.Sum256([]byte(domain + "\x00" + value))
	return hex.EncodeToString(sum[:])
}

// New validates an observation and derives its semantic ID. Timestamp is excluded
// so a retried write of the same observation preserves the first ledger record.
// Distinct outcomes and activity attempts are separate observations.
func New(event Event) (Event, error) {
	event.SchemaVersion = SchemaVersion
	event.ID = ""
	event.Timestamp = event.Timestamp.UTC()
	event.Fingerprints = slices.Clone(event.Fingerprints)
	for index, fingerprint := range event.Fingerprints {
		event.Fingerprints[index] = strings.ToLower(fingerprint)
	}
	slices.Sort(event.Fingerprints)
	event.Fingerprints = slices.Compact(event.Fingerprints)
	identity := event
	identity.Timestamp = time.Time{}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return Event{}, fmt.Errorf("encode audit identity: %w", err)
	}
	event.ID = Hash("event", string(encoded))
	if err := event.Validate(); err != nil {
		return Event{}, err
	}
	return event, nil
}

// Validate rejects values outside the versioned allowlist before any sink write.
func (e Event) Validate() error {
	if e.SchemaVersion != SchemaVersion || !isDigest(e.ID) || e.Timestamp.IsZero() {
		return errors.New("invalid audit schema, event ID or timestamp")
	}
	if !slices.Contains([]string{ActionCheck, ActionApprove, ActionRenew, ActionRotate, ActionVerify}, e.Action) ||
		!slices.Contains([]string{TypeKubeadm, TypeServing, TypeEtcdKey}, e.Credential) ||
		!slices.Contains([]string{PlaneT1, PlaneT2}, e.Plane) ||
		!slices.Contains([]string{OutcomeSuccess, OutcomeFailure, OutcomeNoop}, e.Outcome) {
		return errors.New("invalid audit action, credential, plane or outcome")
	}
	if !slices.Contains([]string{
		ReasonAccepted, ReasonCompleted, ReasonUnchanged, ReasonFailed, ReasonRejected,
		ReasonApprovalFail, ReasonHealthFailed, ReasonVerified, ReasonObserved, ReasonUnavailable,
	}, e.Reason) {
		return errors.New("invalid audit reason code")
	}
	if e.Phase != "" && !slices.Contains([]string{
		"preflight", "renew-node", "postflight", "prepare", "add_secondary",
		"promote_primary", "rewrite", "remove_old_keys", "finalize",
	}, e.Phase) {
		return errors.New("invalid audit lifecycle phase")
	}
	if err := e.Target.validate(); err != nil {
		return err
	}
	if err := e.Actor.validate(); err != nil {
		return err
	}
	if err := e.Correlation.validate(); err != nil {
		return err
	}
	if len(e.Fingerprints) > 32 || (e.Credential == TypeEtcdKey && len(e.Fingerprints) > 0) {
		return errors.New("invalid audit certificate fingerprint count or credential type")
	}
	for _, fingerprint := range e.Fingerprints {
		if !isDigest(fingerprint) {
			return errors.New("invalid audit public certificate fingerprint")
		}
	}
	return nil
}

func (t Target) validate() error {
	if (t.ClusterID == "") == (t.ClusterHash == "") || (t.NodeID != "" && t.NodeHash != "") {
		return errors.New("audit target requires one cluster identity and at most one node identity")
	}
	for _, value := range []string{t.ClusterID, t.NodeID} {
		if value != "" {
			id, err := uuid.Parse(value)
			if err != nil || id == uuid.Nil || id.String() != value {
				return errors.New("invalid audit managed target UUID")
			}
		}
	}
	for _, value := range []string{t.ClusterHash, t.NodeHash} {
		if value != "" && !isDigest(value) {
			return errors.New("invalid audit external target hash")
		}
	}
	return nil
}

func (a Actor) validate() error {
	if !slices.Contains([]string{ActorAutomatic, ActorOperatorUnattributed, ActorOperatorReported}, a.Kind) {
		return errors.New("invalid audit actor kind")
	}
	if a.Kind == ActorOperatorReported {
		if !isDigest(a.SubjectHash) {
			return errors.New("operator-reported audit requires a subject hash")
		}
	} else if a.SubjectHash != "" {
		return errors.New("audit actor attribution unavailable for this invocation")
	}
	return nil
}

func (c Correlation) validate() error {
	if !isDigest(c.OperationHash) || c.Attempt < 0 {
		return errors.New("invalid audit operation correlation")
	}
	for _, value := range []string{c.WorkflowHash, c.RunHash, c.ActivityHash} {
		if value != "" && !isDigest(value) {
			return errors.New("invalid audit execution correlation")
		}
	}
	return nil
}

func isDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
