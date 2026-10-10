// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerdeploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// capiVersion is injected by cli/Makefile from CAPI_VERSION in
// make/tool-versions.mk. One number covers CAPI core, both kubeadm providers,
// clusterctl, and the version CI installs.
var capiVersion = "dev" //nolint:gochecknoglobals // set by ldflags at build time

const (
	// capiCoreNamespace/capiCoreDeployment identify the core controller, used
	// only to decide whether CAPI is already installed. Same probe as
	// controller/Makefile.dev's capi-core-up target.
	capiCoreNamespace  = "capi-system"
	capiCoreDeployment = "capi-controller-manager"

	// The provisioned infrastructure provider's own kustomize output names
	// these; see cluster-api-provider-provisioned/config.
	provisionedNamespace  = "provisioned-system"
	provisionedDeployment = "provisioned-controller-manager"

	// provisionedDefaultImage is the image reference config/manager/manager.yaml
	// carries. It is a placeholder: no image is published under it. The install
	// below rewrites it to Config.ProvisionedProviderImage, exactly as
	// cluster-api-provider-provisioned/hack/release-manifests.sh does for a
	// clusterctl provider release.
	provisionedDefaultImage = "docker.io/library/cluster-api-provider-provisioned:latest"
)

// installCAPI puts Cluster API and the provisioned infrastructure provider on
// the bootstrap cluster, so that the CAPI objects the NKE controller writes for a tier-0
// cluster have controllers to act on them.
//
// This runs inside `nkectl bootstrap site` rather than beside it because the
// cluster it installs into is created and destroyed within that one command:
// there is no window in which anything else could install into it.
//
// The split between the two halves is deliberate. The upstream providers go in
// through `clusterctl init`, because their published component manifests carry
// ${...} placeholders that clusterctl resolves and a bare `kubectl apply` would
// leave as literal text. The provisioned provider goes in through `kubectl`,
// because it is this repository's own kustomize output: no placeholders, no
// provider repository to publish, and its image is loaded into the cluster
// rather than pulled.
func (e *Engine) installCAPI(ctx context.Context, kubeconfigPath string, cfg Config, timeout time.Duration) error {
	if err := e.installCAPICore(ctx, kubeconfigPath); err != nil {
		return err
	}
	if cfg.ProvisionedProviderImage == "" {
		e.log.Info("provisioned infrastructure provider image not configured; skipping provider install")
		return nil
	}
	return e.installProvisionedProvider(ctx, kubeconfigPath, cfg, timeout)
}

// installCAPICore runs `clusterctl init` for CAPI core and the kubeadm
// bootstrap and control-plane providers, skipping the work when they are
// already there. clusterctl brings cert-manager with it and refuses a second
// init, so the probe is what makes a re-run of `bootstrap site` against a kept
// cluster work.
func (e *Engine) installCAPICore(ctx context.Context, kubeconfigPath string) error {
	if _, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"-n", capiCoreNamespace, "get", "deployment", capiCoreDeployment); err == nil {
		e.log.Info("cluster api core already installed", "namespace", capiCoreNamespace)
		return nil
	}

	e.log.Info("installing cluster api core and the kubeadm providers", "version", capiVersion)
	if out, err := e.Runner.Run(ctx, "clusterctl", "init",
		"--kubeconfig", kubeconfigPath,
		"--core", "cluster-api:"+capiVersion,
		"--bootstrap", "kubeadm:"+capiVersion,
		"--control-plane", "kubeadm:"+capiVersion,
		"--wait-providers",
	); err != nil {
		return fmt.Errorf("controllerdeploy: clusterctl init: %w\n%s\n"+
			"clusterctl %s must be on PATH and the host must be able to reach "+
			"github.com and the cert-manager charts", err, out, capiVersion)
	}
	return nil
}

// installProvisionedProvider renders cluster-api-provider-provisioned's
// manifest embedded in this binary, points it at
// Config.ProvisionedProviderImage, and applies it.
//
// Server-side apply: the CRDs are large enough that a client-side apply can
// exceed the last-applied-configuration annotation limit.
func (e *Engine) installProvisionedProvider(
	ctx context.Context,
	kubeconfigPath string,
	cfg Config,
	timeout time.Duration,
) error {
	if cfg.ProvisionedProviderImage == "" {
		return errors.New("controllerdeploy: Config.ProvisionedProviderImage is required; it names the " +
			"cluster-api-provider-provisioned image to run (published by " +
			".github/workflows/image-provisioned-nvcr.yml, or build it with " +
			"`docker build -f cluster-api-provider-provisioned/Dockerfile .`)")
	}

	raw, err := manifestsFS.ReadFile(provisionedProviderFile)
	if err != nil {
		return fmt.Errorf("controllerdeploy: reading the embedded provisioned provider manifest: %w", err)
	}
	rendered := string(raw)
	if !strings.Contains(rendered, provisionedDefaultImage) {
		return fmt.Errorf("controllerdeploy: %s does not contain the expected placeholder image %q; "+
			"the provider's manager manifest changed and this install can no longer set its image",
			provisionedProviderFile, provisionedDefaultImage)
	}
	rendered = strings.ReplaceAll(rendered, provisionedDefaultImage, cfg.ProvisionedProviderImage)

	manifest, cleanup, err := writeTempFile("nkectl-provisioned-provider-*.yaml", []byte(rendered))
	if err != nil {
		return err
	}
	defer cleanup()

	e.log.Info("installing the provisioned infrastructure provider",
		"namespace", provisionedNamespace, "image", cfg.ProvisionedProviderImage)
	if out, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"apply", "--server-side", "--force-conflicts", "-f", manifest); err != nil {
		return fmt.Errorf("controllerdeploy: applying the provisioned provider: %w\n%s", err, out)
	}

	if out, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"-n", provisionedNamespace, "rollout", "status", "deployment/"+provisionedDeployment,
		"--timeout", timeout.String()); err != nil {
		return fmt.Errorf("controllerdeploy: waiting for the provisioned provider: %w\n%s", err, out)
	}
	return nil
}
