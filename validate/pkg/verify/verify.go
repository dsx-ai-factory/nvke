// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package verify creates and checks image-validation verdicts: in-toto
// statements over an image digest, carried in DSSE envelopes signed with an
// operator-supplied key. It is the consumer's half of the validator — the
// management plane re-checks a verdict in milliseconds, never the image — so
// it depends on nothing else in this module and on no network: keys are
// plain PEM, there is no transparency log and no OIDC issuer involved.
package verify

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// PayloadType is the DSSE payload type for in-toto statements.
	PayloadType = "application/vnd.in-toto+json"
	// StatementType is the in-toto statement framing version.
	StatementType = "https://in-toto.io/Statement/v1"
	// PredicateType identifies an image-validation verdict predicate.
	PredicateType = "https://nvke.nvidia.com/image-validation/v1"
)

// Verdict outcomes. The outcome mirrors the validator's exit contract: a fail
// or error verdict is still evidence worth signing — a consumer enforcing
// "validated" requires OutcomePass, not merely a verifiable envelope.
const (
	OutcomePass  = "pass"
	OutcomeFail  = "fail"
	OutcomeError = "error"
)

// Envelope is a DSSE envelope (payload and signatures base64, standard
// encoding).
type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

// Signature is one DSSE signature; KeyID is advisory (the sha256 of the
// signer's PKIX public key here) and never trusted on its own.
type Signature struct {
	KeyID string `json:"keyid,omitempty"`
	Sig   string `json:"sig"`
}

// Statement is the in-toto statement a verdict envelope carries.
type Statement struct {
	Type          string    `json:"_type"`
	Subject       []Subject `json:"subject"`
	PredicateType string    `json:"predicateType"`
	Predicate     Predicate `json:"predicate"`
}

// Subject names one artifact and its digests (bare hex, keyed by algorithm).
type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// Predicate is the image-validation verdict: who validated what against which
// check set, with what outcome. The types mirror the report document but are
// defined here so a consumer vendors only this package.
type Predicate struct {
	Outcome             string        `json:"outcome"`
	CheckSetVersion     string        `json:"checkSetVersion"`
	ReportSchemaVersion string        `json:"reportSchemaVersion"`
	Validator           Tool          `json:"validator"`
	Target              Target        `json:"target"`
	Summary             Summary       `json:"summary"`
	Results             []CheckResult `json:"results"`
	FinishedAt          time.Time     `json:"finishedAt"`
}

// Tool identifies the validator build that produced the verdict.
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Target echoes what the run validated against.
type Target struct {
	Variant string `json:"variant"`
	Arch    string `json:"arch"`
	Ubuntu  string `json:"ubuntu"`
}

// Summary is the per-status tally of the run.
type Summary struct {
	Pass         int `json:"pass"`
	Fail         int `json:"fail"`
	BlockingFail int `json:"blockingFail"`
	WarnFail     int `json:"warnFail"`
	Skip         int `json:"skip"`
	Error        int `json:"error"`
}

// CheckResult is one check's outcome as the verdict records it.
type CheckResult struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Severity string `json:"severity"`
	Evidence string `json:"evidence,omitempty"`
}

// Policy is what a consumer requires of a verdict beyond a valid signature.
type Policy struct {
	// Keys are the accepted verification keys (ed25519.PublicKey or
	// *ecdsa.PublicKey); at least one must verify a signature.
	Keys []crypto.PublicKey
	// PredicateType overrides the expected predicate type; empty means
	// PredicateType.
	PredicateType string
	// MinCheckSetVersion is the lowest acceptable check-set version
	// (dotted-numeric compare); empty means no floor.
	MinCheckSetVersion string
	// MaxAge rejects verdicts whose FinishedAt is older than now-MaxAge;
	// zero means no age limit.
	MaxAge time.Duration
}

// Verify checks a verdict envelope against the policy and the image digest
// (bare-hex sha256) and returns the statement it carries. Every failure is an
// error: a verdict that cannot be verified must never read as a pass, and the
// returned statement's Outcome still needs checking — Verify answers "is this
// an authentic, applicable verdict", not "did validation pass".
func Verify(envelopeJSON []byte, imageSHA256 string, pol Policy, now time.Time) (*Statement, error) {
	if len(pol.Keys) == 0 {
		return nil, fmt.Errorf("verdict policy carries no verification keys")
	}
	var env Envelope
	if err := json.Unmarshal(envelopeJSON, &env); err != nil {
		return nil, fmt.Errorf("parsing DSSE envelope: %w", err)
	}
	if env.PayloadType != PayloadType {
		return nil, fmt.Errorf("envelope payload type is %q, want %q", env.PayloadType, PayloadType)
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return nil, fmt.Errorf("decoding envelope payload: %w", err)
	}
	if err := verifyAnySignature(pol.Keys, env, payload); err != nil {
		return nil, err
	}

	var st Statement
	if err := json.Unmarshal(payload, &st); err != nil {
		return nil, fmt.Errorf("parsing in-toto statement: %w", err)
	}
	if st.Type != StatementType {
		return nil, fmt.Errorf("statement type is %q, want %q", st.Type, StatementType)
	}
	wantPredicate := pol.PredicateType
	if wantPredicate == "" {
		wantPredicate = PredicateType
	}
	if st.PredicateType != wantPredicate {
		return nil, fmt.Errorf("predicate type is %q, want %q", st.PredicateType, wantPredicate)
	}
	digest := strings.ToLower(imageSHA256)
	if !subjectMatches(st.Subject, digest) {
		return nil, fmt.Errorf("no statement subject carries sha256 %s", digest)
	}
	switch st.Predicate.Outcome {
	case OutcomePass, OutcomeFail, OutcomeError:
	default:
		return nil, fmt.Errorf("verdict outcome is %q, want one of pass|fail|error", st.Predicate.Outcome)
	}
	if pol.MinCheckSetVersion != "" {
		older, err := versionLess(st.Predicate.CheckSetVersion, pol.MinCheckSetVersion)
		if err != nil {
			return nil, err
		}
		if older {
			return nil, fmt.Errorf("check-set version %s is below the required minimum %s", st.Predicate.CheckSetVersion, pol.MinCheckSetVersion)
		}
	}
	if pol.MaxAge > 0 {
		if st.Predicate.FinishedAt.IsZero() {
			return nil, fmt.Errorf("verdict carries no finishedAt but the policy sets a maximum age")
		}
		if age := now.Sub(st.Predicate.FinishedAt); age > pol.MaxAge {
			return nil, fmt.Errorf("verdict is %s old, policy maximum is %s", age.Round(time.Second), pol.MaxAge)
		}
	}
	return &st, nil
}

// verifyAnySignature accepts the envelope when any (signature, key) pair
// verifies over the DSSE pre-authentication encoding.
func verifyAnySignature(keys []crypto.PublicKey, env Envelope, payload []byte) error {
	if len(env.Signatures) == 0 {
		return fmt.Errorf("envelope carries no signatures")
	}
	message := pae(env.PayloadType, payload)
	for _, sig := range env.Signatures {
		raw, err := base64.StdEncoding.DecodeString(sig.Sig)
		if err != nil {
			continue
		}
		for _, key := range keys {
			if verifyOne(key, message, raw) {
				return nil
			}
		}
	}
	return fmt.Errorf("no envelope signature verifies with any accepted key")
}

func verifyOne(key crypto.PublicKey, message, sig []byte) bool {
	switch k := key.(type) {
	case ed25519.PublicKey:
		return ed25519.Verify(k, message, sig)
	case *ecdsa.PublicKey:
		digest := sha256.Sum256(message)
		return ecdsa.VerifyASN1(k, digest[:], sig)
	default:
		return false
	}
}

func subjectMatches(subjects []Subject, sha256Hex string) bool {
	for _, s := range subjects {
		if strings.ToLower(s.Digest["sha256"]) == sha256Hex && sha256Hex != "" {
			return true
		}
	}
	return false
}

// pae is the DSSE pre-authentication encoding: what is actually signed.
func pae(payloadType string, payload []byte) []byte {
	return []byte(fmt.Sprintf("DSSEv1 %d %s %d %s", len(payloadType), payloadType, len(payload), payload))
}

// versionLess reports a < b for dotted-numeric versions (missing segments are
// zero). Anything non-numeric is an error: a version that cannot be compared
// must not silently satisfy a floor.
func versionLess(a, b string) (bool, error) {
	as, err := versionSegments(a)
	if err != nil {
		return false, err
	}
	bs, err := versionSegments(b)
	if err != nil {
		return false, err
	}
	for len(as) < len(bs) {
		as = append(as, 0)
	}
	for len(bs) < len(as) {
		bs = append(bs, 0)
	}
	for i := range as {
		if as[i] != bs[i] {
			return as[i] < bs[i], nil
		}
	}
	return false, nil
}

func versionSegments(v string) ([]int, error) {
	parts := strings.Split(v, ".")
	segments := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("version %q is not dotted-numeric", v)
		}
		segments = append(segments, n)
	}
	return segments, nil
}
