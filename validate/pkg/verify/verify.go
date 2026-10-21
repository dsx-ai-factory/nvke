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

// Evidence modes a verdict records: how the release evidence reached the run.
const (
	// EvidenceModeBundle: the evidence bundle was verified offline against a
	// pinned trusted root before any check ran.
	EvidenceModeBundle = "bundle"
	// EvidenceModeDigestOnly: a bare checksums file the caller trusted; the
	// evidence itself was never authenticated.
	EvidenceModeDigestOnly = "digest-only"
)

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
	// EvidenceMode says how the release evidence was supplied (bundle |
	// digest-only). Older verdicts predate the field and carry "".
	EvidenceMode string `json:"evidenceMode,omitempty"`
	// CheckSetSHA256 is the sha256 of the checks.yaml that judged the run —
	// the version string names the policy, this pins its content.
	CheckSetSHA256 string `json:"checkSetSha256,omitempty"`
	// BOMSHA256 is the sha256 of the target BOM file; empty when none was
	// supplied.
	BOMSHA256 string `json:"bomSha256,omitempty"`
	// TrustedRootSHA256 is the sha256 of the operator-pinned trusted root the
	// run verified the release evidence against; empty on digest-only runs
	// (no evidence was verified) and on verdicts that predate the field.
	TrustedRootSHA256 string `json:"trustedRootSha256,omitempty"`
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

// CheckResult is one check's outcome as the verdict records it. Reason is
// set on skips and distinguishes genuine non-applicability from omitted
// validation (tier-off, input-missing).
type CheckResult struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Severity string `json:"severity"`
	Reason   string `json:"reason,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

// SkipNotApplicable is the one skip reason that satisfies a required check;
// every other skip is omitted validation.
const SkipNotApplicable = "not-applicable"

// MaxClockSkew is how far in the future a verdict's finishedAt may sit before
// verification rejects it outright: enough for ordinary clock drift between
// signer and verifier, far too little to matter against MaxAge.
const MaxClockSkew = 5 * time.Minute

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
	// zero means no age limit. FinishedAt is asserted by the signer, so this
	// bounds staleness only under an honest signer clock: it is freshness
	// hygiene, not revocation. A compromised signing key is withdrawn by
	// removing it from Keys, never by trusting the ages its verdicts claim.
	MaxAge time.Duration
	// RequiredChecks is the qualification profile: each listed ID must have
	// passed or be skipped not-applicable. A missing result, disabled tier or
	// missing input is omitted validation and fails verification.
	RequiredChecks []string
	// CheckSetSHA256 pins approved registry contents (bare hex, any match).
	// Without it, applicability belongs to the producer's registry, and every
	// required check can be satisfied by a not-applicable skip.
	CheckSetSHA256 []string
	// ExpectedTarget: non-empty fields must equal the verdict's target.
	ExpectedTarget Target
	// TrustedRootSHA256 pins the trusted root(s) the run may have verified
	// evidence against (bare hex, any match). A verdict that records no
	// trusted root — a digest-only run, or one predating the field — fails
	// the pin: the consumer asked for authenticated evidence provenance.
	TrustedRootSHA256 []string
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
	if !st.Predicate.FinishedAt.IsZero() && st.Predicate.FinishedAt.After(now.Add(MaxClockSkew)) {
		// Rejected regardless of MaxAge: a forward-dated verdict would carry
		// a negative age past any maximum, forever. The signer controls
		// FinishedAt, so this closes only the clumsy forgery — see MaxAge's
		// honest-clock caveat for what age checks can and cannot promise.
		return nil, fmt.Errorf("verdict finishedAt %s is in the future (now %s, allowed skew %s)",
			st.Predicate.FinishedAt.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339), MaxClockSkew)
	}
	if pol.MaxAge > 0 {
		if st.Predicate.FinishedAt.IsZero() {
			return nil, fmt.Errorf("verdict carries no finishedAt but the policy sets a maximum age")
		}
		if age := now.Sub(st.Predicate.FinishedAt); age > pol.MaxAge {
			return nil, fmt.Errorf("verdict is %s old, policy maximum is %s", age.Round(time.Second), pol.MaxAge)
		}
	}
	if err := approvedDigest("check set", st.Predicate.CheckSetSHA256, pol.CheckSetSHA256); err != nil {
		return nil, err
	}
	if err := approvedDigest("trusted root", st.Predicate.TrustedRootSHA256, pol.TrustedRootSHA256); err != nil {
		return nil, err
	}
	if err := matchTarget(st.Predicate.Target, pol.ExpectedTarget); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(st.Predicate.Results))
	for _, r := range st.Predicate.Results {
		// Structural, not profile-dependent: a well-formed verdict never
		// repeats an ID, and last-write-wins would let fail-then-pass read
		// as pass.
		if seen[r.ID] {
			return nil, fmt.Errorf("the verdict carries duplicate results for check %s", r.ID)
		}
		seen[r.ID] = true
	}
	if err := requireChecks(st.Predicate.Results, pol.RequiredChecks); err != nil {
		return nil, err
	}
	return &st, nil
}

// approvedDigest enforces a content pin: with no pins it is inert, and with
// pins the verdict's recorded digest must match one of them. A verdict that
// records nothing fails a given pin — absence never satisfies a content
// requirement.
func approvedDigest(what, got string, pins []string) error {
	if len(pins) == 0 {
		return nil
	}
	got = strings.ToLower(got)
	approved := false
	for _, want := range pins {
		want = strings.ToLower(want)
		if !isSHA256Hex(want) {
			// A pin that can never match is a policy defect, not a
			// silently inert entry.
			return fmt.Errorf("policy pin %q is not a bare-hex sha256", want)
		}
		if got == want {
			approved = true
		}
	}
	if !approved {
		return fmt.Errorf("the verdict's %s (sha256 %q) is not an approved one", what, got)
	}
	return nil
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// matchTarget enforces every non-empty expectation exactly.
func matchTarget(got, want Target) error {
	for _, f := range []struct{ name, got, want string }{
		{"variant", got.Variant, want.Variant},
		{"arch", got.Arch, want.Arch},
		{"ubuntu", got.Ubuntu, want.Ubuntu},
	} {
		if f.want != "" && f.got != f.want {
			return fmt.Errorf("verdict target %s is %q, policy expects %q", f.name, f.got, f.want)
		}
	}
	return nil
}

// requireChecks enforces the qualification profile. The list is the
// consumer's, independent of the verdict's contents, so a signer cannot
// shrink it by omitting results.
func requireChecks(results []CheckResult, required []string) error {
	if len(required) == 0 {
		return nil
	}
	byID := make(map[string]CheckResult, len(results))
	for _, r := range results {
		byID[r.ID] = r // duplicates already rejected structurally in Verify
	}
	for _, id := range required {
		r, ok := byID[id]
		if !ok {
			return fmt.Errorf("required check %s: the verdict carries no result for it", id)
		}
		switch {
		case r.Status == "pass":
		case r.Status == "skip" && r.Reason == SkipNotApplicable:
		case r.Status == "skip":
			reason := r.Reason
			if reason == "" {
				reason = "no reason recorded"
			}
			return fmt.Errorf("required check %s was skipped (%s): omitted validation does not satisfy the profile", id, reason)
		default:
			return fmt.Errorf("required check %s: status %s (%s)", id, r.Status, r.Evidence)
		}
	}
	return nil
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
