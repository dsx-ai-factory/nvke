// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/tenantauth"
)

// The apiserver auth-config settings the brownfield write-back persists into
// kube-system/kubeadm-config; must match internal/kubeadmconfig on the controller side.
const (
	oidcAuthConfigFlag       = "authentication-config"
	oidcAuthConfigPath       = "/etc/kubernetes/oidc/auth-config.yaml"
	oidcAuthConfigVolumeName = "oidc-auth-config"
	oidcAuthConfigDir        = "/etc/kubernetes/oidc"
)

// validateBrownfieldOIDC turns OIDC on for an already-provisioned (OIDC-off)
// cluster through the canonical front door: the UpdateCluster{enableOidc:true}
// API, which takes the UpdateInProgress lock and enqueues the EnableOIDC workflow
// via the outbox. It is two ordered specs on the same cluster: the first proves
// the enablement (API trigger + kubeadm-config write-back + secret publish),
// which is the brownfield feature itself; the second asserts a SPIRE identity
// authenticates on the tenant apiserver (the full SPIRE->apiserver JWT trust
// chain). Requires the control-plane agent at >= v0.1.29
// (PatchAPIServerManifest); the operation's Validate fails closed otherwise.
//
// The "creates the cluster" step turned the controller OIDC create-default off so this
// cluster is born OIDC-off (a cluster on an OIDC-enabled site is otherwise always
// born OIDC-on, which would make the enable a no-op). This spec flips it back on
// before enabling; the OIDC issuer/discovery/CA config the workflow needs was
// never removed, so only the create-default toggles.
func validateBrownfieldOIDC(test *tenantTest) {
	It("enables OIDC on an existing cluster and persists the kubeadm-config write-back", func(ctx context.Context) {
		By("Re-enabling the controller OIDC create-default turned off at cluster creation")
		Expect(nvke.SetOIDCEnabled(ctx, test.k8sClient, true)).To(Succeed())

		skipIfNVKEOIDCDisabled(ctx, test.k8sClient)

		By("Waiting for every desired node to be ready")
		Eventually(test.assertDesiredNodesReady).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(1*time.Second).
			Should(Succeed(), "all desired nodes should be ready before enabling OIDC")

		By("Enabling OIDC via the UpdateCluster API")
		resp := expectSucceed(test.nvke.UpdateOIDC(ctx, test.cluster.ID, true))
		Expect(resp.StatusCode()).To(Equal(200), string(resp.Body))
		Expect(resp.JSON200).NotTo(BeNil())
		Expect(resp.JSON200.Conditions.UpdateInProgress).NotTo(BeNil(),
			"UpdateCluster should take the enablement lock synchronously")
		Expect(resp.JSON200.Conditions.UpdateInProgress.Type).To(Equal(api.UpdateInProgressTypeOidcEnable))

		By("Waiting for OIDC enablement to complete")
		test.cluster = expectSucceed(test.nvke.WaitForOIDCEnabled(ctx, test.cluster.ID))

		By("Validating the auth-config was written back into kubeadm-config")
		tenantClient := expectSucceed(test.tenantClients.GetClient(ctx, test.cluster.Name))
		Expect(validateKubeadmOIDCConfig(ctx, tenantClient)).To(Succeed())

		By("Asserting the OIDC kubeconfig + trust bundle are published")
		// The tenant-access controller publishes the nke-system secrets once it
		// observes the flipped marker, so poll.
		Eventually(func() error {
			return tenantauth.AssertKubeconfigAndTrustBundle(ctx, test.k8sClient, test.cluster.Name)
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	}, Label("lifecycle", "oidc", "oidc-enable"), NodeTimeout(20*time.Minute))

	It("authenticates the controller SPIRE identity on the tenant apiserver", func(ctx context.Context) {
		skipIfNVKEOIDCDisabled(ctx, test.k8sClient)
		if test.trustDomain == "" {
			Skip("SPIRE trust domain could not be derived from kubeconfig context")
		}

		By("Verifying the controller identity authenticates and can list nodes")
		verifyIdentity(ctx, test,
			tenantauth.Identity{Namespace: controller.Namespace(), ServiceAccount: controller.ServiceAccount()},
			"spiffe://"+test.trustDomain+"/nke-service/"+controller.ServiceAccount())
	}, Label("lifecycle", "oidc", "oidc-enable"), NodeTimeout(10*time.Minute))
}

// validateKubeadmOIDCConfig asserts the brownfield write-back set the
// authentication-config apiserver arg in kube-system/kubeadm-config, so a later
// kubeadm re-render keeps OIDC. The auth-config volume is covered byte-for-byte
// by the controller-side kubeadmconfig golden unit tests.
func validateKubeadmOIDCConfig(ctx context.Context, tenantClient client.Client) error {
	var configMap corev1.ConfigMap
	if err := tenantClient.Get(ctx, client.ObjectKey{
		Namespace: kubeadmConfigMapNamespace,
		Name:      kubeadmConfigMapName,
	}, &configMap); err != nil {
		return fmt.Errorf("getting kubeadm ConfigMap: %w", err)
	}
	clusterConfiguration, ok := configMap.Data[kubeadmConfigDataKey]
	if !ok || strings.TrimSpace(clusterConfiguration) == "" {
		return fmt.Errorf("kubeadm ConfigMap is missing non-empty %q data", kubeadmConfigDataKey)
	}
	extraArgs, err := parseKubeadmExtraArgs(clusterConfiguration)
	if err != nil {
		return err
	}
	if got := extraArgs[oidcAuthConfigFlag]; got != oidcAuthConfigPath {
		return fmt.Errorf("kubeadm apiServer.extraArgs[%q] = %q, want %q",
			oidcAuthConfigFlag, got, oidcAuthConfigPath)
	}

	volume, err := kubeadmAPIServerExtraVolume(clusterConfiguration, oidcAuthConfigVolumeName)
	if err != nil {
		return err
	}
	if got, _ := volume["hostPath"].(string); got != oidcAuthConfigDir {
		return fmt.Errorf("kubeadm apiServer.extraVolumes[%q].hostPath = %q, want %q",
			oidcAuthConfigVolumeName, got, oidcAuthConfigDir)
	}
	if got, _ := volume["mountPath"].(string); got != oidcAuthConfigDir {
		return fmt.Errorf("kubeadm apiServer.extraVolumes[%q].mountPath = %q, want %q",
			oidcAuthConfigVolumeName, got, oidcAuthConfigDir)
	}
	if readOnly, _ := volume["readOnly"].(bool); !readOnly {
		return fmt.Errorf("kubeadm apiServer.extraVolumes[%q] should be readOnly", oidcAuthConfigVolumeName)
	}
	return nil
}

// kubeadmAPIServerExtraVolume returns the named apiServer.extraVolumes entry from
// the kubeadm ClusterConfiguration, or an error if it is absent. The list shape
// is identical across v1beta3 and v1beta4.
func kubeadmAPIServerExtraVolume(clusterConfiguration, name string) (map[string]any, error) {
	configJSON, err := yaml.YAMLToJSON([]byte(clusterConfiguration))
	if err != nil {
		return nil, fmt.Errorf("converting kubeadm ClusterConfiguration to JSON: %w", err)
	}
	var config struct {
		APIServer struct {
			ExtraVolumes []map[string]any `json:"extraVolumes"`
		} `json:"apiServer"`
	}
	if err := json.Unmarshal(configJSON, &config); err != nil {
		return nil, fmt.Errorf("decoding kubeadm ClusterConfiguration: %w", err)
	}
	for _, volume := range config.APIServer.ExtraVolumes {
		if volumeName, _ := volume["name"].(string); volumeName == name {
			return volume, nil
		}
	}
	return nil, fmt.Errorf("kubeadm apiServer.extraVolumes has no %q volume", name)
}
