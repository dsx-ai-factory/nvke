// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/authtoken"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
)

const profileHeader = "apiVersion: validate.nvke.dev/v1alpha1\nkind: ValidationProfile\n"

func writeProfile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profile.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestUnit_ValidateProfileLoads(t *testing.T) {
	p, err := loadValidateProfile(writeProfile(t, profileHeader+`
controller:
  namespace: my-controller
  expectedVersion: v0.24.0
managementCluster:
  namespaces: [kube-system, cert-manager, my-controller]
  scope: namespaces
  kubernetesVersion: 1.30
  components: [argocd, {name: cnpg, namespace: databases}]
  apiServer:
    etcdEncrypted: true
    flags:
      audit-log-path: /var/log/audit.log
      tls-cipher-suites: a,b
  workloadVersions:
    kube-system/calico-node: v3.29.1
tenant:
  gpu: {nodes: 2}
`))
	require.NoError(t, err)
	require.Equal(t, "my-controller", p.Controller.Namespace)
	require.Equal(t, "v0.24.0", p.Controller.ExpectedVersion)
	require.Equal(t, []string{"kube-system", "cert-manager", "my-controller"}, p.ManagementCluster.Namespaces)
	require.Equal(t, "1.30", p.ManagementCluster.KubernetesVersion, "a version is taken as written, so 1.30 is not read as the number 1.3")
	require.Equal(t, []profileComponent{{Name: componentArgoCD}, {Name: componentCNPG, Namespace: "databases"}}, p.ManagementCluster.Components)
	require.Equal(t, profileAPIServer{EtcdEncrypted: true, Flags: map[string]string{"audit-log-path": "/var/log/audit.log", "tls-cipher-suites": "a,b"}}, p.ManagementCluster.APIServer)
	require.Equal(t, ptr.To(2), p.Tenant.GPU.Nodes)
}

func TestUnit_ValidateProfilePortTakesNumberOrName(t *testing.T) {
	for body, want := range map[string]string{"port: 8443": "8443", "port: http": "http"} {
		p, err := loadValidateProfile(writeProfile(t, profileHeader+"controller: {"+body+"}\n"))
		require.NoError(t, err, body)
		require.Equal(t, want, p.Controller.Port, body)
	}
}

func TestUnit_ValidateProfileREADMEExampleLoads(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)
	example := regexp.MustCompile("(?s)```yaml\n(# yaml-language-server: [^\n]*validation-profile\\.schema\\.json\n.*?)```").FindSubmatch(readme)
	require.NotNil(t, example, "the README's validation profile example")
	p, err := loadValidateProfile(writeProfile(t, string(example[1])))
	require.NoError(t, err, "the README's example must load as the image would load it")
	require.NotEmpty(t, p.ManagementCluster.Components)
}

func TestUnit_ValidateProfileRejects(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"no apiVersion":             {"kind: ValidationProfile\ncontroller: {namespace: a}\n", "want apiVersion validate.nvke.dev/v1alpha1"},
		"another apiVersion":        {"apiVersion: validate.nvke.dev/v1\nkind: ValidationProfile\ncontroller: {namespace: a}\n", "want apiVersion"},
		"another kind":              {"apiVersion: validate.nvke.dev/v1alpha1\nkind: Profile\ncontroller: {namespace: a}\n", "and kind ValidationProfile"},
		"a misspelled field":        {profileHeader + "managementCluster:\n  scop: namespaces\n", "field scop not found"},
		"a misspelled section":      {profileHeader + "managementClusters:\n  scope: namespaces\n", "field managementClusters not found"},
		"the flat format":           {profileHeader + "NVKE_SITE_NAMESPACES: [a]\n", "field NVKE_SITE_NAMESPACES not found"},
		"a wrong type":              {profileHeader + "managementCluster:\n  apiServer: {etcdEncrypted: sometimes}\n", "line 4"},
		"a count not a number":      {profileHeader + "tenant:\n  gpu: {nodes: four}\n", "cannot unmarshal"},
		"a map for a list":          {profileHeader + "managementCluster:\n  components: {argocd: argocd}\n", "cannot unmarshal"},
		"a component's unknown key": {profileHeader + "managementCluster:\n  components: [{name: cnpg, ns: databases}]\n", "field ns not found in a component"},
		"a component listed twice":  {profileHeader + "managementCluster:\n  components: [cnpg, {name: cnpg, namespace: databases}]\n", "managementCluster.components: cnpg is listed twice"},
		"not YAML":                  {profileHeader + "managementCluster: [\n", "did not find expected"},
		"a field set twice":         {profileHeader + "tenant: {namespace: a}\ntenant: {namespace: b}\n", "already defined"},
		"a second document":         {profileHeader + "tenant: {namespace: a}\n---\ntenant: {namespace: b}\n", "more than one document"},
		"empty":                     {"", "is empty"},
		"comments only":             {"# nothing set\n", "is empty"},
		"only the header":           {profileHeader, "sets nothing"},
		"an unknown scope":          {profileHeader + "managementCluster: {scope: namespace}\n", `managementCluster.scope: want cluster or namespaces, got "namespace"`},
		"an unknown component":      {profileHeader + "tenant:\n  components: [flux]\n", `tenant.components: unknown component "flux"`},
		"an empty namespaces":       {profileHeader + "tenant: {namespaces: []}\n", "tenant.namespaces names no namespace"},
		"a bad timeout":             {profileHeader + "controller: {healthTimeout: soon}\n", "controller.healthTimeout"},
		"a negative GPU count":      {profileHeader + "tenant:\n  gpu: {nodes: -1}\n", "tenant.gpu.nodes"},
		"a bad reserved memory":     {profileHeader + "managementCluster: {reservedMemory: two gigs}\n", "managementCluster.reservedMemory"},
		"every problem reported":    {profileHeader + "managementCluster: {scope: x}\ntenant: {namespaces: []}\n", "tenant.namespaces"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadValidateProfile(writeProfile(t, tc.body))
			require.ErrorContains(t, err, tc.want)
		})
	}

	_, err := loadValidateProfile(filepath.Join(t.TempDir(), "absent.yaml"))
	require.Error(t, err, "a profile named but missing fails the run rather than validating with defaults")
}

func TestUnit_ValidateProfileSharedInputs(t *testing.T) {
	p := validationProfile{
		Controller: profileController{Namespace: "nke", Service: "nke-api", HealthTimeout: "90s"},
		Auth:       profileAuth{Audience: "nke"},
	}
	env := map[string]string{controller.EnvService: "nke-api"}
	notes, err := applyValidateProfile(p, envMap(env), func(key, value string) { env[key] = value })
	require.NoError(t, err)
	require.Equal(t, "nke", env[controller.EnvNamespace])
	require.Equal(t, "90s", env[controllerHealthTimeoutEnv])
	require.Equal(t, "nke", env[authtoken.EnvAudience])
	require.Equal(t, []string{
		"controller.namespace = nke (profile)",
		"controller.service = nke-api (profile and NVKE_CONTROLLER_SERVICE)",
		"controller.healthTimeout = 90s (profile)",
		"auth.audience = nke (profile)",
	}, notes)

	env = map[string]string{controller.EnvNamespace: "nke-system", authtoken.EnvAudience: "other"}
	_, err = applyValidateProfile(p, envMap(env), func(key, value string) { env[key] = value })
	require.EqualError(t, err, `controller.namespace is "nke" in the profile but NVKE_CONTROLLER_NAMESPACE is "nke-system"; set one`+"\n"+
		`auth.audience is "nke" in the profile but NKE_E2E_AUDIENCE is "other"; set one`)
}

func TestUnit_ValidateProfileExpectedVersion(t *testing.T) {
	p := validationProfile{Controller: profileController{ExpectedVersion: "v0.23.1"}}
	env := map[string]string{}
	notes, err := applyValidateProfile(p, envMap(env), func(key, value string) { env[key] = value })
	require.NoError(t, err)
	require.Equal(t, "v0.23.1", env[expectedVersionEnv])
	require.Equal(t, []string{"controller.expectedVersion = v0.23.1 (profile)"}, notes)

	env = map[string]string{expectedVersionEnv: "v0.24.0"}
	notes, err = applyValidateProfile(p, envMap(env), func(key, value string) { env[key] = value })
	require.NoError(t, err, "a version is per run, so the environment's wins rather than conflicting")
	require.Equal(t, "v0.24.0", env[expectedVersionEnv])
	require.Equal(t, []string{"controller.expectedVersion = v0.24.0 (NVKE_CONTROLLER_EXPECTED_VERSION, overriding the profile's v0.23.1)"}, notes)
}

func TestUnit_ValidateProfileRetiredInputs(t *testing.T) {
	lookup := func(env map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) { v, ok := env[key]; return v, ok }
	}
	require.NoError(t, retiredInputProblems(lookup(map[string]string{siteValidateEnv: "true", tenantWorkloadVersionsEnv: "a/b=1"})))

	err := retiredInputProblems(lookup(map[string]string{"NVKE_SITE_COMPONENTS": "argocd", "NVKE_SITE_VALIDATE": "", "NVKE_TENANT_GPU_NODES": "2"}))
	require.EqualError(t, err, "NVKE_SITE_COMPONENTS is no longer read: set managementCluster.components in the validation profile (NVKE_VALIDATE_PROFILE)\n"+
		"NVKE_SITE_VALIDATE is no longer read: set NVKE_MGMT_CLUSTER_VALIDATE instead\n"+
		"NVKE_TENANT_GPU_NODES is no longer read: set tenant.gpu.nodes in the validation profile (NVKE_VALIDATE_PROFILE)",
		"set blank still fails: the caller still passes it")

	_, err = useValidateProfile(lookup(map[string]string{"NVKE_SITE_NAMESPACES": "a"}), func(string, string) {})
	require.ErrorContains(t, err, "managementCluster.namespaces", "retired inputs fail the run with or without a profile")
}

// Every field a retired variable points to exists in the profile.
func TestUnit_ValidateProfileRetiredInputsPointAtFields(t *testing.T) {
	fields := profileFields(reflect.TypeFor[validationProfile](), "")
	for name, to := range retiredInputs {
		if strings.HasPrefix(to, "NVKE_") {
			continue
		}
		require.Contains(t, fields, to, "%s points to %s", name, to)
	}
}

// The schema and the Go types name the same fields, and the schema refuses
// any other, as the loader does.
func TestUnit_ValidateProfileSchemaMatchesTypes(t *testing.T) {
	data, err := os.ReadFile("validation-profile.schema.json")
	require.NoError(t, err)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(data, &schema))
	defs, _ := schema["$defs"].(map[string]any)

	schemaFields := map[string]bool{}
	var walk func(node map[string]any, prefix string)
	walk = func(node map[string]any, prefix string) {
		if ref, ok := node["$ref"].(string); ok {
			node = defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		}
		props, ok := node["properties"].(map[string]any)
		if !ok {
			return
		}
		require.Equal(t, false, node["additionalProperties"], "%q refuses unknown fields", prefix)
		for name, child := range props {
			path := strings.TrimPrefix(prefix+"."+name, ".")
			schemaFields[path] = true
			walk(child.(map[string]any), path)
		}
	}
	walk(schema, "")

	goFields := profileFields(reflect.TypeFor[validationProfile](), "")
	require.Equal(t, slices.Sorted(maps.Keys(goFields)), slices.Sorted(maps.Keys(schemaFields)))

	components := defs["componentName"].(map[string]any)["enum"].([]any)
	names := make([]string, 0, len(components))
	for _, c := range components {
		names = append(names, c.(string))
	}
	require.ElementsMatch(t, slices.Collect(maps.Keys(siteComponentNamespaces)), names)
}

// profileFields lists the dotted path of every field under t, following
// inline structs and stopping at maps and lists.
func profileFields(t reflect.Type, prefix string) map[string]bool {
	fields := map[string]bool{}
	for f := range t.Fields() {
		tag := f.Tag.Get("yaml")
		if tag == ",inline" {
			maps.Copy(fields, profileFields(f.Type, prefix))
			continue
		}
		path := strings.TrimPrefix(prefix+"."+tag, ".")
		fields[path] = true
		if f.Type.Kind() == reflect.Struct {
			maps.Copy(fields, profileFields(f.Type, path))
		}
	}
	return fields
}

// Every variable the module reads is a shared input a profile sets, a
// per-run input, or one left out on purpose, so a new one cannot be added
// without deciding which.
func TestUnit_ValidateProfileCoversEveryInput(t *testing.T) {
	inputs := (validationProfile{}).sharedInputs()
	shared := make([]string, 0, len(inputs))
	for _, in := range inputs {
		shared = append(shared, in.env)
	}
	perRun := []string{
		validateProfileEnv,
		// Choose what a run does, and the image's ENV sets them.
		siteValidateEnv,
		tenantValidateEnv,
		expectedVersionRequireEnv,
		// Versions change with every release, so a run may name them
		// without editing the profile.
		expectedVersionEnv,
		siteKubernetesVersionEnv,
		siteWorkloadVersionsEnv,
		tenantWorkloadVersionsEnv,
		// Name one tenant, not tenants alike.
		tenantClusterEnv,
		tenantTokenSecretEnv,
		tenantKubernetesVersionEnv,
		// Turns authentication off, which a management cluster's file must not do for a run.
		"NKE_E2E_AUTH_DISABLE",
		// How long one run's token lives.
		"NKE_E2E_TOKEN_TTL",
		keepOnFailureEnv,
		// Read only by specs the validation image does not run: the lanes that
		// build, upgrade or change a management cluster.
		"NKE_E2E_BUNDLE_SERVER_URL",
		"NKE_E2E_DSX_IDENTITY_EXCHANGE",
		"NKE_E2E_NICO_VMS_CNI",
		"NKE_E2E_OIDC_ISSUER_HOST",
		"NKE_E2E_REQUIRE_AUTHN",
		"NKE_E2E_SPIRE_RELEASE",
		"NKE_E2E_UPGRADE_HOPS",
		"NVKE_CONTROLLER_RELEASES_DIR",
		"NVKE_E2E_MANAGEMENT_NAMESPACE",
		"NVKE_E2E_NODE_AGENT_BUNDLE_VOLUME",
		"NVKE_E2E_NODE_AGENT_UPGRADE",
		"NVKE_E2E_RBAC_CONFIGMAP",
	}

	input := regexp.MustCompile(`"((?:NVKE_SITE|NVKE_MGMT_CLUSTER|NVKE_TENANT|NVKE_CONTROLLER|NVKE_VALIDATE|NVKE_E2E|NKE_E2E)_[A-Z0-9_]*[A-Z0-9])"`)
	found := map[string]bool{}
	require.NoError(t, filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		// Unit tests name inputs that do not exist, to show they are
		// ignored, and the profile names the retired ones to refuse them.
		if strings.HasSuffix(path, "_unit_test.go") || path == "validate_profile_test.go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range input.FindAllSubmatch(data, -1) {
			found[string(match[1])] = true
		}
		return nil
	}))
	require.Contains(t, found, "NKE_E2E_AUDIENCE", "the scan reaches internal packages")

	for key := range found {
		require.NotContains(t, retiredInputs, key, "%s is retired, and the suite still reads it", key)
		require.True(t, slices.Contains(shared, key) || slices.Contains(perRun, key),
			"%s is an input; add it to the profile as a shared input, or to perRun with the reason", key)
	}
}
