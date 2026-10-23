// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package authn holds the NVKE API authentication: the configuration schema, token
// verification, and per-issuer admission. Three states, chosen outside this
// package:
//
//	Disabled   AUTH_CONFIG_PATH unset; the middleware is not in the chain.
//	Permissive evaluate, log and count, never reject.
//	Enforcing  401 on authentication, 403 on admission.
package authn

import (
	"context"
)

// Principal is the authenticated caller, carried on the request context.
type Principal struct {
	// IssuerName is the operator-chosen name of the matching issuer entry. Stable
	// across issuer URL changes, so logs and metric labels use it.
	IssuerName string

	// Issuer is the verified "iss", the URL the token names as its issuer.
	Issuer string

	// Subject is the verified "sub": system:serviceaccount:<ns>:<name> for a
	// bound service account token, the full SPIFFE ID for a JWT-SVID.
	Subject string

	// Claims is the full verified set, for admission rules and logging.
	Claims map[string]any
}

// ServiceAccount returns the (namespace, name) p's nested "kubernetes.io"
// claim names, only when that claim is well-formed and p.Subject is exactly
// system:serviceaccount:<namespace>:<name>. False for anything else,
// including a SPIFFE subject carrying a kubernetes.io claim.
//
// The nested claim is read rather than "sub" parsed, because a colon in a
// namespace name would break the parse in the permissive direction. The two
// must then agree, or a token admitted on one identity could carry a claim
// naming another and be treated as that one.
func (p *Principal) ServiceAccount() (ServiceAccount, bool) {
	if p == nil {
		return ServiceAccount{}, false
	}
	sa, ok := serviceAccountFromClaims(p.Claims)
	if !ok {
		return ServiceAccount{}, false
	}
	if p.Subject != serviceAccountSubjectPrefix+sa.Namespace+":"+sa.Name {
		return ServiceAccount{}, false
	}
	return sa, true
}

// principalContextKey is unexported so no other package can collide with it.
type principalContextKey struct{}

func ContextWithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// PrincipalFromContext returns the Principal carried by ctx, if any. False when
// the request was not authenticated: disabled mode or the anonymous surface.
func PrincipalFromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(*Principal)
	if !ok || p == nil {
		return nil, false
	}
	return p, true
}
