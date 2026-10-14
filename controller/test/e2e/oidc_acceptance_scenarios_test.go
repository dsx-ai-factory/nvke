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
	liveoidc "github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/oidc"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/tenantauth"
)

const (
	oidcIssuerHostEnv = "NKE_E2E_OIDC_ISSUER_HOST"
	oidcAudience      = "nke-oidc-acceptance"
	oidcUsername      = "oidc-acceptance-user"
	oidcCleanup       = 15 * time.Minute
)

type oidcAcceptanceRun struct {
	site            *liveoidc.Site
	issuer          *testissuer.Server
	test            tenantTest
	provider        liveoidc.Provider
	clusters        []*api.Cluster
	failed          bool
	clusterBFixture *oidcAdminFixture
}

type oidcAdminFixture struct {
	allowedPath string
	deniedPath  string
	trust       liveoidc.EndpointTrust
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

		management := expectSucceed(configuredManagement())
		restConfig := expectSucceed(kube.RESTConfig(management.Kubeconfig, management.Context))
		clientset := expectSucceed(kubernetes.NewForConfig(restConfig))
		run.site = expectSucceed(liveoidc.NewSite(clientset, liveoidc.SiteConfig{
			Namespace: controller.Namespace(), Deployment: controller.Deployment(), Container: controller.Deployment(),
			ResetForwarders: kube.ResetSharedForwarders,
		}))
		snapshot := expectSucceed(run.site.Snapshot(ctx))
		DeferCleanup(func(ctx SpecContext) {
			Expect(run.site.Restore(ctx, snapshot)).To(Succeed(), "restore the exact site OIDC state")
		}, NodeTimeout(oidcCleanup))

		timeouts := nvke.Options{Provision: 2 * time.Hour, Delete: time.Hour, Poll: 10 * time.Second}
		run.test = expectSucceed(setupTenantTest(ctx, management, timeouts))
		expectSucceed(liveoidc.ReservedUsers(ctx, run.test.k8sClient))
		run.provider = liveoidc.Provider{
			Name: "acceptance", IssuerURL: run.issuer.IssuerURL(), Audiences: []string{run.issuer.Audience()},
			UsernameClaim: "sub", GroupsClaim: "groups", RequiredClaims: map[string]string{"tenant": "acceptance"},
			CABundle: string(run.issuer.CAPEM()),
		}
		DeferCleanup(func(ctx SpecContext) {
			Expect(cleanupOIDCAcceptance(ctx, run.test, &run.clusters, run.failed)).To(Succeed())
			run.clusterBFixture = nil
		}, NodeTimeout(2*time.Hour))
	})

	It("creates cluster A with external OIDC only", func(ctx SpecContext) {
		Expect(run.site.Apply(ctx, []liveoidc.Provider{run.provider}, false)).To(Succeed())
		createOIDCAcceptanceCluster(ctx, run, "a")
	}, NodeTimeout(2*time.Hour))

	It("proves external RBAC and focused rejection controls on cluster A", func(ctx SpecContext) {
		fixture := expectSucceed(prepareOIDCAdminFixture(ctx, run, run.clusters[0]))
		expectValidExternalProbe(ctx, run, fixture, fixture.trust.Endpoint)
		reservedUsers := expectSucceed(liveoidc.ReservedUsers(ctx, run.test.k8sClient))
		negativeTokens := make([]testissuer.TokenSpec, 0, 2+2*len(reservedUsers))
		negativeTokens = append(negativeTokens,
			testissuer.TokenSpec{Claims: oidcClaims(oidcUsername), UntrustedKey: true},
			testissuer.TokenSpec{Claims: oidcClaims(oidcUsername), Audience: []string{"wrong-audience"}},
		)
		for _, subject := range reservedUsers {
			negativeTokens = append(negativeTokens,
				testissuer.TokenSpec{Claims: oidcClaims(subject)},
				testissuer.TokenSpec{Claims: oidcClaimsWithGroups(oidcUsername, []string{subject})},
			)
		}
		for _, tokenSpec := range negativeTokens {
			expectValidExternalProbe(ctx, run, fixture, fixture.trust.Endpoint)
			token := expectSucceed(run.issuer.Sign(tokenSpec))
			result := expectSucceed(liveoidc.ProbeToken(ctx, fixture.trust.Endpoint, fixture.trust.CAData, []byte(token), fixture.allowedPath, fixture.deniedPath))
			Expect(result.IdentityStatus).To(Equal(http.StatusUnauthorized))
			Expect(result.AllowedStatus).To(Equal(http.StatusUnauthorized))
			Expect(result.DeniedStatus).To(Equal(http.StatusUnauthorized))
		}
	}, NodeTimeout(15*time.Minute))

	It("creates cluster B with external and shared OIDC", func(ctx SpecContext) {
		Expect(run.site.Apply(ctx, []liveoidc.Provider{run.provider}, true)).To(Succeed())
		createOIDCAcceptanceCluster(ctx, run, "b")
	}, NodeTimeout(2*time.Hour))

	It("proves external and SPIRE access on cluster B", func(ctx SpecContext) {
		cluster := run.clusters[1]
		fixture := expectSucceed(prepareOIDCAdminFixture(ctx, run, cluster))
		run.clusterBFixture = fixture
		expectValidExternalProbe(ctx, run, fixture, fixture.trust.Endpoint)
		expectSPIREAccess(ctx, run, cluster)
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

		fixture := run.clusterBFixture
		nodes := expectSucceed(run.test.nvke.ListClusterNodesByControlPlane(ctx, cluster.ID, true))
		Expect(nodes).To(HaveLen(desired))
		for _, node := range nodes {
			Expect(node.IP).NotTo(BeEmpty())
			endpoint := "https://" + net.JoinHostPort(node.IP, "6443")
			expectValidExternalProbe(ctx, run, fixture, endpoint)
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
	trust, err := liveoidc.TrustFromKubeconfig(kubeconfig)
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

func expectValidExternalProbe(ctx context.Context, run *oidcAcceptanceRun, fixture *oidcAdminFixture, endpoint string) {
	GinkgoHelper()
	token := expectSucceed(run.issuer.Sign(testissuer.TokenSpec{Claims: oidcClaims(oidcUsername)}))
	result := expectSucceed(liveoidc.ProbeToken(ctx, endpoint, fixture.trust.CAData, []byte(token), fixture.allowedPath, fixture.deniedPath))
	Expect(result.IdentityStatus).To(Equal(http.StatusCreated))
	Expect(result.Username).To(Equal(oidcUsername))
	Expect(result.Groups).To(ContainElements("oidc-acceptance", "system:authenticated"))
	Expect(result.AllowedStatus).To(Equal(http.StatusOK))
	Expect(result.DeniedStatus).To(Equal(http.StatusForbidden))
}

func oidcClaims(username string) map[string]any {
	return oidcClaimsWithGroups(username, []string{"oidc-acceptance"})
}

func oidcClaimsWithGroups(username string, groups []string) map[string]any {
	return map[string]any{"sub": username, "groups": groups, "tenant": "acceptance"}
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
	expected := "spiffe://" + run.test.trustDomain + "/nke-service/" + controller.ServiceAccount()
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
