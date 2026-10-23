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
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra"
)

const (
	// defaultStorageClassManifest is rancher local-path, what the reference
	// site used because its instance type has one NVMe with root on it.
	defaultStorageClassManifest = "https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.31/deploy/local-path-storage.yaml"
	defaultStorageClassName     = "local-path"

	// oidcPort is the OIDC discovery provider's NodePort.
	oidcPort = 30443

	nodeAgentPort         = "15005"
	spireServerAPIAddress = "spire-server.spire-server.svc.cluster.local:443"

	// valuesCleanupTimeout bounds removing the rendered values files. Short on
	// purpose: it is one rm over a session that was answering a moment ago.
	valuesCleanupTimeout = 2 * time.Minute
)

// Where the deploy path lives on the node. assetsArchive is transient: it is
// uploaded, untarred into assetsDir, and deleted in the same command.
const (
	assetsArchive = workDir + "/assets.tar.gz"
	assetsDir     = workDir + "/assets"
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

// Ref is the image as repository:tag.
func (i Image) Ref() string { return i.Repository + ":" + i.Tag }

// CAPIProviderConfig is how the Cluster API infrastructure provider the stack
// installs reaches the infrastructure it creates tenant machines on. That
// provider needs an endpoint and a credential of its own, because the site
// controller creates machines long after this run is over.
//
// Named for CAPI because three things here are called a provider: infra.Provider
// creates the site node from the test process, environment.Environment.Provider
// names the dialect a file is written in, and this one is the CAPI provider
// running in the cluster the run just built.
//
// An interface because which provider is installed follows which one built the
// site, and nothing between here and the deploy should have to name it. The
// value renders into bootstrap-config.json under its own key, and the deploy
// tree's credentials template reaches for that key, so adding a provider is a
// type here and a template there with nothing in between.
type CAPIProviderConfig interface {
	// ConfigKey is the key this renders under in bootstrap-config.json.
	ConfigKey() string

	// Validate reports what the provider cannot be configured without. Called
	// before the deploy stages anything, so a missing credential fails the
	// run rather than the controller that reads the Secret.
	Validate() error

	// Credentials is every secret this configuration puts on the node, for
	// redacting the diagnostics archive. On the interface rather than read off
	// a concrete type, so a provider added later cannot be forgotten here:
	// whoever writes the type is the one who knows which of its fields is a
	// credential, and the archive is uploaded whatever the run did.
	Credentials() []string
}

// Nico is where the site's CAPNICo reaches the NICo API.
type Nico struct {
	Endpoint string `json:"Endpoint"`
	OrgID    string `json:"OrgID"`
	Token    string `json:"Token"`
	APIName  string `json:"APIName"`
}

// NewNico is the CAPNICo configuration.
//
// The API name is stated rather than defaulted. It is the path segment the API
// is served under, and it is NOT the same for every site: hosted Forge serves
// "forge" and a DSX site serves "nico". It was defaulted while Forge was the
// only substrate, and a default that is right for one of two is worse than no
// default, because the wrong one fails as a 404 against a URL nothing printed.
func NewNico(endpoint, orgID, token, apiName string) Nico {
	return Nico{Endpoint: endpoint, OrgID: orgID, Token: token, APIName: apiName}
}

// Credentials implements CAPIProviderConfig. The token is the whole of it:
// the endpoint, org and API name are infrastructure, and the token is what
// reaches the node, both in bootstrap-config.json and in the Secret the
// credentials template renders from it.
func (n Nico) Credentials() []string {
	if n.Token == "" {
		return nil
	}
	return []string{n.Token}
}

// ConfigKey implements CAPIProviderConfig. "Nico" is what
// 20-nico-credentials.yaml.tmpl reaches for.
func (n Nico) ConfigKey() string { return "Nico" }

// Validate implements CAPIProviderConfig.
func (n Nico) Validate() error {
	if n.Endpoint == "" || n.OrgID == "" || n.Token == "" {
		return errors.New("NICo endpoint, org and token are required")
	}
	return nil
}

// Tenant is the credential the controller creates tenant clusters' admin with.
type Tenant struct {
	User     string `json:"User"`
	Password string `json:"Password"`
}

// Stack deploys the NVKE stack with a deploy path from the Stack: it does what
// the deploy path's README says a driver does around the install, and renders
// the values only a site knows.
//
// The deploy path and the scripts that apply it come from the staged assets,
// not from this suite, so the suite follows whatever deploy path the checkout
// it stages carries.
type Stack struct {
	// Assets are the deploy path and the scripts that apply it, staged to
	// the node with their layout intact, which the deploy path depends on: a
	// release reaches its chart by relative path. ExportDeployAssets builds
	// them.
	//
	// Nothing in the bootstrap reads them: nvkectl pulls its charts from their
	// repositories and embeds its values. These are the deploy's own
	// installer, tree and scripts.
	Assets DeployAssets

	Controller Image
	Profiles   Image

	// Provider is how the installed Cluster API provider reaches its
	// infrastructure. Required: a stack with no way to create machines
	// deploys and then cannot do its job.
	Provider CAPIProviderConfig
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

// DeployAssets are a ref's deploy path and the scripts that apply it,
// exported onto the runner.
type DeployAssets struct {
	// Dir is the export. Layout.Export, relative to it, is what is staged.
	Dir string
	// Layout is where Dir keeps the executor and the deploy path, which
	// depends on the ref it came from. Zero is CurrentLayout().
	Layout DeployLayout
}

// ExportDeployAssets exports ref's deploy assets from repo and checks that
// the controller name they install is the one the suite looks for. cleanup
// removes the export; the caller owns when it runs.
//
// Call it before any machine is taken, not at deploy time, so a release that
// predates the supported layout or disagrees about the name fails here on
// the runner rather than after a node has been provisioned.
func ExportDeployAssets(ctx context.Context, repo, ref string) (DeployAssets, func(), error) {
	layout, err := ResolveLayout(ctx, repo, ref)
	if err != nil {
		return DeployAssets{}, nil, err
	}
	dir, err := ExportRef(ctx, repo, ref, layout.Export)
	if err != nil {
		return DeployAssets{}, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := CheckControllerName(dir, layout.DeployPath); err != nil {
		cleanup()
		return DeployAssets{}, nil, err
	}
	return DeployAssets{Dir: dir, Layout: layout}, cleanup, nil
}

// stageAssets copies the deploy path onto the node, with the layout the
// scripts in it expect.
func (d Stack) stageAssets(ctx context.Context, s infra.Session) error {
	archive, err := archiveAssets(d.Assets.Dir, d.Assets.Layout.Export)
	if err != nil {
		return err
	}
	if err := s.Put(ctx, assetsArchive, archive, 0o600); err != nil {
		return err
	}
	cmd := fmt.Sprintf("rm -rf %[1]s && mkdir -p %[1]s && tar -C %[1]s -xzf %[2]s && rm -f %[2]s",
		shellQuote(assetsDir), shellQuote(assetsArchive))
	if out, err := s.Run(ctx, asRoot(cmd)); err != nil {
		return fmt.Errorf("stage the deploy path: %w\n%s", err, tail(out))
	}
	return nil
}

// sortedKeys renders a map in a stable order, so the same input produces the
// same command twice.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Deploy implements Deployer.
func (d Stack) Deploy(ctx context.Context, s infra.Session, t DeployTarget) error {
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

	if err := d.stageAssets(ctx, s); err != nil {
		return err
	}

	overlay, err := s.Fetch(ctx, path.Join(assetsDir, d.Assets.Layout.DeployPath, releaseSpire, "overlay.yaml"))
	if err != nil {
		return fmt.Errorf("read the deploy path's SPIRE overlay: %w", err)
	}
	files, err := d.render(t, overlay)
	if err != nil {
		return err
	}

	valuesDir := path.Join(workDir, "values")
	// Registered before the first Put rather than after the loop. Every one of
	// these files carries the bootstrap tokens and the database password, so a
	// Put that fails halfway has already written one to the node, and the map
	// is walked in random order -- which would make the exposure intermittent
	// and the bug hard to see.
	defer func() {
		// Detached, because the failure that brings us here is often a
		// cancellation and that is when the files matter most. Bounded,
		// because a wedged session must not hold the spec open.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), valuesCleanupTimeout)
		defer cancel()
		_, _ = s.Run(cctx, asRoot("rm -rf "+shellQuote(valuesDir)))
	}()
	for name, data := range files {
		if err := s.Put(ctx, path.Join(valuesDir, name), data, 0o600); err != nil {
			return err
		}
	}

	for _, step := range d.commands(t, valuesDir) {
		if out, err := s.Run(ctx, asRoot(step.cmd)); err != nil {
			return fmt.Errorf("%s: %w\n%s", step.name, err, tail(out))
		}
	}
	return nil
}

func (d Stack) withDefaults() Stack {
	if d.Assets.Layout.Executor == "" {
		d.Assets.Layout = CurrentLayout()
	}
	if d.StorageClassManifest == "" {
		d.StorageClassManifest = defaultStorageClassManifest
		if d.StorageClassName == "" {
			d.StorageClassName = defaultStorageClassName
		}
	}
	return d
}

// CheckControllerName fails when the deploy path installs the controller
// under a name the suite is not configured to look for.
//
// These are two owners of one string: the deploy path, exported from the
// controller version under test, and NVKE_CONTROLLER_NAMESPACE and
// NVKE_CONTROLLER_SERVICE, which default to nvke-controller. Nothing makes the
// two move together.
//
// Getting it wrong is expensive rather than obvious: the readiness probe
// opens a port-forward onto a Service that does not exist, the forwarder
// retries the NotFound rather than reporting it, and the run spends ten
// minutes reporting that the controller will not come up while it is healthy
// in the namespace beside it. So this runs on the runner, before any machine
// is taken.
func CheckControllerName(assets, deployPath string) error {
	deployed, err := controllerNameFromDeployPath(assets, deployPath)
	if err != nil {
		return err
	}
	if deployed == controller.Namespace() && deployed == controller.Service() {
		return nil
	}
	return fmt.Errorf(
		"the deploy path installs the controller as %q but the suite looks for %s/%s: set %s and %s to %q, or qualify a controller whose deploy path matches",
		deployed, controller.Namespace(), controller.Service(), controller.EnvNamespace, controller.EnvService, deployed,
	)
}

// controllerNameFromDeployPath is the name a set of exported assets installs
// the controller under, read from the HelmRelease rather than assumed.
func controllerNameFromDeployPath(assets, deployPath string) (string, error) {
	releasePath := ControllerReleasePath(deployPath)
	raw, err := os.ReadFile(filepath.Join(assets, releasePath))
	if err != nil {
		return "", fmt.Errorf("reading the deploy path's controller release: %w", err)
	}

	var release struct {
		Release struct {
			Namespace string `json:"namespace"`
		} `json:"release"`
	}
	if err := yaml.Unmarshal(raw, &release); err != nil {
		return "", fmt.Errorf("parsing %s: %w", releasePath, err)
	}
	if release.Release.Namespace == "" {
		return "", fmt.Errorf("%s names no release namespace, so the name the controller is installed under cannot be read", releasePath)
	}
	return release.Release.Namespace, nil
}

// controllerName is the namespace, Service and ServiceAccount the controller
// is installed under, and so the last segment of its SPIFFE ID.
//
// Read from the controller package rather than written out here, because this
// file does not get to choose it. The deploy path does, and
// CheckControllerName fails the run before any spec when the two disagree.
func controllerName() string { return controller.Service() }

// spireAdminPath is the controller's SPIFFE ID below the trust domain. A name that does
// not match what the deploy path installed leaves the controller denied every
// authorization check, which reads as an RBAC fault rather than a naming one.
func spireAdminPath() string { return "/nvke-service/" + controllerName() }

func (d Stack) validate() error {
	var problems []string
	if !d.Controller.valid() {
		problems = append(problems, "controller image needs a repository and a tag")
	}
	if !d.Profiles.valid() {
		problems = append(problems, "profiles image needs a repository and a tag")
	}
	if d.Assets.Dir == "" {
		problems = append(problems, "no assets")
	} else if _, err := os.Stat(d.Assets.Dir); err != nil {
		problems = append(problems, fmt.Sprintf("assets: %v", err))
	}
	for _, rel := range d.Assets.Layout.Export {
		if filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), "..") {
			problems = append(problems, fmt.Sprintf("asset path %q is not inside the checkout", rel))
		}
	}
	if _, ok := tpmServerChecksum(d.Arch); !ok {
		problems = append(problems, fmt.Sprintf("no TPM attestor checksum for architecture %q", d.Arch))
	}
	if d.StorageClassName == "" {
		problems = append(problems, "a storage class manifest needs the name of the class it creates")
	}
	if d.Provider == nil {
		problems = append(problems, "no provider configuration, so the stack could not create a machine")
	} else if err := d.Provider.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return errors.New("stack deployer: " + strings.Join(problems, "; "))
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
func (d Stack) commands(t DeployTarget, valuesDir string) []step {
	kc := shellQuote(t.Cluster.NodeKubeconfig)
	env := "KUBECONFIG=" + kc + " "
	values := func(release string) string {
		return "--values-for " + shellQuote(release+"="+path.Join(valuesDir, valuesFile(release)))
	}
	defaultClass := `{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}`
	// The flag that names the deploy path is the one thing the two executors
	// spell differently.
	target := ""
	for i, arg := range d.Assets.Layout.TargetArgs(assetsDir) {
		if i > 0 {
			target += " "
		}
		target += shellQuote(arg)
	}
	return []step{
		{"untaint the control plane", env + "kubectl taint nodes --all node-role.kubernetes.io/control-plane- || true"},
		{"clusterctl init", env + "clusterctl init --wait-providers"},
		{"default storage class", env + "kubectl apply -f " + shellQuote(d.StorageClassManifest) +
			" && " + env + "kubectl patch storageclass " + shellQuote(d.StorageClassName) + " -p " + shellQuote(defaultClass)},
		{"install " + d.Assets.Layout.DeployPath, env + "bash " + shellQuote(path.Join(assetsDir, d.Assets.Layout.Executor)) + " install" +
			" " + target +
			" --config " + shellQuote(path.Join(valuesDir, "bootstrap-config.json")) +
			" " + values(releaseController) +
			" " + values(releaseSpire) +
			" " + values(releasePostgreSQL) +
			" " + values(releaseTemporal) +
			" --kubeconfig " + kc},
		{"CAPI SPIFFE exec auth", env + "bash " + shellQuote(path.Join(assetsDir, hackDir, "apply-capi-spiffe-exec-auth.sh"))},
	}
}

// render produces the per-site values files and the BootstrapConfig the
// deploy path's template items render against.
func (d Stack) render(t DeployTarget, spireOverlay []byte) (map[string][]byte, error) {
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
	// the deploy path gives the controller and Temporal an account of their own.
	sqlPassword := map[string]any{"password": d.DatabasePassword}

	docs := map[string]any{
		valuesFile(releaseController): map[string]any{
			"image":        map[string]any{"repository": d.Controller.Repository, "tag": d.Controller.Tag},
			"appDb":        map[string]any{"postgresPassword": d.DatabasePassword},
			"nvkeOIDCAuth": map[string]any{"discoveryURL": fmt.Sprintf("https://%s:%d/.well-known/openid-configuration", vip, oidcPort)},
			"spiffe":       map[string]any{"trustDomain": t.TrustDomain, "spire": map[string]any{"serverAddress": vip}},
			"nodeAgent":    map[string]any{"spireAttestationMethod": "tpm"},
			"profiles":     map[string]any{"image": map[string]any{"repository": d.Profiles.Repository, "tag": d.Profiles.Tag}},
			"env":          env,
		},
		valuesFile(releaseSpire): map[string]any{
			"global": map[string]any{"spire": map[string]any{"trustDomain": t.TrustDomain, "clusterName": t.SiteName}},
			"spire-server": map[string]any{
				"adminIDs":     []string{"spiffe://" + t.TrustDomain + spireAdminPath()},
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
	// ClusterName is the name NKTP labels the site's telemetry with. NVKE's
	// kubeadm configuration names every cluster "kubernetes".
	config, err := json.MarshalIndent(map[string]any{
		d.Provider.ConfigKey(): d.Provider,
		"Tenant":               d.Tenant,
		"ClusterName":          t.SiteName,
		// Controller releases that still carry nvke-base-config's
		// ghcr-imagepull template render a pull Secret from .Registry. The
		// server is not a registry any image uses, so no pull sends these
		// values and the public GHCR image is pulled anonymously.
		"Registry": map[string]string{"Server": "placeholder.invalid", "Username": "unused", "Token": "unused"},
	}, "", "  ")
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
