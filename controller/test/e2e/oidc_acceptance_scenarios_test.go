//go:build oidc_acceptance

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/api"
	testissuer "github.com/dsx-ai-factory/nvke/controller/api/testutil/oidc"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/oidc"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/tenantauth"
)

const (
	oidcIssuerHostEnv = "NKE_E2E_OIDC_ISSUER_HOST"
	oidcAudience      = "nke-oidc-acceptance"
	oidcUsername      = "oidc-acceptance-user"
	oidcGroup         = "oidc-acceptance"
	oidcPrefix        = "prefixed:"
	oidcCleanup       = 15 * time.Minute

	oidcSecondAudience = "nke-oidc-acceptance-second"
	oidcSecondPrefix   = "second:"
	oidcSecondTenant   = "second"
)

type oidcAcceptanceRun struct {
	site            *oidc.Site
	issuer          *testissuer.Server
	secondIssuer    *testissuer.Server
	test            tenantTest
	provider        oidc.Provider
	prefixed        oidc.Provider
	second          oidc.Provider
	clusters        []*api.Cluster
	failed          bool
	clusterBFixture *oidcAdminFixture
}

// clusterBProviders is the fleet-wide list cluster B is created with.
func (r *oidcAcceptanceRun) clusterBProviders() []oidc.Provider {
	return []oidc.Provider{r.prefixed, r.second}
}

type oidcAdminFixture struct {
	allowedPath string
	deniedPath  string
	trust       oidc.EndpointTrust
}

var _ = Describe("NVKE OIDC acceptance", Ordered, Serial, Label("oidc-acceptance"), func() {
	run := &oidcAcceptanceRun{}

	AfterEach(func() { run.failed = run.failed || CurrentSpecReport().Failed() })

	BeforeAll(func(ctx SpecContext) {
		host := strings.TrimSpace(os.Getenv(oidcIssuerHostEnv))
		ip := net.ParseIP(host)
		Expect(host).NotTo(BeEmpty(), oidcIssuerHostEnv+" must name the runner address reachable from tenant nodes")
		Expect(ip).NotTo(BeNil(), oidcIssuerHostEnv+" must be an IP address, not a hostname")
		Expect(ip.To4()).NotTo(BeNil(), oidcIssuerHostEnv+" must be the runner's IPv4 address on the Kind network")

		run.issuer = expectSucceed(testissuer.Start(testissuer.Config{
			ListenAddress: "0.0.0.0:0", AdvertiseHost: host, Audience: oidcAudience,
		}))
		DeferCleanup(func() { Expect(run.issuer.Close()).To(Succeed()) })
		run.secondIssuer = expectSucceed(testissuer.Start(testissuer.Config{
			ListenAddress: "0.0.0.0:0", AdvertiseHost: host, Audience: oidcSecondAudience,
		}))
		DeferCleanup(func() { Expect(run.secondIssuer.Close()).To(Succeed()) })

		management := expectSucceed(configuredManagement())
		restConfig := expectSucceed(kube.RESTConfig(management.Kubeconfig, management.Context))
		clientset := expectSucceed(kubernetes.NewForConfig(restConfig))
		run.site = expectSucceed(oidc.NewSite(clientset, oidc.SiteConfig{
			Namespace: controller.Namespace(), Deployment: controller.Deployment(), Container: controller.Deployment(),
			ResetForwarders: kube.ResetSharedForwarders,
		}))
		snapshot := expectSucceed(run.site.Snapshot(ctx))
		DeferCleanup(func(ctx SpecContext) {
			Expect(run.site.Restore(ctx, snapshot)).To(Succeed(), "restore the exact site OIDC state")
		}, NodeTimeout(oidcCleanup))

		timeouts := nvke.Options{Provision: 2 * time.Hour, Delete: time.Hour, Poll: 10 * time.Second}
		run.test = expectSucceed(setupTenantTest(ctx, management, timeouts))
		expectSucceed(oidc.ReservedUsers(ctx, run.test.k8sClient))
		run.provider = oidc.Provider{
			Name: "acceptance", IssuerURL: run.issuer.IssuerURL(), Audiences: []string{run.issuer.Audience()},
			UsernameClaim: "sub", GroupsClaim: "groups", RequiredClaims: map[string]string{"tenant": "acceptance"},
			CABundle: string(run.issuer.CAPEM()),
		}
		// Cluster B pins the same provider with prefixes. A cluster keeps the
		// mapping it was created with, so cluster A covers names that land in a
		// reserved namespace and cluster B covers names moved out of one.
		run.prefixed = run.provider
		run.prefixed.UsernamePrefix, run.prefixed.GroupsPrefix = oidcPrefix, oidcPrefix
		run.second = oidc.Provider{
			Name: "acceptance-second", IssuerURL: run.secondIssuer.IssuerURL(), Audiences: []string{run.secondIssuer.Audience()},
			UsernameClaim: "sub", UsernamePrefix: oidcSecondPrefix, GroupsClaim: "groups", GroupsPrefix: oidcSecondPrefix,
			RequiredClaims: map[string]string{"tenant": oidcSecondTenant}, CABundle: string(run.secondIssuer.CAPEM()),
		}
		DeferCleanup(func(ctx SpecContext) {
			Expect(cleanupOIDCAcceptance(ctx, run.test, &run.clusters, run.failed)).To(Succeed())
			run.clusterBFixture = nil
		}, NodeTimeout(2*time.Hour))
	})

	It("creates cluster A with external OIDC only", func(ctx SpecContext) {
		Expect(run.site.Apply(ctx, []oidc.Provider{run.provider}, false)).To(Succeed())
		createOIDCAcceptanceCluster(ctx, run, "a")
	}, NodeTimeout(2*time.Hour))

	It("proves external RBAC and focused rejection controls on cluster A", func(ctx SpecContext) {
		expectPinnedAuthenticationConfig(ctx, run, run.clusters[0], false, []oidc.Provider{run.provider})
		fixture := expectSucceed(prepareOIDCAdminFixture(ctx, run, run.clusters[0]))
		expectValidExternalProbe(ctx, run, fixture, fixture.trust.Endpoint, "")
		reservedUsers := expectSucceed(oidc.ReservedUsers(ctx, run.test.k8sClient))
		reservedPrefixes := oidc.ReservedPrefixes()
		missingTenant := oidcClaims(oidcUsername)
		delete(missingTenant, "tenant")
		wrongTenant := oidcClaims(oidcUsername)
		wrongTenant["tenant"] = "other"
		negativeTokens := make([]testissuer.TokenSpec, 0, 4+2*len(reservedPrefixes)+2*len(reservedUsers))
		negativeTokens = append(negativeTokens,
			testissuer.TokenSpec{Claims: oidcClaims(oidcUsername), UntrustedKey: true},
			testissuer.TokenSpec{Claims: oidcClaims(oidcUsername), Audience: []string{"wrong-audience"}},
			testissuer.TokenSpec{Claims: missingTenant},
			testissuer.TokenSpec{Claims: wrongTenant},
		)
		// Unprefixed, a token can name a reserved namespace directly, and the
		// API server's CEL rules refuse it. Cluster B maps the same names under
		// its prefix instead.
		for _, prefix := range reservedPrefixes {
			negativeTokens = append(negativeTokens,
				testissuer.TokenSpec{Claims: oidcClaims(prefix + oidcUsername)},
				testissuer.TokenSpec{Claims: oidcClaimsWithGroups(oidcUsername, []string{prefix + oidcGroup})},
			)
		}
		for _, subject := range reservedUsers {
			negativeTokens = append(negativeTokens,
				testissuer.TokenSpec{Claims: oidcClaims(subject)},
				testissuer.TokenSpec{Claims: oidcClaimsWithGroups(oidcUsername, []string{subject})},
			)
		}
		for _, tokenSpec := range negativeTokens {
			expectValidExternalProbe(ctx, run, fixture, fixture.trust.Endpoint, "")
			expectRejected(ctx, run.issuer, fixture, fixture.trust.Endpoint, tokenSpec)
		}
	}, NodeTimeout(15*time.Minute))

	It("creates cluster B with shared OIDC and two prefixed external providers", func(ctx SpecContext) {
		Expect(run.site.Apply(ctx, run.clusterBProviders(), true)).To(Succeed())
		createOIDCAcceptanceCluster(ctx, run, "b")
	}, NodeTimeout(2*time.Hour))

	It("proves external and SPIRE access on cluster B", func(ctx SpecContext) {
		cluster := run.clusters[1]
		fixture := expectSucceed(prepareOIDCAdminFixture(ctx, run, cluster))
		run.clusterBFixture = fixture
		expectValidExternalProbe(ctx, run, fixture, fixture.trust.Endpoint, oidcPrefix)
		expectSPIREAccess(ctx, run, cluster)
	}, NodeTimeout(15*time.Minute))

	It("maps external usernames and groups under the prefix on cluster B", func(ctx SpecContext) {
		fixture := run.clusterBFixture
		Expect(fixture).NotTo(BeNil(), "cluster B's RBAC fixture")
		endpoint := fixture.trust.Endpoint

		By("keeping the prefixed username out of the binding for the unprefixed one")
		result := probeToken(ctx, run.issuer, fixture, endpoint, testissuer.TokenSpec{Claims: oidcClaimsWithGroups(oidcUsername, []string{})})
		Expect(result.IdentityStatus).To(Equal(http.StatusCreated))
		Expect(result.Username).To(Equal(oidcPrefix + oidcUsername))
		Expect(result.Groups).To(ConsistOf("system:authenticated"))
		Expect(result.AllowedStatus).To(Equal(http.StatusForbidden))
		Expect(result.DeniedStatus).To(Equal(http.StatusForbidden))

		By("moving names cluster A refuses out of the reserved namespaces")
		result = probeToken(ctx, run.issuer, fixture, endpoint, testissuer.TokenSpec{Claims: oidcClaimsWithGroups("system:admin", []string{"system:masters"})})
		Expect(result.IdentityStatus).To(Equal(http.StatusCreated))
		Expect(result.Username).To(Equal(oidcPrefix + "system:admin"))
		Expect(result.Groups).To(ConsistOf(oidcPrefix+"system:masters", "system:authenticated"))
		Expect(result.AllowedStatus).To(Equal(http.StatusForbidden))
		Expect(result.DeniedStatus).To(Equal(http.StatusForbidden))
	}, NodeTimeout(15*time.Minute))

	It("keeps the second provider's identities and rules apart on cluster B", func(ctx SpecContext) {
		fixture := run.clusterBFixture
		Expect(fixture).NotTo(BeNil(), "cluster B's RBAC fixture")
		endpoint := fixture.trust.Endpoint

		By("mapping the same subject and group from the second issuer to distinct names")
		expectSecondProviderMapping(ctx, run, fixture, endpoint)

		By("applying each provider's audience and required tenant only to its own tokens")
		expectRejected(ctx, run.issuer, fixture, endpoint, testissuer.TokenSpec{
			Claims: oidcClaims(oidcUsername), Audience: []string{oidcSecondAudience},
		})
		expectRejected(ctx, run.secondIssuer, fixture, endpoint, testissuer.TokenSpec{
			Claims: secondClaims(oidcUsername), Audience: []string{oidcAudience},
		})
		expectRejected(ctx, run.secondIssuer, fixture, endpoint, testissuer.TokenSpec{Claims: oidcClaims(oidcUsername)})
	}, NodeTimeout(15*time.Minute))

	It("rejects a real SPIRE token on cluster A", func(ctx SpecContext) {
		cluster := run.clusters[0]
		identity := controllerIdentity()
		if os.Getenv(dsxIdentityExchangeEnv) == "true" {
			identity.JWTExchangeEndpoint = expectSucceed(tenantauth.KubeconfigExecEnv(
				ctx, run.test.k8sClient, run.clusters[1].Name, "SPIFFE_JWT_EXCHANGE_ENDPOINT",
			))
		}
		Expect(tenantauth.VerifySPIRERejected(ctx, run.test.k8sClient, identity, cluster.Name)).To(Succeed())
	}, NodeTimeout(15*time.Minute))

	It("clears live providers and observes the restarted controller", func(ctx SpecContext) {
		Expect(run.site.Apply(ctx, nil, false)).To(Succeed())
		Expect(run.site.WaitForStartupProviderCount(ctx, 0)).To(Succeed())
	})

	It("joins one control-plane node to cluster B and probes every API endpoint", func(ctx SpecContext) {
		cluster := run.clusters[1]
		currentNodes := expectSucceed(run.test.nvke.ListClusterNodesByControlPlane(ctx, cluster.ID, true))
		Expect(currentNodes).NotTo(BeEmpty())
		desired := len(currentNodes) + 1
		expectSucceed(run.test.nvke.UpdateControlPlaneDesiredCount(ctx, cluster.ID, desired))
		Expect(run.test.nvke.WaitForJoinedControlPlaneNodes(ctx, cluster.ID, desired)).To(Succeed())

		expectPinnedAuthenticationConfig(ctx, run, cluster, true, run.clusterBProviders())
		fixture := run.clusterBFixture
		nodes := expectSucceed(run.test.nvke.ListClusterNodesByControlPlane(ctx, cluster.ID, true))
		Expect(nodes).To(HaveLen(desired))
		for _, node := range nodes {
			Expect(node.IP).NotTo(BeEmpty())
			endpoint := "https://" + net.JoinHostPort(node.IP, "6443")
			expectValidExternalProbe(ctx, run, fixture, endpoint, oidcPrefix)
			expectSecondProviderMapping(ctx, run, fixture, endpoint)
		}
		expectSPIREAccess(ctx, run, cluster)
	}, NodeTimeout(2*time.Hour))

	It("deletes both clusters", func(ctx SpecContext) {
		if run.test.cfg.Lifecycle.SkipDelete {
			Skip("Lifecycle config keeps the clusters")
		}
		Expect(cleanupOIDCAcceptance(ctx, run.test, &run.clusters, run.failed)).To(Succeed())
		run.clusterBFixture = nil
	}, NodeTimeout(2*time.Hour))
})

func createOIDCAcceptanceCluster(ctx context.Context, run *oidcAcceptanceRun, suffix string) {
	GinkgoHelper()
	shape := clusterShape{
		controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
		workerPools:       []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
	}
	spec := capdSpec(run.test.cfg, fmt.Sprintf("%s-oidc-%s", clusterNamePrefix, suffix), shape)
	spec.Name = fmt.Sprintf("%s-%s", spec.Name, spec.Backend)
	spec.ControlPlane.ControlPlaneVIP = expectSucceed(run.test.availableControlPlaneVIP(ctx, preferredControlPlaneVIP(spec.Backend)))
	cluster := expectSucceed(run.test.lifecycle.CreateCluster(ctx, spec))
	run.clusters = append(run.clusters, cluster)
	run.clusters[len(run.clusters)-1] = expectSucceed(run.test.lifecycle.WaitForClusterReady(ctx, cluster.ID))
}

func prepareOIDCAdminFixture(ctx context.Context, run *oidcAcceptanceRun, cluster *api.Cluster) (*oidcAdminFixture, error) {
	admin, err := run.test.tenantClients.GetAdminClient(ctx, cluster.Name)
	if err != nil {
		return nil, fmt.Errorf("get tenant admin client: %w", err)
	}
	kubeconfig, err := run.test.tenantClients.AdminKubeconfig(ctx, cluster.Name)
	if err != nil {
		return nil, fmt.Errorf("get tenant admin kubeconfig: %w", err)
	}
	trust, err := oidc.TrustFromKubeconfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	namespace := "oidc-acceptance-" + uuid.NewString()[:8]
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "allowed", Namespace: namespace}},
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "oidc-reader", Namespace: namespace},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"allowed"}, Verbs: []string{"get"}}},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "oidc-reader", Namespace: namespace},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: oidcUsername}},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "oidc-reader"},
		},
		// Cluster B's prefixed identities reach the ConfigMap only through the
		// prefixed group, never through the unprefixed user binding above.
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "oidc-prefixed-reader", Namespace: namespace},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: oidcPrefix + oidcGroup}},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "oidc-reader"},
		},
	}
	fixture := &oidcAdminFixture{
		trust:       trust,
		allowedPath: "/api/v1/namespaces/" + url.PathEscape(namespace) + "/configmaps/allowed",
		deniedPath:  "/api/v1/namespaces/" + url.PathEscape(namespace) + "/secrets/denied",
	}
	for _, object := range objects {
		if err = admin.Create(ctx, object); err != nil {
			return nil, fmt.Errorf("create OIDC RBAC fixture: %w", err)
		}
	}
	return fixture, nil
}

// expectValidExternalProbe proves a valid external token maps under prefix,
// the cluster's pinned username and groups prefix, and reads only the allowed
// ConfigMap.
func expectValidExternalProbe(ctx context.Context, run *oidcAcceptanceRun, fixture *oidcAdminFixture, endpoint, prefix string) {
	GinkgoHelper()
	result := probeToken(ctx, run.issuer, fixture, endpoint, testissuer.TokenSpec{Claims: oidcClaims(oidcUsername)})
	Expect(result.IdentityStatus).To(Equal(http.StatusCreated))
	Expect(result.Username).To(Equal(prefix + oidcUsername))
	Expect(result.Groups).To(ConsistOf(prefix+oidcGroup, "system:authenticated"))
	Expect(result.AllowedStatus).To(Equal(http.StatusOK))
	Expect(result.DeniedStatus).To(Equal(http.StatusForbidden))
}

// expectSecondProviderMapping proves the second issuer's token for the same
// subject and group as the first maps to distinct, second-prefixed names that
// inherit none of the first provider's access.
func expectSecondProviderMapping(ctx context.Context, run *oidcAcceptanceRun, fixture *oidcAdminFixture, endpoint string) {
	GinkgoHelper()
	result := probeToken(ctx, run.secondIssuer, fixture, endpoint, testissuer.TokenSpec{Claims: secondClaims(oidcUsername)})
	Expect(result.IdentityStatus).To(Equal(http.StatusCreated))
	Expect(result.Username).To(Equal(oidcSecondPrefix + oidcUsername))
	Expect(result.Groups).To(ConsistOf(oidcSecondPrefix+oidcGroup, "system:authenticated"))
	Expect(result.AllowedStatus).To(Equal(http.StatusForbidden))
	Expect(result.DeniedStatus).To(Equal(http.StatusForbidden))
}

func expectRejected(ctx context.Context, issuer *testissuer.Server, fixture *oidcAdminFixture, endpoint string, spec testissuer.TokenSpec) {
	GinkgoHelper()
	result := probeToken(ctx, issuer, fixture, endpoint, spec)
	Expect(result.IdentityStatus).To(Equal(http.StatusUnauthorized))
	Expect(result.AllowedStatus).To(Equal(http.StatusUnauthorized))
	Expect(result.DeniedStatus).To(Equal(http.StatusUnauthorized))
}

func probeToken(ctx context.Context, issuer *testissuer.Server, fixture *oidcAdminFixture, endpoint string, spec testissuer.TokenSpec) oidc.ProbeResult {
	GinkgoHelper()
	token := expectSucceed(issuer.Sign(spec))
	return expectSucceed(oidc.ProbeToken(ctx, endpoint, fixture.trust.CAData, []byte(token), fixture.allowedPath, fixture.deniedPath))
}

// secondClaims is a valid token for the second provider: the first provider's
// subject and group, with the second provider's required tenant.
func secondClaims(username string) map[string]any {
	claims := oidcClaims(username)
	claims["tenant"] = oidcSecondTenant
	return claims
}

func oidcClaims(username string) map[string]any {
	return oidcClaimsWithGroups(username, []string{oidcGroup})
}

func oidcClaimsWithGroups(username string, groups []string) map[string]any {
	return map[string]any{"sub": username, "groups": groups, "tenant": "acceptance"}
}

// expectPinnedAuthenticationConfig reads the AuthenticationConfiguration from
// every control-plane node container and checks it holds the providers the
// cluster was created with, identically on each node, whatever the controller's
// configuration says now.
func expectPinnedAuthenticationConfig(ctx context.Context, run *oidcAcceptanceRun, cluster *api.Cluster, shared bool, providers []oidc.Provider) {
	GinkgoHelper()
	want := oidc.ExpectedAuthentication{
		ClusterName:   cluster.Name,
		Providers:     providers,
		ReservedUsers: expectSucceed(oidc.ReservedUsers(ctx, run.test.k8sClient)),
	}
	if shared {
		want.Shared = expectSucceed(run.site.SharedIssuer(ctx))
	}
	nodes := expectSucceed(run.test.nvke.ListClusterNodesByControlPlane(ctx, cluster.ID, true))
	Expect(nodes).NotTo(BeEmpty())
	var first, firstNode string
	for _, node := range nodes {
		container := expectSucceed(nodeContainerName(ctx, node.Name))
		content := expectSucceed(agentDockerOutput(ctx, "exec", container, "cat", oidc.AuthConfigPath))
		Expect(oidc.CheckAuthenticationConfig([]byte(content), want)).To(Succeed(), "control-plane node %s", node.Name)
		if firstNode == "" {
			first, firstNode = content, node.Name
			continue
		}
		Expect(content).To(Equal(first), "control-plane node %s has a different %s from %s", node.Name, oidc.AuthConfigPath, firstNode)
	}
}

func controllerIdentity() tenantauth.Identity {
	identity := tenantauth.Identity{Namespace: controller.Namespace(), ServiceAccount: controller.ServiceAccount()}
	if os.Getenv(dsxIdentityExchangeEnv) == "true" {
		identity.CABundleConfigMap = dsxExchangeCAConfigMap
	}
	return identity
}

func expectSPIREAccess(ctx context.Context, run *oidcAcceptanceRun, cluster *api.Cluster) {
	GinkgoHelper()
	Eventually(func() error {
		if err := tenantauth.AssertKubeconfigAndTrustBundle(ctx, run.test.k8sClient, cluster.Name); err != nil {
			return err
		}
		return tenantauth.AssertSPIREKubeconfigHasNoFallback(ctx, run.test.k8sClient, cluster.Name)
	}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	expected := "spiffe://" + run.test.trustDomain + "/nvke-service/" + controller.ServiceAccount()
	if os.Getenv(dsxIdentityExchangeEnv) == "true" {
		expected = dsxExchangeControllerUser
	}
	var result tenantauth.VerifyResult
	Eventually(func() error {
		var err error
		result, err = tenantauth.VerifySPIREAuth(ctx, run.test.k8sClient, controllerIdentity(), cluster.Name, expected, []tenantauth.Check{listNodes, deleteNodes})
		return err
	}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	Expect(result.Username).To(Equal(expected))
	Expect(result.CanI[listNodes]).To(BeTrue())
	Expect(result.CanI[deleteNodes]).To(BeTrue())
	Expect(result.ListNodesOK).To(BeTrue())
}
