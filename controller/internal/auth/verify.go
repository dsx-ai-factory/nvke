// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
)

// KeySource supplies one issuer's public keys. A source is bound to its issuer at
// construction, so nothing in the token selects the key set.
type KeySource interface {
	KeyFor(ctx context.Context, kid string) (crypto.PublicKey, error)
}

// ErrKeysStale reports that an issuer's cached key set has aged out and could not
// be refreshed. Reported as ReasonJWKSStale rather than ReasonBadSignature: the controller
// is wrong, not the token.
var ErrKeysStale = errors.New("issuer key set is stale")

// FailureReason labels why authentication or admission failed. These are the
// nvke_auth_requests_total{reason} values, so the set is closed.
type FailureReason string

const (
	ReasonNoToken       FailureReason = "no_token"
	ReasonUnknownIssuer FailureReason = "unknown_issuer"
	// ReasonBadSignature also covers a token that is not a well-formed JWT, and
	// one that carries no "sub" and so names no principal.
	ReasonBadSignature FailureReason = "bad_signature"
	// ReasonExpired covers an absent "exp" too; this package requires it.
	ReasonExpired             FailureReason = "expired"
	ReasonNotYetValid         FailureReason = "not_yet_valid"
	ReasonAudienceMismatch    FailureReason = "audience_mismatch"
	ReasonAlgorithmNotAllowed FailureReason = "algorithm_not_allowed"
	// ReasonJWKSStale is the controller's own failure, not the token's. See ErrKeysStale.
	ReasonJWKSStale FailureReason = "jwks_stale"
	// ReasonNotAdmitted is the only reason the caller renders as 403; every
	// other one is 401.
	ReasonNotAdmitted FailureReason = "not_admitted"
)

const (
	// clockSkewLeeway is applied to both "exp" and "nbf". Kubernetes mints bound
	// service account tokens with nbf == iat and golang-jwt applies no leeway of
	// its own, so a controller pod a second behind the apiserver would reject freshly
	// minted tokens.
	clockSkewLeeway = 60 * time.Second

	// maxLoggedValueBytes bounds a caller-controlled string on its way into a log
	// field. "iss" and "kid" are logged on rejection and read before anything is
	// verified, bounded only by MaxHeaderBytes, so an unauthenticated loop could
	// otherwise fill the node disk.
	maxLoggedValueBytes = 128
)

// elideForLog truncates s to maxLoggedValueBytes on a rune boundary, reporting
// the original length so a truncated value is not mistaken for a whole one.
func elideForLog(s string) string {
	if len(s) <= maxLoggedValueBytes {
		return s
	}
	cut := maxLoggedValueBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s... (truncated from %d bytes)", s[:cut], len(s))
}

// Error carries the metric labels for one failure.
type Error struct {
	Reason FailureReason
	// IssuerName is the Issuer.Name of the matched entry, never the token's
	// "iss", so the metric label's value set is fixed at startup. Empty for a
	// failure raised before the registry lookup.
	IssuerName string
	// Err is safe for the server log and never for the response body.
	Err error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return string(e.Reason)
	}
	return fmt.Sprintf("%s: %v", e.Reason, e.Err)
}

// Unwrap lets errors.Is reach ErrKeysStale and the golang-jwt sentinels.
func (e *Error) Unwrap() error { return e.Err }

func ReasonOf(err error) (FailureReason, bool) {
	var authErr *Error
	if errors.As(err, &authErr) {
		return authErr.Reason, true
	}
	return "", false
}

// IssuerOf returns the configured issuer name carried by err, if any. False when
// the failure happened before any issuer was matched.
func IssuerOf(err error) (string, bool) {
	var authErr *Error
	if errors.As(err, &authErr) && authErr.IssuerName != "" {
		return authErr.IssuerName, true
	}
	return "", false
}

func failure(reason FailureReason, err error) *Error {
	return &Error{Reason: reason, IssuerName: "", Err: err}
}

type issuerEntry struct {
	name       string
	issuer     string
	audiences  []string
	algorithms []string
	admit      *admitRules
	keys       KeySource
	parser     *jwt.Parser
}

// failure attributes a failure to this entry. The two reasons meaning "no issuer
// could be attributed" are refused; ReasonUnknownIssuer reaches here when the
// verified claims disagree with the entry selected on the unverified "iss".
func (e *issuerEntry) failure(reason FailureReason, err error) *Error {
	if reason == ReasonNoToken || reason == ReasonUnknownIssuer {
		return failure(reason, err)
	}
	return &Error{Reason: reason, IssuerName: e.name, Err: err}
}

// Verifier verifies bearer tokens and applies each issuer's admission rules. It
// is immutable after construction and safe for concurrent use.
type Verifier struct {
	// entries is keyed by the exact "iss" value. No prefix matching and no
	// normalization: a prefix match would let
	// "https://evil.example.com.attacker.test" select a trusted key source.
	entries map[string]*issuerEntry

	// unverifiedParser reads "iss" and "alg" before any key is chosen. Its output
	// only selects an entry; jwt.WithIssuer re-checks "iss" once verified.
	unverifiedParser *jwt.Parser
}

// NewVerifier takes a validated configuration and one KeySource per issuer, keyed
// by Issuer.Name. Both directions of mismatch are startup failures.
func NewVerifier(cfg *Config, sources map[string]KeySource) (*Verifier, error) {
	return newVerifier(cfg, sources, true)
}

func newVerifier(cfg *Config, sources map[string]KeySource, requireAdmissionPolicy bool) (*Verifier, error) {
	if cfg == nil {
		return nil, errors.New("authentication config is nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid authentication config: %w", err)
	}
	if requireAdmissionPolicy {
		for index, issuer := range cfg.Issuers {
			if issuer.Admit == nil {
				return nil, fmt.Errorf(
					"invalid authentication config: issuers[%d] (%s): admit is required; an issuer with no admission rules would authenticate callers and then reject all of them",
					index,
					strings.TrimSpace(issuer.Name),
				)
			}
		}
	}

	entries := make(map[string]*issuerEntry, len(cfg.Issuers))
	configured := make(map[string]struct{}, len(cfg.Issuers))
	for _, iss := range cfg.Issuers {
		configured[iss.Name] = struct{}{}
		src, ok := sources[iss.Name]
		if !ok || src == nil {
			return nil, fmt.Errorf("issuer %q (%s): no key source supplied", iss.Name, iss.Issuer)
		}

		var admit *admitRules
		if iss.Admit != nil {
			var err error
			admit, err = iss.Admit.resolve()
			if err != nil {
				return nil, fmt.Errorf("issuer %q (%s): admit: %w", iss.Name, iss.Issuer, err)
			}
		}

		// Cloned because jwt.WithAudience keeps the slice rather than copying it.
		audiences := slices.Clone(iss.EffectiveAudiences(cfg.Audiences))
		algorithms := slices.Clone(iss.Algorithms)
		parserOptions := []jwt.ParserOption{
			// golang-jwt treats "exp" as optional by default, so without this
			// a token minted with no expiry verifies forever.
			jwt.WithExpirationRequired(),
			// Re-checks "iss" against the verified claims, which is what makes
			// the pre-verification read below safe to select a key source on.
			jwt.WithIssuer(iss.Issuer),
			jwt.WithValidMethods(algorithms),
			jwt.WithLeeway(clockSkewLeeway),
			// Any-of: expectAllAud defaults to false, so one matching audience
			// is enough. Not WithAllAudiences, where adding an accepted
			// audience would reject every token not also carrying it.
			jwt.WithAudience(audiences...),
			// jwt.WithIssuedAt is deliberately absent: "iat" says when a token
			// was minted, not when it becomes usable.
		}
		entries[iss.Issuer] = &issuerEntry{
			name:       iss.Name,
			issuer:     iss.Issuer,
			audiences:  audiences,
			algorithms: algorithms,
			admit:      admit,
			keys:       src,
			parser:     jwt.NewParser(parserOptions...),
		}
	}

	for name := range sources {
		if _, ok := configured[name]; !ok {
			return nil, fmt.Errorf("key source %q does not match any configured issuer", name)
		}
	}

	return &Verifier{entries: entries, unverifiedParser: jwt.NewParser()}, nil
}

// Verify authenticates a bearer token. It does not apply admission rules; see
// Admit. Every failure is an *Error.
func (v *Verifier) Verify(ctx context.Context, token string) (*Principal, error) {
	if strings.TrimSpace(token) == "" {
		return nil, failure(ReasonNoToken, errors.New("request carried no bearer token"))
	}

	// Reads "iss" and "alg" to select a key source and nothing more; the claims
	// are attacker-controlled until the signature check below succeeds.
	unverified := jwt.MapClaims{}
	parsed, _, err := v.unverifiedParser.ParseUnverified(token, unverified)
	if err != nil {
		// ErrTokenUnverifiable here means "alg" is missing or names a method the
		// library does not implement, which is not a signature problem.
		if errors.Is(err, jwt.ErrTokenUnverifiable) {
			return nil, failure(ReasonAlgorithmNotAllowed, err)
		}
		return nil, failure(ReasonBadSignature, fmt.Errorf("token is not a well-formed JWT: %w", err))
	}

	issuer, err := unverified.GetIssuer()
	if err != nil {
		return nil, failure(ReasonUnknownIssuer, fmt.Errorf("token has an unreadable \"iss\" claim: %w", err))
	}
	entry, ok := v.entries[issuer]
	if !ok {
		// Elided because "iss" is the caller's.
		return nil, failure(ReasonUnknownIssuer,
			fmt.Errorf("no configured issuer matches %q", elideForLog(issuer)))
	}

	// Checked before parsing so the failure reports as algorithm_not_allowed.
	// jwt.WithValidMethods enforces the same rule but reports
	// ErrTokenSignatureInvalid, indistinguishable from a genuinely bad signature.
	alg, _ := parsed.Header["alg"].(string)
	if !slices.Contains(entry.algorithms, alg) {
		return nil, entry.failure(ReasonAlgorithmNotAllowed,
			fmt.Errorf("issuer %q does not allow algorithm %q (allowed: %s)",
				entry.name, alg, strings.Join(entry.algorithms, ", ")))
	}

	verified := jwt.MapClaims{}
	if _, err := entry.parser.ParseWithClaims(token, verified, entry.keyFunc(ctx)); err != nil {
		return nil, entry.failure(classifyParseError(unverified, err), err)
	}

	subject, err := verified.GetSubject()
	if err != nil {
		return nil, entry.failure(ReasonBadSignature, fmt.Errorf("token has an unreadable \"sub\" claim: %w", err))
	}
	// GetSubject returns ("", nil) for an absent "sub" and the parser does not
	// require the claim, so without this a token with no "sub" authenticates as a
	// Principal nothing can authorize. bad_signature, not not_admitted, whose 403
	// would say the controller knows who this is.
	if subject == "" {
		return nil, entry.failure(ReasonBadSignature,
			errors.New("token carries no \"sub\" claim, so it names no principal"))
	}

	return &Principal{
		IssuerName: entry.name,
		Issuer:     entry.issuer,
		Subject:    subject,
		Claims:     verified,
	}, nil
}

// Admit applies the admission rules of the principal's issuer, returning an
// *Error with ReasonNotAdmitted when none matches.
func (v *Verifier) Admit(p *Principal) error {
	if p == nil {
		return failure(ReasonNotAdmitted, errors.New("no principal to admit"))
	}
	entry, ok := v.entries[p.Issuer]
	if !ok {
		return failure(ReasonUnknownIssuer, fmt.Errorf("no configured issuer matches %q", p.Issuer))
	}
	if entry.admit == nil {
		return entry.failure(ReasonNotAdmitted, errors.New("token verifier has no REST admission policy"))
	}
	if !entry.admit.admits(p) {
		// Elided even though the subject is verified: it is still a caller-chosen
		// string of unbounded length.
		return entry.failure(ReasonNotAdmitted,
			fmt.Errorf("subject %q from issuer %q matches no admission rule",
				elideForLog(p.Subject), entry.name))
	}
	return nil
}

// Authenticate verifies a token and admits the principal behind it.
func (v *Verifier) Authenticate(ctx context.Context, token string) (*Principal, error) {
	p, err := v.Verify(ctx, token)
	if err != nil {
		return nil, err
	}
	if err := v.Admit(p); err != nil {
		return nil, err
	}
	return p, nil
}

func (e *issuerEntry) keyFunc(ctx context.Context) jwt.Keyfunc {
	return func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		key, err := e.keys.KeyFor(ctx, kid)
		// e.issuer is configured and bounded; kid is the caller's.
		if err != nil {
			return nil, fmt.Errorf("no key for issuer %q kid %q: %w", e.issuer, elideForLog(kid), err)
		}
		if key == nil {
			return nil, fmt.Errorf("key source for issuer %q returned no key for kid %q", e.issuer, elideForLog(kid))
		}
		return key, nil
	}
}

// classifyParseError maps a golang-jwt parse failure onto a FailureReason. The
// library joins claim-validation errors, so case order is the reporting
// priority.
func classifyParseError(claims jwt.MapClaims, err error) FailureReason {
	switch {
	case errors.Is(err, ErrKeysStale):
		return ReasonJWKSStale
	case errors.Is(err, jwt.ErrTokenExpired):
		return ReasonExpired
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return ReasonNotYetValid
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return ReasonAudienceMismatch
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return ReasonUnknownIssuer
	case errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		return missingClaimReason(claims)
	default:
		return ReasonBadSignature
	}
}

// missingClaimReason attributes an ErrTokenRequiredClaimMissing to "exp" or
// "aud". Not a presence test: golang-jwt raises this when a claim RESOLVES to
// nothing, and `aud: []`, `aud: ""` and `aud: 1234` are all present and empty.
func missingClaimReason(claims jwt.MapClaims) FailureReason {
	if exp, err := claims.GetExpirationTime(); err == nil && exp == nil {
		return ReasonExpired
	}
	aud, err := claims.GetAudience()
	if err != nil || len(aud) == 0 || (len(aud) == 1 && aud[0] == "") {
		return ReasonAudienceMismatch
	}
	return ReasonBadSignature
}
