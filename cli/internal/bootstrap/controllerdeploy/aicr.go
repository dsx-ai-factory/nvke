// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerdeploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
)

// SeedService is the overlay of the AICR catalog the bootstrap cluster is built from.
const SeedService = "nvke-seed"

// AICRConfig says how the stack is deployed through AICR.
type AICRConfig struct {
	// Catalog is the rendered AICR catalog directory (nvkectl render catalog).
	Catalog string
	// Bin is the aicr binary.
	Bin string
}

// SeedValues returns the site-specific values for each seed component: what the catalog cannot know. The
// catalog holds every default; these are the settings that come from the site spec, the environment or the run.
// The infrastructure provider is one of them: the site spec's choice enables one provider's components in the
// seed and leaves the other's empty.
func (c Config) SeedValues(password string) (map[string]map[string]any, error) {
	clusterName := c.SPIFFEClusterName
	if clusterName == "" {
		clusterName = "nvkectl-bootstrap"
	}
	trustDomain := c.SPIFFETrustDomain
	if trustDomain == "" {
		trustDomain = clusterName + ".nke.local"
	}
	port := int(effectiveCWEPort(c))

	server := map[string]any{
		"adminIDs":          []any{"spiffe://" + trustDomain + "/nvke-service/" + cweReleaseName},
		"controllerManager": map[string]any{"className": spireControllerManagerClass},
		"service":           map[string]any{"port": 8081},
	}
	// No thumbprints means no TPM attestor, except with NICo: CAPNICo reports each machine's key as it allocates the
	// machine and the controller adds it to the ConfigMap the attestor reads, so the allowlist starts empty.
	if c.TPMAttestorEnabled() {
		tpm := map[string]any{"enabled": true}
		if sum := SpireTPMDirectChecksum(runtime.GOARCH); sum != "" {
			tpm["checksum"] = sum
		}
		if len(c.TPMEKFingerprints) > 0 {
			hashes := make([]any, len(c.TPMEKFingerprints))
			for i, h := range c.TPMEKFingerprints {
				hashes[i] = h
			}
			tpm["hashes"] = hashes
		} else {
			server["extraVolumes"] = []any{map[string]any{"name": tpmPubhashesVolume, "configMap": map[string]any{"name": tpmPubhashesConfigMap, "optional": true}}}
			server["extraVolumeMounts"] = []any{map[string]any{"name": tpmPubhashesVolume, "mountPath": tpmHashesMountPath, "readOnly": true}}
		}
		server["nodeAttestor"] = map[string]any{"tpmDirect": tpm}
	}
	if c.SPIFFEUpstreamCACertPath != "" && c.SPIFFEUpstreamCAKeyPath != "" {
		cert, err := os.ReadFile(c.SPIFFEUpstreamCACertPath)
		if err != nil {
			return nil, fmt.Errorf("reading the SPIRE upstream CA certificate: %w", err)
		}
		key, err := os.ReadFile(c.SPIFFEUpstreamCAKeyPath)
		if err != nil {
			return nil, fmt.Errorf("reading the SPIRE upstream CA key: %w", err)
		}
		// A self-signed root is its own bundle.
		ca := map[string]any{"certificate": string(cert), "key": string(key), "bundle": string(cert)}
		server["upstreamAuthority"] = map[string]any{"disk": map[string]any{"enabled": true, "secret": map[string]any{"data": ca}}}
	}

	// Where the controller tells nodes to find SPIRE. Hosts that already exist were given the address when they were
	// provisioned, so the in-cluster name is a placeholder for them; nodes CAPNICo creates have only this userdata.
	spireAddress, spirePort := spireServiceHost(), spireServerPort
	if c.NicoInfrastructure {
		spireAddress, spirePort = c.SPIREExternalAddress, strconv.Itoa(int(SPIREServerNodePort))
	}
	repo, tag, digest := splitImageRef(c.CWEImage)
	controller := map[string]any{
		"appDb": map[string]any{"postgresPassword": password},
		"image": map[string]any{"repository": repo, "tag": tag, "digest": digest, "pullPolicy": "IfNotPresent"},
		"spiffe": map[string]any{
			"enabled":     true,
			"trustDomain": trustDomain,
			"spire": map[string]any{
				"className":     spireControllerManagerClass,
				"serverAddress": spireAddress,
				"serverPort":    spirePort,
			},
		},
		"service":                     map[string]any{"port": port},
		"allowControlPlaneWithoutVIP": c.AllowControlPlaneWithoutVIP,
	}
	if c.NodeAgentPort != "" {
		controller["env"] = []any{map[string]any{"name": "NODE_AGENT_PORT", "value": c.NodeAgentPort}}
	}
	if c.ProfilesImage != "" {
		if err := ValidateProfilesImage(c.ProfilesImage); err != nil {
			return nil, err
		}
		r, t, d := splitImageRef(c.ProfilesImage)
		controller["profiles"] = map[string]any{"image": map[string]any{"enabled": true, "repository": r, "tag": t, "digest": d}}
	}
	if len(c.OIDCProviders) > 0 {
		body, err := yaml.Marshal(c.OIDCProviders)
		if err != nil {
			return nil, fmt.Errorf("encoding the OIDC providers: %w", err)
		}
		var providers []any
		if err := yaml.Unmarshal(body, &providers); err != nil {
			return nil, err
		}
		controller["oidc"] = map[string]any{"providers": providers}
	}

	provider := "provisioned"
	provisioned := map[string]any{"image": c.ProvisionedProviderImage}
	capnico := map[string]any{}
	seedConfig := map[string]any{"controllerPort": port}
	if c.NicoInfrastructure {
		provider = "nico"
		required := []struct{ name, value string }{
			{"endpoint", c.CAPNICo.Endpoint},
			{"org id", c.CAPNICo.OrgID},
			{"API name", c.CAPNICo.APIName},
			{"token", c.CAPNICo.Token},
			{"tenant user", c.DGXCTenant.User},
			{"tenant password", c.DGXCTenant.Password},
		}
		for _, f := range required {
			if f.value == "" {
				return nil, fmt.Errorf("controllerdeploy: the NICo %s is required", f.name)
			}
		}
		if c.SPIREExternalAddress == "" {
			return nil, errors.New("controllerdeploy: Config.SPIREExternalAddress is required with NicoInfrastructure: " +
				"nodes created by CAPNICo read the SPIRE server's address from their userdata, and the in-cluster name does not reach them")
		}
		capnico["nicoCredentials"] = map[string]any{"endpoint": c.CAPNICo.Endpoint, "orgID": c.CAPNICo.OrgID, "apiName": c.CAPNICo.APIName, "token": c.CAPNICo.Token}
		seedConfig["dgxcTenant"] = map[string]any{"user": c.DGXCTenant.User, "password": c.DGXCTenant.Password}
		controller["supportedInfraProviders"] = "nico"
		controller["secrets"] = map[string]any{"dgxcTenantCredSecretName": dgxcTenantSecret}
	}
	provisioned["provider"], capnico["provider"] = provider, provider

	sql := map[string]any{"sql": map[string]any{"password": password}}
	persistence := map[string]any{"default": sql, "visibility": sql}
	return map[string]map[string]any{
		"spire": {
			"global":       map[string]any{"spire": map[string]any{"clusterName": clusterName, "trustDomain": trustDomain}},
			"spire-server": server,
			"spire-agent":  map[string]any{"server": map[string]any{"port": 8081}},
		},
		"postgres":             {"auth": map[string]any{"password": password}},
		"temporal":             {"server": map[string]any{"config": map[string]any{"persistence": persistence}}},
		"nke-controller":       controller,
		"nvke-seed-config":     seedConfig,
		"provisioned-provider": provisioned,
		"capnico":              capnico,
	}, nil
}

// DeployWithAICR installs the controller stack from the AICR catalog instead of this package's own charts: it
// installs Cluster API and the provisioned provider (prerequisites outside AICR), then has aicr turn the seed
// overlay into a bundle, fills the site's values into it and runs the bundle's deploy.sh. It does not wait for
// the controller; call WaitReady.
func (e *Engine) DeployWithAICR(ctx context.Context, kubeconfigPath string, cfg Config, a AICRConfig) error {
	switch {
	case kubeconfigPath == "":
		return errors.New("controllerdeploy: kubeconfigPath is required")
	case a.Catalog == "" || a.Bin == "":
		return errors.New("controllerdeploy: the AICR catalog and the aicr binary are required")
	case cfg.CWEImage == "":
		return errors.New("controllerdeploy: Config.CWEImage is required")
	case cfg.ProvisionedProviderImage == "" && !cfg.NicoInfrastructure:
		return errors.New("controllerdeploy: Config.ProvisionedProviderImage is required; the CAPI objects the NVKE " +
			"controller writes for a site cluster go unreconciled without it")
	}
	password := cfg.PostgresPassword
	if password == "" {
		var err error
		if password, err = randomPassword(); err != nil {
			return fmt.Errorf("controllerdeploy: generating postgres password: %w", err)
		}
	}
	redact.RegisterSecret(password)
	values, err := cfg.SeedValues(password)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "nvkectl-aicr-*")
	if err != nil {
		return fmt.Errorf("controllerdeploy: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	if err := os.Chmod(work, 0o700); err != nil {
		return err
	}

	// Cluster API core is a prerequisite outside AICR: clusterctl init installs it with cert-manager, and the
	// providers in the seed need its CRDs.
	if err := e.installCAPICore(ctx, kubeconfigPath); err != nil {
		return err
	}

	recipe := filepath.Join(work, "recipe.yaml")
	e.log.Info("resolving the recipe", "service", SeedService, "catalog", a.Catalog)
	if out, err := e.Runner.Run(ctx, a.Bin, "recipe", "--service", SeedService, "--data", a.Catalog, "--output", recipe); err != nil {
		return fmt.Errorf("controllerdeploy: aicr recipe: %w\n%s", err, out)
	}
	bundle := filepath.Join(work, "bundle")
	args := []string{"bundle", "--recipe", recipe, "--data", a.Catalog, "--deployer", "helm", "--output", bundle}
	setArgs, err := setFileArgs(work, values)
	if err != nil {
		return err
	}
	e.log.Info("bundling the stack", "components", len(values))
	if out, err := e.Runner.Run(ctx, a.Bin, append(args, setArgs...)...); err != nil {
		return fmt.Errorf("controllerdeploy: aicr bundle: %w\n%s", err, out)
	}
	e.log.Info("deploying the bundle")
	script := "cd " + shellQuote(bundle) + " && chmod +x deploy.sh && ./deploy.sh"
	if out, err := e.Runner.Run(ctx, "env", "KUBECONFIG="+kubeconfigPath, "sh", "-c", script); err != nil {
		return fmt.Errorf("controllerdeploy: the bundle's deploy.sh: %w\n%s", err, out)
	}
	// The operator's own objects, applied once the controller's CRDs exist: the MachineImage and ClusterRelease
	// manifests the controller resolves a release from.
	if cfg.NicoInfrastructure {
		if cfg.ReleasesPath == "" {
			return errors.New("controllerdeploy: Config.ReleasesPath is required with NicoInfrastructure; the controller cannot resolve the control plane's release without the release manifests")
		}
		return e.applyReleases(ctx, kubeconfigPath, cfg.ReleasesPath)
	}
	return nil
}

// setFileArgs writes each top-level value of each component to a file under dir and returns the matching
// `aicr bundle --set-file component:key=file` arguments. Files, not --set, so a password or a key is never an
// argument another process can read.
func setFileArgs(dir string, values map[string]map[string]any) ([]string, error) {
	var args []string
	components := make([]string, 0, len(values))
	for c := range values {
		components = append(components, c)
	}
	sort.Strings(components)
	n := 0
	for _, c := range components {
		keys := make([]string, 0, len(values[c]))
		for k := range values[c] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			body, err := yaml.Marshal(values[c][k])
			if err != nil {
				return nil, fmt.Errorf("controllerdeploy: encoding %s %s: %w", c, k, err)
			}
			n++
			path := filepath.Join(dir, "value-"+strconv.Itoa(n)+".yaml")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				return nil, fmt.Errorf("controllerdeploy: %w", err)
			}
			args = append(args, "--set-file", c+":"+k+"="+path)
		}
	}
	return args, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
