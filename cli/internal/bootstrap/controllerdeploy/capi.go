// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerdeploy

import (
	"context"
	"fmt"
	"regexp"
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

	// CAPNICoArtifact is the NVKR release artifact that carries the CAPNICo
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

// capnicoImageLine matches an image: line of the manifest.
var capnicoImageLine = regexp.MustCompile(`(?m)^(\s*image:\s*)(\S+)[ \t]*$`)

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
