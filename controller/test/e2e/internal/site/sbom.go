// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/infra"
)

const (
	defaultSBOMTarget = "nke-sbom"

	// defaultStorageClassManifest is rancher local-path, what the reference
	// site used because its instance type has one NVMe with root on it.
	defaultStorageClassManifest = "https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.31/deploy/local-path-storage.yaml"
	defaultStorageClassName     = "local-path"

	spireAdminPath = "/nke-service/cloud-workflow-engine"

	// oidcPort is the OIDC discovery provider's NodePort.
	oidcPort = 30443

	nodeAgentPort         = "15005"
	spireServerAPIAddress = "spire-server.spire-server.svc.cluster.local:443"

	defaultNicoAPIName = "forge"
)

// The deploy path's releases. Each gets a values file named after it, passed
// with --values-for <release>=<file>.
const (
	releaseController = "controller"
	releaseSpire      = "spire"
	releasePostgreSQL = "postgresql"
	releaseTemporal   = "temporal"
)

func valuesFile(release string) string { return release + ".yaml" }

// tpmServerChecksum is the SHA-256 of the TPM attestor plugin binary the
// SPIRE server loads, per node architecture.
func tpmServerChecksum(arch string) (string, bool) {
	switch arch {
	case "x86_64":
		return "46d0caad8c25a027dd11c93e18b58a8bc6fbd9f1fe2e36fa2a0dd440986de4dc", true
	case "arm64":
		return "b69d0153c8241383f24da138bc85429a620d9f962a2f10abb314aa0e39046d16", true
	default:
		return "", false
	}
}

// envVar is an entry in a container's env list.
type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Image is a container image by repository and tag.
type Image struct {
	Repository string
	Tag        string
}

func (i Image) valid() bool { return i.Repository != "" && i.Tag != "" }

// Nico is where the site's CAPNICo reaches the NICo API.
type Nico struct {
	Endpoint string `json:"Endpoint"`
	OrgID    string `json:"OrgID"`
	Token    string `json:"Token"`
	APIName  string `json:"APIName"`
}

// Registry is the credential the site pulls private images with.
type Registry struct {
	Server   string `json:"Server"`
	Username string `json:"Username"`
	Token    string `json:"Token"`
}

// Tenant is the credential CWE creates tenant clusters' admin with.
type Tenant struct {
	User     string `json:"User"`
	Password string `json:"Password"`
}

// SBOM deploys the NKE stack with a deploy path from the SBOM: it does what
// the deploy path's README says a driver does around the install, and renders
// the values only a site knows.
//
// The deploy path and the scripts that apply it come from the staged assets,
// not from this suite, so the suite follows whatever deploy path the checkout
// it stages carries.
type SBOM struct {
	// Target is the deploy path under controller/deploy. Defaults to
	// nke-sbom.
	Target string

	Controller Image
	Profiles   Image

	Nico     Nico
	Registry Registry
	Tenant   Tenant

	// Arch is the node's architecture, as uname -m reports it. It selects the
	// TPM attestor checksum.
	Arch string

	// DatabasePassword is set on PostgreSQL and on both Temporal datastores.
	// Empty generates one per run.
	DatabasePassword string

	// StorageClassManifest installs the site's default StorageClass, named
	// StorageClassName. They default to rancher local-path; a site with real
	// storage names its own.
	StorageClassManifest string
	StorageClassName     string

	// ControllerEnv is appended to the controller's environment.
	ControllerEnv map[string]string
}

// Deploy implements Deployer.
func (d SBOM) Deploy(ctx context.Context, s infra.Session, t DeployTarget) error {
	d = d.withDefaults()
	if err := d.validate(); err != nil {
		return err
	}
	if d.DatabasePassword == "" {
		password, err := randomPassword()
		if err != nil {
			return err
		}
		d.DatabasePassword = password
	}

	sbomDir := path.Join(t.Assets, deployDir)
	overlay, err := s.Fetch(ctx, path.Join(sbomDir, d.Target, releaseSpire, "overlay.yaml"))
	if err != nil {
		return fmt.Errorf("read the deploy path's SPIRE overlay: %w", err)
	}
	files, err := d.render(t, overlay)
	if err != nil {
		return err
	}

	valuesDir := path.Join(workDir, "values")
	for name, data := range files {
		if err := s.Put(ctx, path.Join(valuesDir, name), data, 0o600); err != nil {
			return err
		}
	}
	defer func() {
		_, _ = s.Run(context.WithoutCancel(ctx), asRoot("rm -rf "+shellQuote(valuesDir)))
	}()

	for _, step := range d.commands(t, sbomDir, valuesDir) {
		if out, err := s.Run(ctx, asRoot(step.cmd)); err != nil {
			return fmt.Errorf("%s: %w\n%s", step.name, err, tail(out))
		}
	}
	return nil
}

func (d SBOM) withDefaults() SBOM {
	if d.Target == "" {
		d.Target = defaultSBOMTarget
	}
	if d.StorageClassManifest == "" {
		d.StorageClassManifest = defaultStorageClassManifest
		if d.StorageClassName == "" {
			d.StorageClassName = defaultStorageClassName
		}
	}
	if d.Nico.APIName == "" {
		d.Nico.APIName = defaultNicoAPIName
	}
	return d
}

func (d SBOM) validate() error {
	var problems []string
	if !d.Controller.valid() {
		problems = append(problems, "controller image needs a repository and a tag")
	}
	if !d.Profiles.valid() {
		problems = append(problems, "profiles image needs a repository and a tag")
	}
	if _, ok := tpmServerChecksum(d.Arch); !ok {
		problems = append(problems, fmt.Sprintf("no TPM attestor checksum for architecture %q", d.Arch))
	}
	if d.StorageClassName == "" {
		problems = append(problems, "a storage class manifest needs the name of the class it creates")
	}
	if d.Nico.Endpoint == "" || d.Nico.OrgID == "" || d.Nico.Token == "" {
		problems = append(problems, "NICo endpoint, org and token are required")
	}
	if len(problems) > 0 {
		return errors.New("sbom deployer: " + strings.Join(problems, "; "))
	}
	return nil
}

type step struct {
	name string
	cmd  string
}

// commands are the install and what the deploy path's README says surrounds
// it: untaint and clusterctl init and a default StorageClass before, the CAPI
// SPIFFE patch after.
func (d SBOM) commands(t DeployTarget, sbomDir, valuesDir string) []step {
	kc := shellQuote(t.Kubeconfig)
	env := "KUBECONFIG=" + kc + " "
	values := func(release string) string {
		return "--values-for " + shellQuote(release+"="+path.Join(valuesDir, valuesFile(release)))
	}
	defaultClass := `{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}`
	return []step{
		{"untaint the control plane", env + "kubectl taint nodes --all node-role.kubernetes.io/control-plane- || true"},
		{"clusterctl init", env + "clusterctl init --wait-providers"},
		{"default storage class", env + "kubectl apply -f " + shellQuote(d.StorageClassManifest) +
			" && " + env + "kubectl patch storageclass " + shellQuote(d.StorageClassName) + " -p " + shellQuote(defaultClass)},
		{"install " + d.Target, env + "bash " + shellQuote(path.Join(t.Assets, scriptsDir, "use-nke-sbom.sh")) + " install" +
			" --sbom " + shellQuote(sbomDir) +
			" --target " + shellQuote(d.Target) +
			" --config " + shellQuote(path.Join(valuesDir, "bootstrap-config.json")) +
			" " + values(releaseController) +
			" " + values(releaseSpire) +
			" " + values(releasePostgreSQL) +
			" " + values(releaseTemporal) +
			" --kubeconfig " + kc},
		{"CAPI SPIFFE exec auth", env + "bash " + shellQuote(path.Join(t.Assets, hackDir, "apply-capi-spiffe-exec-auth.sh"))},
	}
}

// render produces the per-site values files and the BootstrapConfig the
// deploy path's template items render against.
func (d SBOM) render(t DeployTarget, spireOverlay []byte) (map[string][]byte, error) {
	vip := t.Node.IP
	domains, err := oidcDomains(spireOverlay, vip)
	if err != nil {
		return nil, err
	}

	checksum, _ := tpmServerChecksum(d.Arch)

	// The overlay's env list is replaced, not merged, so what it sets is
	// repeated here.
	env := []envVar{
		{Name: "NODE_AGENT_PORT", Value: nodeAgentPort},
		{Name: "SPIRE_SERVER_API_ADDRESS", Value: spireServerAPIAddress},
	}
	for _, k := range sortedKeys(d.ControllerEnv) {
		env = append(env, envVar{Name: k, Value: d.ControllerEnv[k]})
	}
	// One value, three places, under two different keys. Temporal's datastores
	// take sql.password; the Bitnami chart takes auth.postgresPassword, and
	// only falls back to auth.password when auth.username is postgres -- so
	// auth.password renders a randomly generated superuser password the moment
	// the deploy path gives CWE and Temporal an account of their own.
	sqlPassword := map[string]any{"password": d.DatabasePassword}

	docs := map[string]any{
		valuesFile(releaseController): map[string]any{
			"image":       map[string]any{"repository": d.Controller.Repository, "tag": d.Controller.Tag},
			"appDb":       map[string]any{"postgresPassword": d.DatabasePassword},
			"nkeOIDCAuth": map[string]any{"discoveryURL": fmt.Sprintf("https://%s:%d/.well-known/openid-configuration", vip, oidcPort)},
			"spiffe":      map[string]any{"trustDomain": t.TrustDomain, "spire": map[string]any{"serverAddress": vip}},
			"nodeAgent":   map[string]any{"spireAttestationMethod": "tpm"},
			"profiles":    map[string]any{"image": map[string]any{"repository": d.Profiles.Repository, "tag": d.Profiles.Tag}},
			"env":         env,
		},
		valuesFile(releaseSpire): map[string]any{
			"global": map[string]any{"spire": map[string]any{"trustDomain": t.TrustDomain, "clusterName": t.SiteName}},
			"spire-server": map[string]any{
				"adminIDs":     []string{"spiffe://" + t.TrustDomain + spireAdminPath},
				"nodeAttestor": map[string]any{"tpmDirect": map[string]any{"checksum": checksum}},
			},
			"spiffe-oidc-discovery-provider": map[string]any{
				"config": map[string]any{
					"jwksUri":           fmt.Sprintf("https://%s:%d/keys", vip, oidcPort),
					"additionalDomains": domains,
				},
				"tls": map[string]any{"certManager": map[string]any{"certificate": map[string]any{"ipAddresses": []string{vip}}}},
			},
		},
		valuesFile(releasePostgreSQL): map[string]any{
			"auth": map[string]any{"postgresPassword": d.DatabasePassword},
		},
		valuesFile(releaseTemporal): map[string]any{
			"server": map[string]any{"config": map[string]any{"persistence": map[string]any{
				"default":    map[string]any{"sql": sqlPassword},
				"visibility": map[string]any{"sql": sqlPassword},
			}}},
		},
	}

	files := make(map[string][]byte, len(docs)+1)
	for name, doc := range docs {
		data, merr := yaml.Marshal(doc)
		if merr != nil {
			return nil, fmt.Errorf("render %s: %w", name, merr)
		}
		files[name] = data
	}
	config, err := json.MarshalIndent(map[string]any{"Nico": d.Nico, "Registry": d.Registry, "Tenant": d.Tenant}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render bootstrap config: %w", err)
	}
	files["bootstrap-config.json"] = config
	return files, nil
}

// oidcDomains is the overlay's additionalDomains plus the VIP. A values file
// replaces a list outright, so the list passed has to be complete, and it is
// read from the overlay rather than repeated here so the two cannot drift.
func oidcDomains(overlay []byte, vip string) ([]string, error) {
	var doc struct {
		Provider struct {
			Config struct {
				AdditionalDomains []string `json:"additionalDomains"`
			} `json:"config"`
		} `json:"spiffe-oidc-discovery-provider"`
	}
	if err := yaml.Unmarshal(overlay, &doc); err != nil {
		return nil, fmt.Errorf("parse SPIRE overlay: %w", err)
	}
	domains := doc.Provider.Config.AdditionalDomains
	if len(domains) == 0 {
		return nil, errors.New("the SPIRE overlay declares no OIDC additionalDomains; " +
			"passing only the VIP would drop the static hostnames")
	}
	for _, d := range domains {
		if d == vip {
			return domains, nil
		}
	}
	return append(domains, vip), nil
}

func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate database password: %w", err)
	}
	return hex.EncodeToString(b), nil
}
