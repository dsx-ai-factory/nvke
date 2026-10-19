// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerdeploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/manifest"
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

	// capnicoNamespace is where CAPNICo's manifest installs its manager, and
	// where it reads its credentials.
	capnicoNamespace  = "capnico-system"
	capnicoDeployment = "controller-manager"

	// nicoCredentialsSecret is the Secret CAPNICo reads its NICo API
	// credentials from. The name, namespace and keys are CAPNICo's own.
	nicoCredentialsSecret = "nico-credentials"

	// CAPNICoArtifact is the stack artifact that carries the CAPNICo
	// image. It is the artifact deploy/nico/capnico/release.yaml binds to the
	// manifest, so a release repoints the same image for the deploy path and
	// the bootstrap.
	CAPNICoArtifact = "capnico-controller"

	// capnicoManifestFile is deploy/nico/capnico/capnico.yaml, copied by
	// `make -C cli capnico-manifest`. The copy lives here because go:embed
	// cannot reach outside this module, and embedding it is what lets
	// `nvkectl bootstrap site` run from the binary alone, with no repository
	// checkout. deploy/nico/capnico/ stays the source of truth; a test fails
	// when the two differ.
	capnicoManifestFile = "manifests/capnico.yaml"

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

// CAPNICoAccess is what the bootstrap CAPNICo needs from the operator: the
// NICo API it calls. Endpoint, OrgID and APIName come from the site spec's
// infra.nico, the API name defaulting to sitespec.DefaultNicoAPIName, and Token
// is read from the file NVKECTL_NICO_CREDENTIALS names. Its image is public, so no registry
// credential is needed.
type CAPNICoAccess struct {
	// Endpoint, OrgID, APIName and Token are the keys of the nico-credentials
	// Secret.
	Endpoint string
	OrgID    string
	APIName  string
	Token    string
}

// installCAPNICo installs CAPNICo the way the site controller's deploy path
// does: the published release manifest, embedded here from
// deploy/nico/capnico/capnico.yaml, with the NICo credentials applied beside it.
//
// It is not the Helm chart. The deploy path installs the manifest because a
// site has no provider checkout and the chart needs a registry login that path
// does not assume, and the manifest carries an edit the chart does not: the
// reboot and repair annotation prefixes the controller writes. Without it,
// every reboot and repair request is silently ignored.
//
// The manifest names no pull secret and this creates none: the image is public.
//
// The credentials are applied first, in a file with their namespace, because
// the manager reads them as soon as the manifest starts it.
func (e *Engine) installCAPNICo(ctx context.Context, kubeconfigPath string, cfg Config, timeout time.Duration) error {
	secrets, err := capnicoSecretsManifest(cfg.CAPNICo)
	if err != nil {
		return err
	}
	raw, err := manifestsFS.ReadFile(capnicoManifestFile)
	if err != nil {
		return fmt.Errorf("controllerdeploy: reading the embedded CAPNICo manifest: %w", err)
	}
	image := cfg.CAPNICoImage
	if image == "" {
		if image, err = pinnedCAPNICoImage(); err != nil {
			return err
		}
	}
	raw, err = capnicoManifest(raw, image)
	if err != nil {
		return err
	}

	// The Secrets hold credentials, so they go in a 0600 file rather than on a
	// command line, where a process listing or a failed command's output would
	// show them.
	secretsFile, cleanupSecrets, err := writeTempFile("nvkectl-capnico-secrets-*.json", secrets)
	if err != nil {
		return err
	}
	defer cleanupSecrets()
	manifestFile, cleanupManifest, err := writeTempFile("nvkectl-capnico-*.yaml", raw)
	if err != nil {
		return err
	}
	defer cleanupManifest()

	e.log.Info("installing CAPNICo", "namespace", capnicoNamespace, "image", image, "release", cfg.CAPNICoImage != "")
	for _, file := range []string{secretsFile, manifestFile} {
		// Server-side apply: the CRDs are large enough that a client-side apply
		// can exceed the last-applied-configuration annotation limit. It also
		// makes a re-run against a kept cluster replace the Secrets.
		if out, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
			"apply", "--server-side", "--force-conflicts", "-f", file); err != nil {
			return fmt.Errorf("controllerdeploy: applying CAPNICo: %w\n%s", err, out)
		}
	}
	if out, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"-n", capnicoNamespace, "rollout", "status", "deployment/"+capnicoDeployment,
		"--timeout", timeout.String()); err != nil {
		return fmt.Errorf("controllerdeploy: waiting for CAPNICo: %w\n%s", err, out)
	}
	return nil
}

// pinnedCAPNICoImage returns the repository and digest of the CAPNICo image the
// bootstrap manifest lists, as repository@digest: the form a release's artifact
// is applied in. The embedded manifest names the image by tag, which a registry
// can move, and the manager can read cluster Secrets, so the default install runs
// the digest instead. It fails when the manifest lists none rather than fall
// back to the tag.
func pinnedCAPNICoImage() (string, error) {
	img := manifest.Load().Components[manifest.ComponentCAPNICo].Images[manifest.ImageCAPNICo]
	if !strings.HasPrefix(img.Digest, "sha256:") {
		return "", fmt.Errorf("controllerdeploy: bootstrap-manifest.yaml lists no sha256 digest for the CAPNICo image %q", img.Ref)
	}
	repo := img.Ref
	if at := strings.IndexByte(repo, '@'); at >= 0 {
		repo = repo[:at]
	}
	if colon := strings.LastIndexByte(repo, ':'); colon > strings.LastIndexByte(repo, '/') {
		repo = repo[:colon]
	}
	return repo + "@" + img.Digest, nil
}

// capnicoImageLine matches an image: line of the manifest.
var capnicoImageLine = regexp.MustCompile(`(?m)^(\s*image:\s*)(\S+)[ \t]*$`)

// capnicoManifest returns the embedded manifest with its image replaced by
// image, as the deploy path's render does for a release (a kustomization that
// rewrites the manager image to the artifact's repository and digest). An empty
// image returns the manifest as embedded.
//
// The manifest names exactly one image, the manager's. If a bump adds a second,
// this refuses rather than guess which one the release's artifact replaces.
func capnicoManifest(raw []byte, image string) ([]byte, error) {
	if image == "" {
		return raw, nil
	}
	if n := len(capnicoImageLine.FindAll(raw, -1)); n != 1 {
		return nil, fmt.Errorf("controllerdeploy: the embedded CAPNICo manifest names %d images, want exactly 1 to replace with %s", n, image)
	}
	return capnicoImageLine.ReplaceAll(raw, []byte("${1}"+image)), nil
}

// capnicoSecretsManifest renders the Namespace and the nico-credentials Secret
// as one JSON document, which kubectl applies as it does YAML. The Secret
// carries the same keys the deploy path's 20-nico-credentials.yaml.tmpl
// renders.
//
// It is built with encoding/json rather than a template so a value holding a
// quote or a backslash is carried, not written altered.
func capnicoSecretsManifest(a CAPNICoAccess) ([]byte, error) {
	for _, f := range []struct{ name, value string }{
		{"Endpoint", a.Endpoint},
		{"OrgID", a.OrgID},
		{"APIName", a.APIName},
		{"Token", a.Token},
	} {
		if f.value == "" {
			return nil, fmt.Errorf("controllerdeploy: Config.CAPNICo.%s is required to install CAPNICo", f.name)
		}
	}
	secret := map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"name": nicoCredentialsSecret, "namespace": capnicoNamespace},
		"stringData": map[string]string{
			"endpoint": a.Endpoint, "orgID": a.OrgID, "token": a.Token, "apiName": a.APIName,
		},
	}
	out, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "List",
		"items": []any{
			map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": capnicoNamespace}},
			secret,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("controllerdeploy: encoding the CAPNICo secrets: %w", err)
	}
	return out, nil
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
