// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package authn holds the NVKE API authentication: the configuration schema, token
// verification, and per-issuer admission. Three states, chosen outside this
// package:
//
//	Disabled   AUTH_CONFIG_PATH unset and apiAuth.enabled false; the middleware
//	           is not in the chain.
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

// ServiceAccount returns the service account p's nested "kubernetes.io" claim
// names. It reports false unless the claim is well formed and p.Subject is
// system:serviceaccount:<namespace>:<name> for the same account.
//
// It reads the nested claim rather than parsing "sub", because "sub" splits
// ambiguously when a part holds a colon, and a wrong split could match another
// account. The two must agree, or a token admitted as one account could be
// treated as another.
func (p *Principal) ServiceAccount() (ServiceAccount, bool) {
	sa, ok := serviceAccountFromClaims(p.Claims)
	if !ok || p.Subject != serviceAccountSubjectPrefix+sa.Namespace+":"+sa.Name {
		return ServiceAccount{}, false
	}
	return sa, true
}

// principalContextKey is unexported so no other package can collide with it.
type principalContextKey struct{}

// ContextWithPrincipal returns a copy of ctx that carries p.
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
