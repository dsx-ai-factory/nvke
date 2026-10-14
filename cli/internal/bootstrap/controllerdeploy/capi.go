// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerdeploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/manifest"
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

	// capnicoNamespace is where the CAPNICo chart installs its manager. The
	// chart creates it, so it must not exist beforehand.
	capnicoNamespace = "capnico-system"

	// capnicoReleaseName is the Helm release name for CAPNICo.
	capnicoReleaseName = "capi-provider-nico"

	// nicoCredentialsSecret is the Secret CAPNICo reads its NICo API
	// credentials from.
	nicoCredentialsSecret = "nico-credentials"

	// provisionedDefaultImage is the image reference config/manager/manager.yaml
	// carries. It is a placeholder: no image is published under it. The install
	// below rewrites it to Config.ProvisionedProviderImage, exactly as
	// cluster-api-provider-provisioned/hack/release-manifests.sh does for a
	// clusterctl provider release.
	provisionedDefaultImage = "docker.io/library/cluster-api-provider-provisioned:latest"
)

// installCAPI puts Cluster API and the provisioned infrastructure provider on
// the bootstrap cluster, so that the CAPI objects the NVKE controller writes for a tier-0
// cluster have controllers to act on them.
//
// This runs inside `nvkectl bootstrap site` rather than beside it because the
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
	// The two infrastructure providers are alternatives, not a pair: a site's
	// machines come from the hardcoded host list or from NICo, never both.
	if cfg.NicoInfrastructure {
		return e.installCAPNICo(ctx, kubeconfigPath, cfg, timeout)
	}
	if cfg.ProvisionedProviderImage == "" {
		e.log.Info("provisioned infrastructure provider image not configured; skipping provider install")
		return nil
	}
	return e.installProvisionedProvider(ctx, kubeconfigPath, cfg, timeout)
}

// installCAPNICo installs the CAPNICo Helm chart and then the NICo credentials
// it reads.
//
// The credentials come after the chart because the chart creates the
// capnico-system namespace unconditionally and fails with an ownership error if
// it already exists.
func (e *Engine) installCAPNICo(ctx context.Context, kubeconfigPath string, cfg Config, timeout time.Duration) error {
	if cfg.NicoCredentialsFile == "" {
		return errors.New("cwedeploy: Config.NicoCredentialsFile is required to install CAPNICo; " +
			"it names a file of KEY=VALUE lines that becomes the " + nicoCredentialsSecret + " Secret")
	}
	chart, err := manifestChart(manifest.ComponentCAPNICo)
	if err != nil {
		return err
	}
	// An OCI chart's reference is its repository and name joined; the overrides
	// replace each half, as Config.*ChartRef does for the other charts.
	chartRef := cfg.CAPNICoChartRef
	if chartRef == "" {
		chartRef = strings.TrimRight(chart.Repo, "/") + "/" + chart.Name
	}
	version := cfg.CAPNICoVersion
	if version == "" {
		version = chart.Version
	}

	e.log.Info("installing CAPNICo", "namespace", capnicoNamespace, "chart", chartRef, "version", version)
	if _, err := e.helm(ctx, kubeconfigPath,
		"upgrade", "--install", capnicoReleaseName, chartRef,
		"--version", version,
		"--namespace", capnicoNamespace, "--create-namespace",
		"--wait", "--timeout", timeout.String(),
	); err != nil {
		// The chart is an OCI artifact, which Helm 3 cannot install.
		return fmt.Errorf("cwedeploy: installing CAPNICo: %w\n"+
			"the CAPNICo chart needs helm v4 or later and network access to ghcr.io", err)
	}

	// A kept bootstrap cluster re-run finds the Secret from the earlier run, and
	// `kubectl create` refuses to replace it.
	if _, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"-n", capnicoNamespace, "get", "secret", nicoCredentialsSecret); err == nil {
		if out, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
			"-n", capnicoNamespace, "delete", "secret", nicoCredentialsSecret); err != nil {
			return fmt.Errorf("cwedeploy: replacing the %s Secret: %w\n%s", nicoCredentialsSecret, err, out)
		}
	}
	// --from-env-file passes the path, not the values, so no credential
	// appears in a process listing or in the output of a failed command.
	if out, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"-n", capnicoNamespace, "create", "secret", "generic", nicoCredentialsSecret,
		"--from-env-file="+cfg.NicoCredentialsFile); err != nil {
		return fmt.Errorf("cwedeploy: creating the %s Secret from %s: %w\n%s",
			nicoCredentialsSecret, cfg.NicoCredentialsFile, err, out)
	}
	return nil
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

	manifest, cleanup, err := writeTempFile("nvkectl-provisioned-provider-*.yaml", []byte(rendered))
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
