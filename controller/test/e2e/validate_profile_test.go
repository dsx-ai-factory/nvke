// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/authtoken"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
)

// validateProfileEnv names the ValidationProfile file that describes the
// management cluster and tenant a validation run checks, for a layout that is
// not deploy/nico. validation-profile.schema.json is its schema.
//
// The suite ships no profiles. A management cluster's profile describes that
// management cluster, and lives with whatever else describes it.
const validateProfileEnv = "NVKE_VALIDATE_PROFILE"

const (
	validateProfileAPIVersion = "validate.nvke.dev/v1alpha1"
	validateProfileKind       = "ValidationProfile"
)

// Sections of a profile, which name the fields in messages.
const (
	sectionManagementCluster = "managementCluster"
	sectionTenant            = "tenant"
)

// validationProfile is the profile file. Every field is optional: an unset one
// keeps the default for deploy/nico.
type validationProfile struct {
	APIVersion        string                   `yaml:"apiVersion"`
	Kind              string                   `yaml:"kind"`
	Controller        profileController        `yaml:"controller"`
	Auth              profileAuth              `yaml:"auth"`
	ManagementCluster profileManagementCluster `yaml:"managementCluster"`
	Tenant            profileTenant            `yaml:"tenant"`
}

type profileController struct {
	Namespace       string `yaml:"namespace"`
	Service         string `yaml:"service"`
	Port            string `yaml:"port"`
	HealthTimeout   string `yaml:"healthTimeout"`
	ExpectedVersion string `yaml:"expectedVersion"`
}

type profileAuth struct {
	Audience       string `yaml:"audience"`
	Namespace      string `yaml:"namespace"`
	ServiceAccount string `yaml:"serviceAccount"`
}

// profileExpectations are what a management cluster and a tenant both declare.
type profileExpectations struct {
	Namespaces        []string           `yaml:"namespaces"`
	Components        []profileComponent `yaml:"components"`
	CertManager       profileCertManager `yaml:"certManager"`
	StorageClasses    []string           `yaml:"storageClasses"`
	PVCStorageClass   string             `yaml:"pvcStorageClass"`
	RequiredWorkloads []string           `yaml:"requiredWorkloads"`
	WorkloadVersions  map[string]string  `yaml:"workloadVersions"`
	APIServer         profileAPIServer   `yaml:"apiServer"`
	ReservedMemory    string             `yaml:"reservedMemory"`
}

// profileComponent is one optional component: its name alone, or a mapping
// with its name and the namespace it runs in where that is not its usual one.
type profileComponent struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// UnmarshalYAML takes either form. It refuses unknown keys itself: a
// decoder's KnownFields does not reach a custom unmarshaler.
func (c *profileComponent) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		c.Name = n.Value
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			if value.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: component field %s takes a string", value.Line, key.Value)
			}
			switch key.Value {
			case "name":
				c.Name = value.Value
			case "namespace":
				c.Namespace = value.Value
			default:
				return fmt.Errorf("line %d: field %s not found in a component; it has name and namespace", key.Line, key.Value)
			}
		}
		return nil
	case yaml.DocumentNode, yaml.SequenceNode, yaml.AliasNode:
	}
	return fmt.Errorf("line %d: a component is a name or {name, namespace}", n.Line)
}

type profileCertManager struct {
	Approval string `yaml:"approval"`
}

type profileAPIServer struct {
	Flags         map[string]string `yaml:"flags"`
	EtcdEncrypted bool              `yaml:"etcdEncrypted"`
}

type profileProbe struct {
	Image       string   `yaml:"image"`
	PullSecrets []string `yaml:"pullSecrets"`
}

type profileManagementCluster struct {
	profileExpectations `yaml:",inline"`
	Scope               string                 `yaml:"scope"`
	KubernetesVersion   string                 `yaml:"kubernetesVersion"`
	Probe               profileManagementProbe `yaml:"probe"`
}

type profileManagementProbe struct {
	Namespace    string `yaml:"namespace"`
	profileProbe `yaml:",inline"`
}

type profileTenant struct {
	profileExpectations `yaml:",inline"`
	Namespace           string       `yaml:"namespace"`
	Probe               profileProbe `yaml:"probe"`
	GPU                 profileGPU   `yaml:"gpu"`
}

type profileGPU struct {
	Nodes *int   `yaml:"nodes"`
	Image string `yaml:"image"`
}

// validateProfile is the profile this run loaded, zero without one. TestE2E
// sets it before the spec tree is built.
var validateProfile validationProfile

// validateProfileNotes say what the profile set, for the report.
var validateProfileNotes []string

// runNotes are the report lines that say what the profile set before any spec
// read its settings.
func runNotes() []string {
	return slices.Clone(validateProfileNotes)
}

// loadValidateProfile reads a profile, refusing an unknown field, a value of
// the wrong type, a second document, or another apiVersion or kind.
func loadValidateProfile(path string) (validationProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return validationProfile{}, fmt.Errorf("%s: %w", validateProfileEnv, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var p validationProfile
	if err := dec.Decode(&p); err != nil {
		if errors.Is(err, io.EOF) {
			return validationProfile{}, fmt.Errorf("%s=%s is empty", validateProfileEnv, path)
		}
		return validationProfile{}, fmt.Errorf("%s=%s: %w", validateProfileEnv, path, err)
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return validationProfile{}, fmt.Errorf("%s=%s holds more than one document", validateProfileEnv, path)
	}
	if p.APIVersion != validateProfileAPIVersion || p.Kind != validateProfileKind {
		return validationProfile{}, fmt.Errorf("%s=%s: want apiVersion %s and kind %s, got %q and %q",
			validateProfileEnv, path, validateProfileAPIVersion, validateProfileKind, p.APIVersion, p.Kind)
	}
	if reflect.DeepEqual(p, validationProfile{APIVersion: p.APIVersion, Kind: p.Kind}) {
		return validationProfile{}, fmt.Errorf("%s=%s sets nothing", validateProfileEnv, path)
	}
	if err := p.check(); err != nil {
		return validationProfile{}, fmt.Errorf("%s=%s: %w", validateProfileEnv, path, err)
	}
	return p, nil
}

// check parses every field the way the specs will, so a bad value fails the
// run before any spec rather than every spec that reads it.
func (p validationProfile) check() error {
	none := func(string) string { return "" }
	var errs []error
	if _, err := siteValidateFromProfile(p.ManagementCluster, none, ""); err != nil {
		errs = append(errs, err)
	}
	if _, err := tenantValidateFromProfile(p.Tenant, none, ""); err != nil {
		errs = append(errs, err)
	}
	if p.Controller.HealthTimeout != "" {
		if _, err := parseHealthTimeout(p.Controller.HealthTimeout); err != nil {
			errs = append(errs, fmt.Errorf("controller.healthTimeout: %w", err))
		}
	}
	return errors.Join(errs...)
}

// sharedInput is a profile field the rest of the suite reads from the
// environment, as the lanes set it.
type sharedInput struct {
	field, env, value string
}

func (p validationProfile) sharedInputs() []sharedInput {
	return []sharedInput{
		{"controller.namespace", controller.EnvNamespace, p.Controller.Namespace},
		{"controller.service", controller.EnvService, p.Controller.Service},
		{"controller.port", controller.EnvPort, p.Controller.Port},
		{"controller.healthTimeout", controllerHealthTimeoutEnv, p.Controller.HealthTimeout},
		{"auth.audience", authtoken.EnvAudience, p.Auth.Audience},
		{"auth.namespace", authtoken.EnvNamespace, p.Auth.Namespace},
		{"auth.serviceAccount", authtoken.EnvServiceAccount, p.Auth.ServiceAccount},
	}
}

// applyValidateProfile sets the environment the suite reads from the
// profile's shared inputs, refusing one the environment sets to something
// else. A version is a per-run input, so the environment's wins over the
// profile's. It returns what it set, for the report.
func applyValidateProfile(p validationProfile, getenv func(string) string, setenv func(key, value string)) ([]string, error) {
	var notes, conflicts []string
	for _, in := range p.sharedInputs() {
		if in.value == "" {
			continue
		}
		switch env := getenv(in.env); env {
		case "":
			setenv(in.env, in.value)
			notes = append(notes, fmt.Sprintf("%s = %s (profile)", in.field, in.value))
		case in.value:
			notes = append(notes, fmt.Sprintf("%s = %s (profile and %s)", in.field, in.value, in.env))
		default:
			conflicts = append(conflicts, fmt.Sprintf("%s is %q in the profile but %s is %q; set one", in.field, in.value, in.env, env))
		}
	}
	if len(conflicts) > 0 {
		return nil, errors.New(strings.Join(conflicts, "\n"))
	}
	version := resolveVersion("controller.expectedVersion", p.Controller.ExpectedVersion, expectedVersionEnv, getenv)
	if version.value != "" && version.source != expectedVersionEnv {
		setenv(expectedVersionEnv, version.value)
	}
	if version.value != "" {
		notes = append(notes, version.String())
	}
	return notes, nil
}

// resolvedVersion is a version a run checks against, and where it came from.
type resolvedVersion struct {
	field, value, source, overridden string
}

// resolveVersion picks a version from the environment if set there, else from
// the profile.
func resolveVersion(field, profile, env string, getenv func(string) string) resolvedVersion {
	if v := strings.TrimSpace(getenv(env)); v != "" {
		return resolvedVersion{field: field, value: v, source: env, overridden: strings.TrimSpace(profile)}
	}
	if v := strings.TrimSpace(profile); v != "" {
		return resolvedVersion{field: field, value: v, source: "profile"}
	}
	return resolvedVersion{field: field}
}

func (v resolvedVersion) String() string {
	if v.value == "" {
		return v.field + " is unset"
	}
	s := fmt.Sprintf("%s = %s (%s", v.field, v.value, v.source)
	if v.overridden != "" && v.overridden != v.value {
		s += fmt.Sprintf(", overriding the profile's %s", v.overridden)
	}
	return s + ")"
}

// retiredInputs are variables the validation image read before profiles were
// typed, and where each one's setting went.
var retiredInputs = map[string]string{
	"NVKE_SITE_VALIDATE":             siteValidateEnv,
	"NVKE_SITE_KUBERNETES_VERSION":   siteKubernetesVersionEnv,
	"NVKE_SITE_WORKLOAD_VERSIONS":    siteWorkloadVersionsEnv,
	"NVKE_SITE_NAMESPACES":           "managementCluster.namespaces",
	"NVKE_SITE_PROBE_NAMESPACE":      "managementCluster.probe.namespace",
	"NVKE_SITE_PROBE_IMAGE":          "managementCluster.probe.image",
	"NVKE_SITE_PROBE_PULL_SECRETS":   "managementCluster.probe.pullSecrets",
	"NVKE_SITE_COMPONENTS":           "managementCluster.components",
	"NVKE_SITE_STORAGE_CLASSES":      "managementCluster.storageClasses",
	"NVKE_SITE_PVC_STORAGE_CLASS":    "managementCluster.pvcStorageClass",
	"NVKE_SITE_REQUIRED_WORKLOADS":   "managementCluster.requiredWorkloads",
	"NVKE_SITE_APISERVER_FLAGS":      "managementCluster.apiServer.flags",
	"NVKE_SITE_ETCD_ENCRYPTED":       "managementCluster.apiServer.etcdEncrypted",
	"NVKE_SITE_RESERVED_MEMORY":      "managementCluster.reservedMemory",
	"NVKE_TENANT_NAMESPACE":          "tenant.namespace",
	"NVKE_TENANT_NAMESPACES":         "tenant.namespaces",
	"NVKE_TENANT_PROBE_IMAGE":        "tenant.probe.image",
	"NVKE_TENANT_PROBE_PULL_SECRETS": "tenant.probe.pullSecrets",
	"NVKE_TENANT_GPU_NODES":          "tenant.gpu.nodes",
	"NVKE_TENANT_GPU_IMAGE":          "tenant.gpu.image",
	"NVKE_TENANT_COMPONENTS":         "tenant.components",
	"NVKE_TENANT_STORAGE_CLASSES":    "tenant.storageClasses",
	"NVKE_TENANT_PVC_STORAGE_CLASS":  "tenant.pvcStorageClass",
	"NVKE_TENANT_REQUIRED_WORKLOADS": "tenant.requiredWorkloads",
	"NVKE_TENANT_APISERVER_FLAGS":    "tenant.apiServer.flags",
	"NVKE_TENANT_ETCD_ENCRYPTED":     "tenant.apiServer.etcdEncrypted",
	"NVKE_TENANT_RESERVED_MEMORY":    "tenant.reservedMemory",
}

// retiredInputProblems names each retired variable the environment sets, so
// a caller still passing one learns where it went instead of having it
// silently ignored.
func retiredInputProblems(lookupEnv func(string) (string, bool)) error {
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(retiredInputs)) {
		if _, ok := lookupEnv(name); !ok {
			continue
		}
		to := retiredInputs[name]
		if strings.HasPrefix(to, "NVKE_") {
			problems = append(problems, fmt.Sprintf("%s is no longer read: set %s instead", name, to))
		} else {
			problems = append(problems, fmt.Sprintf("%s is no longer read: set %s in the validation profile (%s)", name, to, validateProfileEnv))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "\n"))
}

// useValidateProfile refuses retired variables, then loads the profile
// NVKE_VALIDATE_PROFILE names, if any, and applies its shared inputs. It
// returns what to print about it.
func useValidateProfile(lookupEnv func(string) (string, bool), setenv func(key, value string)) ([]string, error) {
	if err := retiredInputProblems(lookupEnv); err != nil {
		return nil, err
	}
	getenv := func(key string) string { v, _ := lookupEnv(key); return v }
	path := strings.TrimSpace(getenv(validateProfileEnv))
	if path == "" {
		return nil, nil
	}
	p, err := loadValidateProfile(path)
	if err != nil {
		return nil, err
	}
	notes, err := applyValidateProfile(p, getenv, setenv)
	if err != nil {
		return nil, err
	}
	validateProfile = p
	return append([]string{fmt.Sprintf("%s=%s (%s %s)", validateProfileEnv, path, validateProfileAPIVersion, validateProfileKind)}, notes...), nil
}
