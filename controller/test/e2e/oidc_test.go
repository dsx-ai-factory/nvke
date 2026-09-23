// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"net/url"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/nke"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/tenantauth"
)

var (
	listNodes   = tenantauth.Check{Verb: "list", Resource: "nodes"}
	deleteNodes = tenantauth.Check{Verb: "delete", Resource: "nodes"}
)

const (
	dsxIdentityExchangeEnv = "NKE_E2E_DSX_IDENTITY_EXCHANGE"
	dsxExchangeCAConfigMap = "dsx-identity-exchange-ca"
	dsxExchangeCWEUser     = "spiffe://nke.capd.cloud-workflow-engine.nvid.id/bm/cwe"
)

func validateOIDCPostCreate(test *tenantTest) {
	It("maps NKE's identities and authorizes them through OIDC on the tenant apiserver", func(ctx context.Context) {
		skipIfNKEOIDCDisabled(ctx, test.k8sClient)
		if test.trustDomain == "" && os.Getenv(dsxIdentityExchangeEnv) != "true" {
			Skip("SPIRE trust domain could not be derived from kubeconfig context")
		}

		By("Asserting the OIDC kubeconfig + trust bundle exist")
		// The controller publishes the nke-system secrets asynchronously, so poll
		// rather than checking once.
		Eventually(func() error {
			return tenantauth.AssertKubeconfigAndTrustBundle(ctx, test.k8sClient, test.cluster.Name)
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		cweIdentity := tenantauth.Identity{Namespace: "cloud-workflow-engine", ServiceAccount: "cloud-workflow-engine"}
		cweUser := "spiffe://" + test.trustDomain + "/nke-service/cloud-workflow-engine"
		if os.Getenv(dsxIdentityExchangeEnv) == "true" {
			By("Asserting the published kubeconfig selects the HTTPS token exchange")
			exchangeEndpoint := expectSucceed(tenantauth.KubeconfigExecEnv(
				ctx, test.k8sClient, test.cluster.Name, "SPIFFE_JWT_EXCHANGE_ENDPOINT",
			))
			parsedEndpoint := expectSucceed(url.Parse(exchangeEndpoint))
			Expect(parsedEndpoint.Scheme).To(Equal("https"))
			Expect(parsedEndpoint.Path).To(Equal("/v1/exchange"))

			cweIdentity.CABundleConfigMap = dsxExchangeCAConfigMap
			cweUser = dsxExchangeCWEUser
		}

		By("Verifying the cwe identity authenticates and has the required node permissions")
		verifyIdentity(ctx, test, cweIdentity, cweUser)

		if os.Getenv(dsxIdentityExchangeEnv) == "true" {
			controllers := []struct {
				namespace, serviceAccount, username string
			}{
				{"capi-system", "capi-manager", "spiffe://nke.capd.capi-manager.nvid.id/bm/cwe"},
				{"capi-kubeadm-control-plane-system", "capi-kubeadm-control-plane-manager", "spiffe://nke.capd.capi-kubeadm-control-plane-manager.nvid.id/bm/cwe"},
				{"capd-system", "capd-manager", "spiffe://nke.capd.capd-manager.nvid.id/bm/cwe"},
			}
			for _, controller := range controllers {
				By("Verifying the " + controller.serviceAccount + " identity is exchanged and keeps cluster-admin access")
				verifyIdentity(ctx, test, tenantauth.Identity{
					Namespace:         controller.namespace,
					ServiceAccount:    controller.serviceAccount,
					CABundleConfigMap: dsxExchangeCAConfigMap,
				}, controller.username)
			}
		}
	}, Label("spire-oidc"), SpecTimeout(15*time.Minute))
}

func validateOIDCPostDelete(test *tenantTest) {
	It("deletes the kubeconfig and CA trust bundle secrets", func(ctx context.Context) {
		skipIfNKEOIDCDisabled(ctx, test.k8sClient)

		Eventually(func() error {
			return tenantauth.AssertKubeconfigAndTrustBundleDeleted(ctx, test.k8sClient, test.cluster.Name)
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	}, Label("spire-oidc"))
}

// skipIfNKEOIDCDisabled skips the current spec when the CWE deployment does not
// have tenant OIDC JWT auth enabled (NKE_OIDC_AUTH_ENABLED != true).
func skipIfNKEOIDCDisabled(ctx context.Context, k8s client.Client) {
	GinkgoHelper()
	if !expectSucceed(nke.OIDCEnabled(ctx, k8s)) {
		Skip("OIDC is disabled in the CWE deployment (NKE_OIDC_AUTH_ENABLED != true)")
	}
}

// verifyIdentity runs a credential pod as id and asserts it maps to wantUser,
// has the node permissions CWE needs, and can make a real list call against the
// tenant API.
func verifyIdentity(ctx context.Context, test *tenantTest, id tenantauth.Identity, wantUser string) {
	GinkgoHelper()
	var authResult tenantauth.VerifyResult
	Eventually(func() error {
		var err error
		authResult, err = tenantauth.VerifySPIREAuth(ctx, test.k8sClient, id, test.cluster.Name, wantUser, []tenantauth.Check{listNodes, deleteNodes})
		return err
	}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

	test.log.Info("Tenant auth whoami", "serviceAccount", id.ServiceAccount, "username", authResult.Username)
	Expect(authResult.Username).To(Equal(wantUser))
	Expect(authResult.CanI[listNodes]).To(BeTrue(), "should be allowed to list nodes (can-i)")
	Expect(authResult.CanI[deleteNodes]).To(BeTrue(), "should be allowed to delete nodes (can-i)")
	Expect(authResult.ListNodesOK).To(BeTrue(), "should be able to list nodes (real call)")
}
