// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"net/http"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/authtoken"
)

// The mapped-role tier of plan-authz-e2e-tests.md §3/§5 (project docs,
// cwe-api-oidc-auth): roles granted at request time by the authz bundle's
// roleMappings.json from what a verified token says (internal/authz/
// role_mappings.go).
//
// Why e2e and not just internal/authz's own unit tests: parentsFor is
// unit-tested against a hand-built Query. What those tests cannot see is
// whether the middleware actually threads the SAME verified identity
// authentication admitted -- Principal.ServiceAccount(), the one parse
// admission and role mapping share -- into the Query the bundle's mappings
// read. The only way to prove that for what a *real, live-admitted* request
// carries, and not just for a struct this suite assembles by hand, is to send
// one through the real middleware chain.
//
// The PSAT subject-consistency rejection case (plan §3, "highest-value case in
// this whole catalog") is deliberately not implemented here. A bound
// Kubernetes service account token's "sub" and its nested "kubernetes.io"
// claim are minted together by the same TokenRequest call and are therefore
// always consistent by construction -- there is no lever in the real
// TokenRequest API (internal/authtoken's only token source) to make the
// apiserver issue a token where they disagree. The parenthetical alternative
// the plan names -- "a SPIFFE-admitted token that happens to also carry a
// kubernetes.io claim" -- needs a JWT-SVID with a custom claim, which SPIRE's
// Workload API does not hand this environment's exec-auth plugin control over,
// and needs SPIRE pod attestation wired into CAPD in the first place, which
// capd-e2e.yml documents is not done (see authz_test.go's authz-spiffe
// doc comment). Both routes to constructing the real scenario are closed with
// only real, unmodified e2e infra; forcing one open would mean either forging
// a signature (fake) or hand-building the claims/subject pair the way the
// unit tests already do in-process (exactly the thing this suite exists to go
// beyond). The closest honest e2e proxy -- a live PSAT whose granted roles
// are asserted to come only from its own (namespace, name), see "grants a
// role by (namespace, service account) match" below -- is a regression signal
// for the matching logic, not a reproduction of the disagreement the check
// defends against. That specific regression remains a unit-level property
// (auth/authn's TestPrincipalServiceAccount, internal/authz's
// TestMiddlewareCarriesThePrincipalToTheMappings).

// The mapped-role test subject: a dev-fixture ServiceAccount minted only for
// "grants a role by (namespace, service account) match" below, distinct from
// authn's own default admitted identity (authtoken.DefaultNamespace/
// DefaultServiceAccount). See deploy/k8s/nvke-auth-dev.yaml for the identity
// and its grant in nke-authz-role-mappings.
const (
	authzClaimSubjectNamespace      = "nke-clients"
	authzClaimSubjectServiceAccount = "nke-claim-subject"
)

var _ = Describe("NKE API authorization by claim", Label("authz", "authz-claims"), func() {
	var (
		restConfig *rest.Config
		clientset  kubernetes.Interface
		audience   string
		info       authzDeploymentInfo
	)

	BeforeEach(func(ctx SpecContext) {
		restConfig, clientset, audience, info = authzSetup(ctx)
	})

	// A roleMappings.json serviceAccount entry grants a role by (namespace,
	// service account) match. This suite can only mint for identities it
	// already has TokenRequest RBAC for (deploy/k8s/nvke-auth-dev.yaml), so the
	// case runs against nke-clients/nke-claim-subject, a dev-fixture identity
	// minted for exactly this spec. It is deliberately not authn's own default
	// admitted identity (authtoken.DefaultNamespace/DefaultServiceAccount):
	// that identity is nvke.NewClient's authtoken.Discover fallback, which
	// essentially every non-authz e2e spec authenticates as, so it holds admin
	// through its own mapping.
	It("grants a role by (namespace, service account) match", func(ctx SpecContext) {
		namespace, name := authzClaimSubjectNamespace, authzClaimSubjectServiceAccount
		roles := authzRolesOf(info, namespace, name)
		if len(roles) == 0 {
			Fail(fmt.Sprintf(
				"no serviceAccount entry in roleMappings.json (ConfigMap nke-authz-role-mappings) matches %s/%s; add one granting %q in deploy/k8s/nvke-auth-dev.yaml",
				namespace, name, authzRoleReader,
			))
		}

		source, err := authtoken.New(clientset, authtoken.Options{Namespace: namespace, ServiceAccount: name, Audience: audience})
		Expect(err).NotTo(HaveOccurred(), "build a token source for %s/%s", namespace, name)
		client := authnClient(restConfig, source)

		status, body, _ := authnListClusters(ctx, client)
		Expect(status).To(Equal(http.StatusOK),
			"roleMappings.json grants %s/%s role(s) %v by (namespace, service account) match; "+
				"every role this bundle declares includes read, so this must succeed. response body: %s",
			namespace, name, roles, body,
		)

		requireAuthzEnforcing(info)
		status, body = authzUpdateCluster(ctx, client, someClusterID)
		Expect(status).To(Equal(http.StatusForbidden),
			"%s/%s must hold role %q and nothing broader, and updateCluster is in the write group; response body: %s",
			namespace, name, authzRoleReader, body,
		)
	})

	// A roleMappings.json claim entry grants a role from an OIDC
	// "groups"-shaped claim. No environment this repository stands
	// up for e2e configures an issuer this suite can mint a controlled claim
	// for: internal/authtoken mints only bound Kubernetes service account
	// tokens via the real TokenRequest API, whose claim shape
	// ("kubernetes.io") is fixed by the apiserver, not settable per test.
	// Exercising this case for real needs an e2e-reachable IdP (or IdP-shaped
	// issuer) this suite can mint a token from with a chosen claim value --
	// nothing here configures one today. So the spec skips when the deployed
	// bundle declares no claim entry, and fails when it declares one it
	// cannot exercise.
	It("grants a role by OIDC claim match", func() {
		if !slices.ContainsFunc(info.mappings, func(m authzRoleMapping) bool { return m.Claim != nil }) {
			Skip("the deployed bundle's roleMappings.json declares no claim entry, so there is no claim-based role grant to exercise")
		}

		Fail("roleMappings.json declares a claim entry, but this suite cannot mint an OIDC token carrying it; remove the entry from the lane's mappings or extend the suite")
	})
})
