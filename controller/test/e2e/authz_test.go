// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/authtoken"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/cwe"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/kube"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/nke"
)

// The gap this suite closes: internal/authz's 39 tests (conformance_test.go's
// bundleMatrix included) run the Cedar engine directly, or through
// httptest.NewServer with a hand-built auth.Principal in the request context.
// None of them go through a real route, a real deployed Helm chart, a real
// verified token, or prove that internal/auth's Principal actually threads into
// internal/authz's middleware over a live request chain -- the same gap
// authn_test.go/authn_spire_test.go close for authentication. See
// plan-authz-e2e-tests.md (project docs, cwe-api-oidc-auth) for the design this
// suite implements: only its "buildable today" tier (core enforcement, §5).
//
// bundleMatrix (internal/authz/conformance_test.go) is this suite's source of
// truth for method/path/want-status: the same rows, run here against a real
// deployed bundle and a real minted credential instead of an in-process fake
// handler. What is NOT reused from bundleMatrix is its literal subject
// strings (subjectReader et al.): those name deploy/authz/roles.json's
// production bindings, which are deployment-specific by construction -- even
// the existing authn dev fixture (deploy/k8s/nke-auth-dev.yaml) admits a
// different identity than any production auth config would. This suite reads
// whichever bundle is actually deployed (authzDeployment, below) and resolves
// a real subject for each role from it, so it proves the same Cedar semantics
// against whatever roles.json a given environment ships, not a hardcoded
// assumption about one.
var _ = Describe("NKE API authorization", Label("authz"), func() {
	var (
		restConfig *rest.Config
		clientset  kubernetes.Interface
		audience   string
		info       authzDeploymentInfo
	)

	BeforeEach(func(ctx SpecContext) {
		restConfig, clientset, audience, info = authzSetup(ctx)
	})

	// requireAuthzEnforcing skips the denial assertions: in permissive mode the
	// middleware counts would_reject and serves anyway, mirroring authn's own
	// requireEnforcing (authn_test.go). NKE_E2E_REQUIRE_AUTHZ asserts more than
	// "authorization is present" (authzAbsentf's own contract) -- it asserts
	// this environment actually enforces, so a permissive-mode deployment must
	// fail these specific denial-path assertions rather than silently skip
	// them: skipping would let a deployment stuck in permissive mode pass
	// green under require-authz with zero enforcement ever proven. This only
	// gates the denial-path assertions that follow each call site, not the
	// suite as a whole -- permissive mode's allow-path behavior (a request
	// that would be denied is still served) is unaffected.
	requireAuthzEnforcing := func() {
		GinkgoHelper()
		if info.mode == authzModePermissive {
			message := fmt.Sprintf(
				"authorization is PERMISSIVE: %s=true on Deployment %s/%s, so a denied request is counted and then served, and 200 is the correct answer to this case",
				authzPermissiveEnv, cwe.Namespace(), cwe.DeploymentName,
			)
			if authzRequired() {
				Fail(fmt.Sprintf(
					"%s asserts this cluster enforces authorization, and permissive mode makes every denial-path assertion vacuous (a real deny is never observable): %s",
					authzRequireEnv, message,
				))
			}
			Skip(message)
		}
	}

	// The reader role: read succeeds, write is refused. Both cases need only a
	// Kubernetes-bound service account token, which internal/authtoken already
	// mints -- no SPIRE, no new provisioning helper. Why e2e and not just
	// conformance_test.go: proves internal/auth's verified Principal.Subject
	// actually reaches internal/authz's role resolution unchanged, over the
	// real middleware chain server.go wires up, not a Principal this test
	// built by hand.
	Describe("the reader role", func() {
		var source *authtoken.Source

		BeforeEach(func(ctx SpecContext) {
			namespace, name, found := authzServiceAccountSubject(info.entities, authzRoleReader)
			if !found {
				Skip(fmt.Sprintf(
					"the deployed bundle grants role %q to no Kubernetes-service-account-shaped principal (roles.json names only SPIFFE identities for it here); this suite mints only bound service account tokens, so it cannot exercise this role in this deployment",
					authzRoleReader,
				))
			}

			tctx, cancel := context.WithTimeout(ctx, authnTimeout)
			defer cancel()

			// The SPIFFE probe path (below) checks its ServiceAccount exists
			// before proceeding; this discovers a dynamically-shaped principal
			// from the bundle the same way, so it needs the same guard -- and one
			// more besides. deploy/k8s/nke-auth-dev.yaml's nke-e2e RBAC is pinned
			// by resourceNames to specific service accounts, not to whatever
			// roles.json happens to grant "reader" -- so a Forbidden minting a
			// TokenRequest for this principal is an availability question, not a
			// test failure, exactly like an absent ServiceAccount is.
			_, err := clientset.CoreV1().ServiceAccounts(namespace).Get(tctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				Skip(fmt.Sprintf(
					"ServiceAccount %s/%s does not exist: roles.json grants role %q to a principal this environment has not provisioned",
					namespace, name, authzRoleReader,
				))
			}
			Expect(err).NotTo(HaveOccurred(), "read the reader-role principal's service account %s/%s", namespace, name)

			if !authzCanMintTokenFor(tctx, clientset, namespace, name) {
				Skip(fmt.Sprintf(
					"this suite's calling identity has no RBAC to mint a TokenRequest for %s/%s; roles.json grants role %q to this principal, but nothing in this environment's token-minter RBAC (deploy/k8s/nke-auth-dev.yaml or equivalent) authorizes minting one for it",
					namespace, name, authzRoleReader,
				))
			}

			source, err = authtoken.New(clientset, authtoken.Options{
				Namespace: namespace, ServiceAccount: name, Audience: audience,
			})
			Expect(err).NotTo(HaveOccurred(), "build a token source for the reader-role principal %s/%s", namespace, name)
		})

		It("reads and cannot write", func(ctx SpecContext) {
			client := authnClient(restConfig, source)

			status, body, _ := authnListClusters(ctx, client)
			Expect(status).To(Equal(http.StatusOK),
				"the reader role covers read; response body: %s", body)

			requireAuthzEnforcing()
			status, body = authzUpdateCluster(ctx, client, someClusterID)
			Expect(status).To(Equal(http.StatusForbidden),
				"the reader role grants no write action, and updateCluster is in the write group; response body: %s", body)
		})
	})

	// Default-deny: an admitted principal absent from every role's grant is
	// refused every request, over real HTTP -- not just in a hand-built Query
	// (conformance_test.go's TestLocalRefusesAnIncompleteQuery proves the
	// engine's own default-deny; this proves the middleware actually asks it
	// for every route rather than only some).
	//
	// The subject is authn's own default admitted identity
	// (nke-clients/nke-client, authtoken.DefaultNamespace/ServiceAccount) --
	// reused rather than provisioned again, and only used here when the
	// deployed bundle grants it no role, so this case never collides with
	// "the reader role" above naming the same principal.
	It("denies every request to an admitted subject holding no role", func(ctx SpecContext) {
		requireAuthzEnforcing()

		subject := authzServiceAccountSubjectString(authtoken.DefaultNamespace, authtoken.DefaultServiceAccount)
		if roles := authzRolesOf(info.entities, subject); len(roles) > 0 {
			Skip(fmt.Sprintf(
				"the deployed bundle grants %s role(s) %v, so it is not a no-role subject in this deployment",
				subject, roles,
			))
		}

		// roles.json is not the only source of a role: an issuer's own
		// roles.serviceAccounts rule (authz_claims_test.go's "grants a role by
		// (namespace, service account) match" exercises exactly this, and
		// deploy/k8s/nke-auth-dev.yaml configures one for this very identity)
		// grants a role at request time without roles.json ever naming the
		// subject. Skipping only on the check above would let this spec run
		// against a subject that, in reality, holds a role and gets served --
		// a false failure, not a proof of default-deny.
		tctx, cancel := context.WithTimeout(ctx, authnTimeout)
		defer cancel()
		claimRules, err := authzServiceAccountRoleRulesFromCluster(tctx, clientset)
		Expect(err).NotTo(HaveOccurred(), "read the deployment's auth configuration")
		if rule, found := matchingServiceAccountRoleRule(claimRules, authtoken.DefaultNamespace, authtoken.DefaultServiceAccount); found {
			Skip(fmt.Sprintf(
				"issuer %q grants %s/%s role %q by (namespace, service account) match, so it is not a no-role subject in this deployment",
				rule.Issuer, authtoken.DefaultNamespace, authtoken.DefaultServiceAccount, rule.Role,
			))
		}

		source, err := authtoken.New(clientset, authtoken.Options{Audience: audience})
		Expect(err).NotTo(HaveOccurred(), "build a token source for the default admitted identity")
		client := authnClient(restConfig, source)

		status, body, _ := authnListClusters(ctx, client)
		Expect(status).To(Equal(http.StatusForbidden),
			"a subject holding no Cedar role must be refused a read; response body: %s", body)

		status, body = authzDeleteCluster(ctx, client, someClusterID)
		Expect(status).To(Equal(http.StatusForbidden),
			"a subject holding no Cedar role must be refused deleteCluster too, not just the coarse write group; response body: %s", body)
	})

	// The operator and nodeOperator roles are SPIFFE-shaped in every roles.json
	// this repo ships (deploy/authz/roles.json has no Kubernetes-service-account
	// binding for either) -- proving them therefore needs a real JWT-SVID, the
	// same SPIRE Workload API path authn_spire_test.go already uses for
	// authentication. That suite's own precedent is why this one is gated
	// behind its own sub-label rather than folded into the cases above:
	// authn-spire is excluded from capd-e2e.yml's label filter today with the
	// comment "needs SPIRE pod attestation not wired in CAPD", and the same is
	// true here -- these specs will Skip in every environment this repository
	// currently stands up, until a ClusterSPIFFEID + ServiceAccount pair exists
	// that mints exactly the SPIFFE ID the deployed roles.json grants
	// "operator"/"nodeOperator". See authzSpiffeProbeSubject's doc comment for
	// exactly what that pair has to produce.
	Describe("the operator and nodeOperator roles", Label("authz-spiffe"), func() {
		var cweImage string

		BeforeEach(func(ctx SpecContext) {
			tctx, cancel := context.WithTimeout(ctx, authnTimeout)
			defer cancel()
			var err error
			cweImage, err = cweContainerImage(tctx, clientset)
			Expect(err).NotTo(HaveOccurred(), "read the NKE deployment's image")
		})

		runProbe := func(ctx SpecContext, role, namespace, serviceAccount string) map[string]string {
			GinkgoHelper()

			subject, found := authzSpiffeSubject(info.entities, role)
			if !found {
				Skip(fmt.Sprintf(
					"the deployed bundle grants role %q to no SPIFFE-shaped principal, so there is nothing for a JWT-SVID to prove here",
					role,
				))
			}

			runtimeClient, err := kube.RuntimeClient(restConfig)
			Expect(err).NotTo(HaveOccurred(), "build a controller-runtime client")

			tctx, cancel := context.WithTimeout(ctx, authnTimeout)
			defer cancel()
			_, err = clientset.CoreV1().ServiceAccounts(namespace).Get(tctx, serviceAccount, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				Skip(fmt.Sprintf(
					"ServiceAccount %s/%s does not exist: this environment has not registered a probe identity for role %q. "+
						"A ClusterSPIFFEID minting exactly %q for a pod running as this ServiceAccount is required "+
						"(see deploy/k8s/nke-spire-client.yaml for the pattern authn_spire_test.go already uses); "+
						"this suite provisions no SPIRE registration of its own.",
					namespace, serviceAccount, role, subject,
				))
			}
			Expect(err).NotTo(HaveOccurred(), "read the probe's service account")

			pod := authzSpiffeProbePod(cweImage, namespace, serviceAccount, audience)
			_, message, err := kube.RunPod(ctx, runtimeClient, pod, authzProbeContainer, authzProbeTimeout)
			Expect(err).NotTo(HaveOccurred(), "the authz SPIRE probe pod did not complete; its report was %q", message)

			report := parseProbeReport(message)
			Expect(report).NotTo(HaveKey("error"), "the probe never reached NKE; its report was %s", message)
			Expect(report["spiffe_id"]).To(Equal(subject),
				"the probe's SVID subject does not match the identity the deployed bundle grants role %q; "+
					"the ClusterSPIFFEID registered for %s/%s mints the wrong SPIFFE ID. report: %s",
				role, namespace, serviceAccount, message)
			return report
		}

		// Proves both the coarse operators-may-write permit and the named,
		// redundant-on-paper operators-may-delete-clusters permit
		// (deploy/authz/policies.cedar) -- the latter is worth its own e2e
		// assertion specifically because it is the rule most likely to get
		// silently reverted to a blanket forbid again (see the policy's own
		// comment), and only a live decision proves the redundant permit is
		// not quietly doing nothing.
		It("admits the operator role's write and explicit cluster-deletion grants, and fences node lifecycle", func(ctx SpecContext) {
			report := runProbe(ctx, authzRoleOperator, authzOperatorProbeNamespace, authzOperatorProbeServiceAccount)

			requireAuthzEnforcing()
			Expect(report["write"]).NotTo(Equal(strconv.Itoa(http.StatusForbidden)),
				"the operator role's coarse write grant must not be refused; report: %s", report)
			Expect(report["delete_cluster"]).NotTo(Equal(strconv.Itoa(http.StatusForbidden)),
				"operators-may-delete-clusters must not be refused; report: %s", report)
			Expect(report["delete_node"]).To(Equal(strconv.Itoa(http.StatusForbidden)),
				"node lifecycle is carved out of the coarse write grant by only-node-operators-change-node-lifecycle; an operator must be refused it; report: %s", report)
		})

		// The one place the shipped policy demonstrates forbid-over-unless
		// carving a narrow role out of a coarse grant (policies.cedar,
		// only-node-operators-change-node-lifecycle). Real e2e value: proves the
		// unless clause resolves role membership from a live SPIFFE-derived
		// subject, not a synthetic Go Query -- and that nodeOperator's parent
		// edge to reader (roles.json) does not leak incidental write access, the
		// deleteCluster grant included.
		It("carves node lifecycle out for the nodeOperator role, and nothing else", func(ctx SpecContext) {
			report := runProbe(ctx, authzRoleNodeOperator, authzNodeOperatorProbeNamespace, authzNodeOperatorProbeServiceAccount)

			requireAuthzEnforcing()
			Expect(report["delete_node"]).NotTo(Equal(strconv.Itoa(http.StatusForbidden)),
				"node-operators-may-change-node-lifecycle must admit the nodeOperator role; report: %s", report)
			Expect(report["write"]).To(Equal(strconv.Itoa(http.StatusForbidden)),
				"nodeOperator does not nest operator, so ordinary writes outside node lifecycle must still be refused; report: %s", report)
			Expect(report["delete_cluster"]).To(Equal(strconv.Itoa(http.StatusForbidden)),
				"nodeOperator holds no operators-may-delete-clusters grant; report: %s", report)
		})
	})
})

// someClusterID/someNodeID/someOtherClusterID mirror conformance_test.go's
// fixed UUIDs: Cedar's decision does not depend on the resource existing, only
// on the route's declared action, so a non-existent ID makes a 403 unambiguous
// (a permitted-but-absent request would 404, never 403) without this suite
// needing to provision a real cluster.
const (
	someClusterID      = "11111111-1111-1111-1111-111111111111"
	someNodeID         = "22222222-2222-2222-2222-222222222222"
	someOtherClusterID = "44444444-4444-4444-4444-444444444444"
)

// The environment deciding whether the authorization middleware is in the
// chain and enforces, mirroring authn's AUTH_CONFIG_PATH/PERMISSIVE_MODE pair
// (authn_test.go) but for the two vars internal/authz/setup.go and
// internal/config/config.go actually read.
const (
	authzBundlePathEnv = "AUTHZ_BUNDLE_PATH"
	authzPermissiveEnv = "AUTHZ_PERMISSIVE"
)

const (
	authzModeDisabled   = "disabled"
	authzModePermissive = "permissive"
	authzModeEnforcing  = "enforcing"
)

// authzBundleVolumeName is the volume name deploy/charts/controller/templates/
// deployment.yaml always mounts the bundle under, regardless of which
// ConfigMap apiAuthz.configMap.name points it at -- so this suite resolves the
// ConfigMap name from the live Deployment rather than assuming the chart
// default.
const authzBundleVolumeName = "nke-authz-policy"

// authzRolesFileKey is rolesFile from internal/authz/bundle.go, duplicated
// here rather than imported: that constant is unexported, and the bundle's
// file layout is the deployment's contract with anything that reads it, this
// suite included.
const authzRolesFileKey = "roles.json"

const (
	authzRoleReader       = "reader"
	authzRoleOperator     = "operator"
	authzRoleNodeOperator = "nodeOperator"
)

// authzDeploymentInfo is what a BeforeEach needs to decide what to run: the
// enforcement mode and the bundle's entities, read once per spec.
type authzDeploymentInfo struct {
	mode     string
	entities []authzEntity
}

// authzEntity is one row of roles.json: a Cedar entity with its parent set.
// Mirrors internal/authz's own (unexported) entity decoding closely enough to
// answer "which principals hold which role", the only question this suite
// asks of the bundle.
type authzEntity struct {
	UID     authzEntityRef   `json:"uid"`
	Parents []authzEntityRef `json:"parents"`
}

type authzEntityRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

const (
	authzRoleEntityType      = "NKE::Role"
	authzPrincipalEntityType = "NKE::Principal"
)

// authzSetup is the discovery every authz spec's BeforeEach needs: the
// cluster, the audience authn discovers, and the deployed bundle's mode and
// entities. Shared between authz_test.go and authz_claims_test.go, which had
// this sequence duplicated near-verbatim.
func authzSetup(ctx SpecContext) (*rest.Config, kubernetes.Interface, string, authzDeploymentInfo) {
	GinkgoHelper()

	restConfig, clientset := authnCluster()

	tctx, cancel := context.WithTimeout(ctx, authnTimeout)
	defer cancel()

	// Authorization requires authentication (config.Validate refuses the
	// reverse), and every credential this suite presents is minted against
	// the audience authn discovers, so an absent authn deployment is an
	// absent authz deployment too, reported in authn's own terms.
	audience, authnFound, _ := authnDeployment(tctx, clientset)
	if !authnFound {
		authzAbsentf(
			"authorization requires authentication, and ConfigMap %s/%s does not exist, so there is no audience to mint a credential against",
			cwe.Namespace(), authtoken.ConfigMapName,
		)
	}

	info, err := authzDeployment(tctx, clientset)
	Expect(err).NotTo(HaveOccurred(), "read the NKE deployment's authorization configuration")
	if info.mode == authzModeDisabled {
		authzAbsentf(
			"authorization is OFF: %s is not set on Deployment %s/%s, so the authorization middleware is not in the request chain",
			authzBundlePathEnv, cwe.Namespace(), cwe.DeploymentName,
		)
	}

	return restConfig, clientset, audience, info
}

// authzDeployment reads the Deployment once and returns its authorization
// mode and, when a bundle is configured, its roles.json entities. A bundle
// that is configured but unreadable is an error, never "disabled": the same
// posture authn_test.go takes for its own ConfigMap (authnDeployment) -- a
// bundle nobody can read is a broken deployment, not an absent feature.
func authzDeployment(ctx context.Context, clientset kubernetes.Interface) (authzDeploymentInfo, error) {
	deployment, err := clientset.AppsV1().Deployments(cwe.Namespace()).Get(ctx, cwe.DeploymentName, metav1.GetOptions{})
	if err != nil {
		return authzDeploymentInfo{}, fmt.Errorf("getting Deployment %s/%s: %w", cwe.Namespace(), cwe.DeploymentName, err)
	}
	container := cweContainer(deployment)
	if container == nil {
		return authzDeploymentInfo{}, fmt.Errorf("deployment %s/%s has no container named %s", cwe.Namespace(), cwe.DeploymentName, cwe.DeploymentName)
	}

	if _, found := containerEnvValue(container, authzBundlePathEnv); !found {
		return authzDeploymentInfo{mode: authzModeDisabled}, nil
	}
	mode := authzModeEnforcing
	if value, _ := containerEnvValue(container, authzPermissiveEnv); strings.EqualFold(strings.TrimSpace(value), "true") {
		mode = authzModePermissive
	}

	configMapName, found := authzBundleConfigMapName(deployment)
	if !found {
		return authzDeploymentInfo{}, fmt.Errorf(
			"deployment %s/%s sets %s but has no %q volume backed by a ConfigMap",
			cwe.Namespace(), cwe.DeploymentName, authzBundlePathEnv, authzBundleVolumeName,
		)
	}
	cm, err := clientset.CoreV1().ConfigMaps(cwe.Namespace()).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return authzDeploymentInfo{}, fmt.Errorf("reading ConfigMap %s/%s: %w", cwe.Namespace(), configMapName, err)
	}
	raw, ok := cm.Data[authzRolesFileKey]
	if !ok {
		return authzDeploymentInfo{}, fmt.Errorf("ConfigMap %s/%s has no %q key", cwe.Namespace(), configMapName, authzRolesFileKey)
	}
	var entities []authzEntity
	if err := json.Unmarshal([]byte(raw), &entities); err != nil {
		return authzDeploymentInfo{}, fmt.Errorf("parsing %s from ConfigMap %s/%s: %w", authzRolesFileKey, cwe.Namespace(), configMapName, err)
	}
	return authzDeploymentInfo{mode: mode, entities: entities}, nil
}

// authzBundleConfigMapName finds the bundle volume's backing ConfigMap name.
// Resolved from the volume rather than a fixed name: unlike authn's
// nke-auth-config (authtoken.ConfigMapName, a constant), apiAuthz.configMap.name
// is a chart value a deployment can set to anything, so the only name this
// suite can trust is whatever the live Deployment actually mounts.
func authzBundleConfigMapName(deployment *appsv1.Deployment) (string, bool) {
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name != authzBundleVolumeName {
			continue
		}
		if volume.ConfigMap == nil {
			return "", false
		}
		return volume.ConfigMap.Name, true
	}
	return "", false
}

// authzRolesOf returns the Cedar role names directly granted to principalID,
// or nil if the bundle names no such principal.
func authzRolesOf(entities []authzEntity, principalID string) []string {
	for _, entity := range entities {
		if entity.UID.Type != authzPrincipalEntityType || entity.UID.ID != principalID {
			continue
		}
		var roles []string
		for _, parent := range entity.Parents {
			if parent.Type == authzRoleEntityType {
				roles = append(roles, parent.ID)
			}
		}
		return roles
	}
	return nil
}

// authzServiceAccountSubject finds the first Kubernetes-service-account-shaped
// principal (system:serviceaccount:<ns>:<name>) the bundle grants role, so a
// PSAT-based case can mint for it via internal/authtoken. found is false when
// the role is granted only to SPIFFE-shaped principals, or not granted at all.
func authzServiceAccountSubject(entities []authzEntity, role string) (string, string, bool) {
	for _, entity := range entities {
		if entity.UID.Type != authzPrincipalEntityType {
			continue
		}
		ns, n, ok := parseServiceAccountSubject(entity.UID.ID)
		if !ok {
			continue
		}
		for _, parent := range entity.Parents {
			if parent.Type == authzRoleEntityType && parent.ID == role {
				return ns, n, true
			}
		}
	}
	return "", "", false
}

// authzCanMintTokenFor reports whether the calling identity is authorized to
// create a TokenRequest for namespace/name, via a SelfSubjectAccessReview
// rather than attempting the mint and inspecting the error: this asks before
// minting, the same "check first, Skip cleanly" posture the SPIFFE probe path
// takes for its own ServiceAccount (apierrors.IsNotFound), extended to an RBAC
// question a plain Get cannot answer.
func authzCanMintTokenFor(ctx context.Context, clientset kubernetes.Interface, namespace, name string) bool {
	GinkgoHelper()
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace:   namespace,
				Verb:        "create",
				Resource:    "serviceaccounts",
				Subresource: "token",
				Name:        name,
			},
		},
	}
	result, err := clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "SelfSubjectAccessReview for TokenRequest on %s/%s", namespace, name)
	return result.Status.Allowed
}

// authzSpiffeSubject is authzServiceAccountSubject for SPIFFE-shaped
// principals, returning the literal spiffe:// subject a JWT-SVID must carry to
// hold role.
func authzSpiffeSubject(entities []authzEntity, role string) (string, bool) {
	for _, entity := range entities {
		if entity.UID.Type != authzPrincipalEntityType || !strings.HasPrefix(entity.UID.ID, "spiffe://") {
			continue
		}
		for _, parent := range entity.Parents {
			if parent.Type == authzRoleEntityType && parent.ID == role {
				return entity.UID.ID, true
			}
		}
	}
	return "", false
}

// authzServiceAccountPrefix begins the "sub" of every bound service account
// token, mirroring internal/auth's own unexported serviceAccountSubjectPrefix
// (admit.go) -- duplicated because that constant is unexported and this
// suite parses the same shape from the outside, not through internal/auth.
const authzServiceAccountPrefix = "system:serviceaccount:"

func parseServiceAccountSubject(subject string) (string, string, bool) {
	rest, ok := strings.CutPrefix(subject, authzServiceAccountPrefix)
	if !ok {
		return "", "", false
	}
	ns, n, ok := strings.Cut(rest, ":")
	if !ok || ns == "" || n == "" {
		return "", "", false
	}
	return ns, n, true
}

func authzServiceAccountSubjectString(namespace, name string) string {
	return authzServiceAccountPrefix + namespace + ":" + name
}

// authzRequireEnv turns every "not deployed" skip into a failure, mirroring
// authn's NKE_E2E_REQUIRE_AUTHN (authn_test.go).
const authzRequireEnv = "NKE_E2E_REQUIRE_AUTHZ"

func authzRequired() bool {
	GinkgoHelper()
	required, err := parseRequireFlag(authzRequireEnv, os.Getenv(authzRequireEnv))
	Expect(err).NotTo(HaveOccurred())
	return required
}

// authzAbsentf ends a spec that found no authorization deployed, skipping by
// default and failing when authzRequireEnv asserts enforcement -- the same
// absence-only contract authnAbsentf documents.
func authzAbsentf(format string, args ...any) {
	GinkgoHelper()
	message := fmt.Sprintf(format, args...)
	if authzRequired() {
		Fail(fmt.Sprintf("%s asserts this cluster authorizes, and it does not. %s", authzRequireEnv, message))
	}
	Skip(message)
}

// authzUpdateCluster probes PATCH /api/clusters/{id} with an empty body: every
// field UpdateClusterRequest declares is optional, so an empty object is a
// well-formed request and the response status is entirely about
// authorization, not validation.
func authzUpdateCluster(ctx context.Context, client *nke.Client, id string) (int, []byte) {
	GinkgoHelper()
	tctx, cancel := context.WithTimeout(ctx, authnTimeout)
	defer cancel()
	resp, err := client.API().UpdateClusterWithResponse(tctx, mustUUID(id), api.UpdateClusterJSONRequestBody{})
	Expect(err).NotTo(HaveOccurred(), "PATCH /api/clusters/%s", id)
	Expect(resp.HTTPResponse).NotTo(BeNil())
	return resp.StatusCode(), resp.Body
}

// authzDeleteCluster probes DELETE /api/clusters/{id}. No body to reason
// about here, unlike authzUpdateCluster above: DELETE carries none, so there
// is no validation for a response status to be ambiguous with in the first
// place.
func authzDeleteCluster(ctx context.Context, client *nke.Client, id string) (int, []byte) {
	GinkgoHelper()
	tctx, cancel := context.WithTimeout(ctx, authnTimeout)
	defer cancel()
	resp, err := client.API().DeleteClusterWithResponse(tctx, mustUUID(id))
	Expect(err).NotTo(HaveOccurred(), "DELETE /api/clusters/%s", id)
	Expect(resp.HTTPResponse).NotTo(BeNil())
	return resp.StatusCode(), resp.Body
}

func mustUUID(s string) openapi_types.UUID {
	return uuid.MustParse(s)
}

// The SPIFFE probe scaffolding: the same shape as authn_spire_test.go's own
// (spireProbePod, spireProbeContainer, parseProbeReport), extended to report
// three status codes instead of two. Kept in this file because it is specific
// to authz's own probe script and probe namespaces, which differ from authn's.
const (
	authzProbeContainer = "probe"
	authzProbeTimeout   = 5 * time.Minute
)

// The probe identities this suite looks for. They name no fixture this repo
// ships: an operator deploying authz e2e coverage for real has to register a
// ClusterSPIFFEID (mirroring deploy/k8s/nke-spire-client.yaml's pattern) that
// mints exactly the SPIFFE ID the deployed bundle's roles.json grants
// "operator"/"nodeOperator", for a pod running as this ServiceAccount --
// overridable so that registration can name whatever identity it wants to.
const (
	authzOperatorProbeNamespaceEnv          = "NKE_E2E_AUTHZ_OPERATOR_NAMESPACE"
	authzOperatorProbeServiceAccountEnv     = "NKE_E2E_AUTHZ_OPERATOR_SERVICE_ACCOUNT"
	authzNodeOperatorProbeNamespaceEnv      = "NKE_E2E_AUTHZ_NODE_OPERATOR_NAMESPACE"
	authzNodeOperatorProbeServiceAccountEnv = "NKE_E2E_AUTHZ_NODE_OPERATOR_SERVICE_ACCOUNT"
)

var (
	authzOperatorProbeNamespace          = envOr(authzOperatorProbeNamespaceEnv, "nke-authz-clients")
	authzOperatorProbeServiceAccount     = envOr(authzOperatorProbeServiceAccountEnv, "nke-authz-operator")
	authzNodeOperatorProbeNamespace      = envOr(authzNodeOperatorProbeNamespaceEnv, "nke-authz-clients")
	authzNodeOperatorProbeServiceAccount = envOr(authzNodeOperatorProbeServiceAccountEnv, "nke-authz-nodeoperator")
)

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// authzSpiffeProbeScript fetches one JWT-SVID for audience and reports the
// status of three real calls: a write (PATCH a cluster), a node-lifecycle
// delete, and a cluster delete. One SVID and three calls in a single probe run
// keeps the pod count (and the attestation wait, the slow part) at one per
// role rather than one per assertion.
const authzSpiffeProbeScript = `
set -uo pipefail

SOCKET="${SPIFFE_ENDPOINT_SOCKET:-unix:///spiffe-workload-api/spire-agent.sock}"
EXEC_AUTH="${SPIFFE_JWT_EXEC_AUTH:-/opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth}"
SVID_TIMEOUT="${SVID_TIMEOUT:-90s}"
REPORT="${REPORT:-/dev/termination-log}"

report() { printf '%s\n' "$*"; printf '%s\n' "$*" >"${REPORT}"; }
fail() { report "error=$*"; exit 1; }

[[ -x "${EXEC_AUTH}" ]] || fail "no JWT-SVID fetcher at ${EXEC_AUTH}"
[[ -n "${NKE_URL:-}" ]] || fail "NKE_URL is unset"
[[ -n "${NKE_AUDIENCE:-}" ]] || fail "NKE_AUDIENCE is unset"

token="$(SPIFFE_ENDPOINT_SOCKET="${SOCKET}" SPIFFE_JWT_AUDIENCE="${NKE_AUDIENCE}" \
  "${EXEC_AUTH}" -timeout "${SVID_TIMEOUT}" 2>/dev/null \
  | awk '$1 == "token:" { print $2; exit }')"
[[ -n "${token}" ]] || fail "no JWT-SVID for audience ${NKE_AUDIENCE} from ${SOCKET} within ${SVID_TIMEOUT}"

payload="${token#*.}"
payload="${payload%%.*}"
payload="${payload//-/+}"
payload="${payload//_//}"
case $(( ${#payload} % 4 )) in
  2) payload="${payload}==" ;;
  3) payload="${payload}=" ;;
esac
spiffe_id="$(printf '%s' "${payload}" | base64 -d 2>/dev/null | sed -n 's/.*"sub":"\([^"]*\)".*/\1/p')"
[[ -n "${spiffe_id}" ]] || fail "the JWT-SVID carries no sub claim"

call() {
  curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X "$1" \
    -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' -d '{}' \
    "${NKE_URL}$2"
}

write_status="$(call PATCH "/api/clusters/${AUTHZ_CLUSTER_ID}")"
delete_node_status="$(call DELETE "/api/nodes/${AUTHZ_NODE_ID}")"
delete_cluster_status="$(call DELETE "/api/clusters/${AUTHZ_DELETE_CLUSTER_ID}")"

report "spiffe_id=${spiffe_id} write=${write_status} delete_node=${delete_node_status} delete_cluster=${delete_cluster_status}"
`

// authzSpiffeProbePod is the one-shot probe pod for the operator/nodeOperator
// cases, structurally identical to authn_spire_test.go's spireProbePod (same
// SPIFFE CSI mount, same image, same termination-message report) but running
// authzSpiffeProbeScript inline instead of a ConfigMap-mounted script: this
// suite ships no probe.sh of its own, so there is nothing to keep in sync with
// a mounted copy.
func authzSpiffeProbePod(image, namespace, serviceAccount, audience string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "nke-authz-probe-",
			Namespace:    namespace,
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "cloud-workflow-engine",
				"nke.nvidia.com/purpose":    "nke-authz-dev",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: serviceAccount,
			Containers: []corev1.Container{{
				Name:            authzProbeContainer,
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/bin/bash", "-c", authzSpiffeProbeScript},
				Env: []corev1.EnvVar{
					{Name: "NKE_AUDIENCE", Value: audience},
					{Name: "NKE_URL", Value: fmt.Sprintf("http://%s.%s.svc:%s", cwe.Service(), cwe.Namespace(), cwe.Port())},
					{Name: "AUTHZ_CLUSTER_ID", Value: someClusterID},
					{Name: "AUTHZ_NODE_ID", Value: someNodeID},
					{Name: "AUTHZ_DELETE_CLUSTER_ID", Value: someOtherClusterID},
				},
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
				VolumeMounts: []corev1.VolumeMount{
					{Name: spireWorkloadAPIVolume, MountPath: spireWorkloadAPIMount, ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{
					Name: spireWorkloadAPIVolume,
					VolumeSource: corev1.VolumeSource{
						CSI: &corev1.CSIVolumeSource{Driver: spireCSIDriver, ReadOnly: ptr.To(true)},
					},
				},
			},
		},
	}
}
