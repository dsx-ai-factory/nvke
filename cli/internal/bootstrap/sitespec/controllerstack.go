// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitespec

import (
	"errors"
	"net/url"
	"strings"
	"unicode"

	"github.com/dsx-ai-factory/nvke/cli/internal/envalias"
)

// ControllerStack is the input the controller stack install reads from the site
// spec. `nvkectl bootstrap site` does not read it. The site name, the trust
// domain and the control-plane VIP come from the rest of the site spec and are
// not repeated here.
//
// The install reads its credentials from the environment (see
// StackCredentialsFromEnv), and the SPIRE CA from NVKECTL_SPIRE_CA_CERT_FILE and
// NVKECTL_SPIRE_CA_KEY_FILE.
type ControllerStack struct {
	// Arch is the node CPU architecture, amd64 or arm64.
	Arch string `yaml:"arch"`
	// Nico is the NICo API CAPNICo calls.
	Nico StackNico `yaml:"nico"`
	// Images overrides the controller and profiles images, which default to
	// the release file's items.
	Images StackImages `yaml:"images,omitempty"`
	// Mirror is a registry the external images are pulled from instead of
	// their upstream registries, as host[:port][/prefix], no scheme.
	Mirror string `yaml:"mirror,omitempty"`
}

// StackNico is the NICo API CAPNICo calls.
type StackNico struct {
	Endpoint string `yaml:"endpoint"`
	OrgID    string `yaml:"orgID"`
	APIName  string `yaml:"apiName"`
}

// StackImages overrides images that default to the release file.
type StackImages struct {
	Controller string `yaml:"controller,omitempty"`
	Profiles   string `yaml:"profiles,omitempty"`
}

// The environment variables the controller stack install reads its credentials from.
const (
	EnvNicoToken        = "NVKECTL_NICO_TOKEN" //nolint:gosec // G101: an env var name, not a credential
	EnvNodeUser         = "NVKECTL_NODE_USER"
	EnvNodePassword     = "NVKECTL_NODE_PASSWORD"     //nolint:gosec // G101: an env var name, not a credential
	EnvDatabasePassword = "NVKECTL_DATABASE_PASSWORD" //nolint:gosec // G101: an env var name, not a credential
)

// StackCredentials are the controller stack install's credentials. NodeUser
// and NodePassword are the login rendered into every tenant node's cloud-init.
type StackCredentials struct {
	NicoToken        string
	NodeUser         string
	NodePassword     string
	DatabasePassword string
}

// envs pairs each credential's environment variable with its value.
func (s *StackCredentials) envs() [][2]string {
	return [][2]string{
		{EnvNicoToken, s.NicoToken},
		{EnvNodeUser, s.NodeUser},
		{EnvNodePassword, s.NodePassword},
		{EnvDatabasePassword, s.DatabasePassword},
	}
}

// TemplateSafe reports whether value can go into the deploy path templates,
// which carry no escaping: no quote, backslash or control character.
func TemplateSafe(value string) bool {
	return !strings.ContainsAny(value, "\"\\") && !strings.ContainsFunc(value, unicode.IsControl)
}

// StackCredentialsFromEnv reads the controller stack install's credentials
// from the environment, trimmed, and names every one that is unset, one
// problem per line. The NICo token and the node login land in templates that
// carry no escaping, so a quote, a backslash or a control character in them is
// refused too.
func StackCredentialsFromEnv() (StackCredentials, error) {
	s := StackCredentials{
		NicoToken:        strings.TrimSpace(envalias.Get(EnvNicoToken)),
		NodeUser:         strings.TrimSpace(envalias.Get(EnvNodeUser)),
		NodePassword:     strings.TrimSpace(envalias.Get(EnvNodePassword)),
		DatabasePassword: strings.TrimSpace(envalias.Get(EnvDatabasePassword)),
	}
	v := &validator{}
	templated := map[string]bool{EnvNicoToken: true, EnvNodeUser: true, EnvNodePassword: true}
	for _, env := range s.envs() {
		switch {
		case env[1] == "":
			v.addf("%s is not set", env[0])
		case templated[env[0]] && !TemplateSafe(env[1]):
			v.addf("%s: the value holds a character the templates cannot carry (a quote, a backslash or a control character)", env[0])
		}
	}
	if len(v.errs) > 0 {
		return s, errors.New(strings.Join(v.errs, "\n"))
	}
	return s, nil
}

// PlaceholderPrefix marks a value `nvkectl generate sitespec` left for the
// operator to fill in.
const PlaceholderPrefix = "REPLACE_"

// StackArchitectures lists the architectures controllerStack.arch accepts.
var StackArchitectures = []string{"amd64", "arm64"}

// stackFields pairs each controllerStack field path with its value, in file order,
// for validation and placeholder reporting.
func (in *ControllerStack) stackFields() [][2]string {
	return [][2]string{
		{"controllerStack.arch", in.Arch},
		{"controllerStack.nico.endpoint", in.Nico.Endpoint},
		{"controllerStack.nico.orgID", in.Nico.OrgID},
		{"controllerStack.nico.apiName", in.Nico.APIName},
		{"controllerStack.images.controller", in.Images.Controller},
		{"controllerStack.images.profiles", in.Images.Profiles},
		{"controllerStack.mirror", in.Mirror},
	}
}

// requiredStackFields are the fields the controller stack install cannot do without.
var requiredStackFields = map[string]bool{
	"controllerStack.arch": true, "controllerStack.nico.endpoint": true, "controllerStack.nico.orgID": true, "controllerStack.nico.apiName": true,
}

// ControllerStackPlaceholders returns the controllerStack field paths whose
// value is still a REPLACE_* placeholder, in file order. It is empty when the
// section is absent; a nil ControllerStack tells the two apart.
func (inv *SiteSpec) ControllerStackPlaceholders() []string {
	if inv.ControllerStack == nil {
		return nil
	}
	var left []string
	for _, field := range inv.ControllerStack.stackFields() {
		if strings.HasPrefix(field[1], PlaceholderPrefix) {
			left = append(left, field[0])
		}
	}
	return left
}

// ValidateControllerStack checks the controllerStack section the controller
// stack install reads: present, no placeholder left, every required field
// set, the architecture and the NICo endpoint well-formed. It reads nothing
// outside the SiteSpec; StackCredentialsFromEnv checks the credentials.
func (inv *SiteSpec) ValidateControllerStack() error {
	if inv.ControllerStack == nil {
		return errors.New("the site spec has no controllerStack: section; nvkectl generate sitespec writes one to fill in")
	}
	v := &validator{}
	for _, path := range inv.ControllerStackPlaceholders() {
		v.addf("%s is still a REPLACE_* placeholder; fill it in", path)
	}
	inv.ControllerStack.appendValueChecks(v)
	return v.err()
}

// ValidateControllerStackValues checks the values that are filled in and
// says nothing about placeholders, so a caller can report the two apart.
func (inv *SiteSpec) ValidateControllerStackValues() error {
	if inv.ControllerStack == nil {
		return errors.New("the site spec has no controllerStack: section; nvkectl generate sitespec writes one to fill in")
	}
	v := &validator{}
	inv.ControllerStack.appendValueChecks(v)
	return v.err()
}

// appendValueChecks adds every problem with a filled-in value to v. A value
// that is still a placeholder is not a value and is skipped.
func (in *ControllerStack) appendValueChecks(v *validator) {
	for _, field := range in.stackFields() {
		path, value := field[0], field[1]
		if strings.HasPrefix(value, PlaceholderPrefix) {
			continue
		}
		if requiredStackFields[path] && value == "" {
			v.addf("%s is required", path)
		}
	}
	if in.Arch != "" && !strings.HasPrefix(in.Arch, PlaceholderPrefix) {
		known := false
		for _, arch := range StackArchitectures {
			known = known || arch == in.Arch
		}
		if !known {
			v.addf("controllerStack.arch %q must be one of %s", in.Arch, strings.Join(StackArchitectures, ", "))
		}
	}
	if ep := in.Nico.Endpoint; ep != "" && !strings.HasPrefix(ep, PlaceholderPrefix) {
		if u, err := url.Parse(ep); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			v.addf("controllerStack.nico.endpoint %q must be an http or https URL", ep)
		}
	}
	if in.Mirror != "" && strings.Contains(in.Mirror, "://") {
		v.addf("controllerStack.mirror %q is a host[:port][/prefix], not a URL", in.Mirror)
	}
}

// DefaultBootstrapClusterName is the cluster name `nvkectl bootstrap site`
// gives its bootstrap cluster unless NVKECTL_BOOTSTRAP_CLUSTER_NAME says
// otherwise. The default trust domain derives from it.
const DefaultBootstrapClusterName = "nvkectl-bootstrap"

// EffectiveTrustDomain returns the SPIFFE trust domain the bootstrap used:
// spire.trustDomain when set, else <bootstrapClusterName>.nke.local, the
// default `nvkectl bootstrap site` derives from its cluster name.
func (inv *SiteSpec) EffectiveTrustDomain(bootstrapClusterName string) string {
	if inv.Spire.TrustDomain != "" {
		return inv.Spire.TrustDomain
	}
	return bootstrapClusterName + ".nke.local"
}

// TrustDomain returns spire.trustDomain, or the trust domain a bootstrap with
// the default cluster name derives, and whether that default was used. The
// bootstrap and the controller stack install share it, so a site spec that
// sets nothing still joins both to one domain.
func (inv *SiteSpec) TrustDomain() (domain string, defaulted bool) {
	if inv.Spire.TrustDomain != "" {
		return inv.Spire.TrustDomain, false
	}
	return inv.EffectiveTrustDomain(DefaultBootstrapClusterName), true
}

// ControllerEndpoint returns the address the site controller is reached at:
// the control-plane VIP, or the only node's IP when the spec has one node
// and no VIP.
func (inv *SiteSpec) ControllerEndpoint() (string, error) {
	if inv.Cluster.ControlPlaneVIP != "" {
		return inv.Cluster.ControlPlaneVIP, nil
	}
	if len(inv.Cluster.Nodes) == 1 {
		return inv.Cluster.Nodes[0].IP, nil
	}
	return "", errors.New("the site spec has several nodes and no cluster.controlPlaneVIP")
}
