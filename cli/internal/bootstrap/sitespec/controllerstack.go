// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitespec

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ControllerStack is the input the controller stack install reads from the site
// spec. `nvkectl bootstrap site` does not read it. The site name, the trust
// domain, the control-plane VIP and the SPIRE CA come from the rest of the
// site spec and are not repeated here.
//
// Tokens and passwords are paths to files that hold them, one secret per
// file, relative paths resolved from the site spec, so the site spec itself
// holds no credential.
type ControllerStack struct {
	// Arch is the node CPU architecture, amd64 or arm64.
	Arch string `yaml:"arch"`
	// Nico is the NICo API CAPNICo calls.
	Nico StackNico `yaml:"nico"`
	// Tenant is the login rendered into every tenant node's cloud-init.
	Tenant StackTenant `yaml:"tenant"`
	// Images overrides the controller and profiles images, which default to
	// the release file's items.
	Images StackImages `yaml:"images,omitempty"`
	// Database names the file holding the Postgres password. The render
	// creates the file with a generated password when it does not exist.
	Database StackDatabase `yaml:"database"`
	// Mirror is a registry the external images are pulled from instead of
	// their upstream registries, as host[:port][/prefix], no scheme.
	Mirror string `yaml:"mirror,omitempty"`
	// NktpValuesFile is a values file for the nktp release, applied last.
	NktpValuesFile string `yaml:"nktpValuesFile,omitempty"`
}

// StackNico is the NICo API CAPNICo calls.
type StackNico struct {
	Endpoint  string `yaml:"endpoint"`
	OrgID     string `yaml:"orgID"`
	APIName   string `yaml:"apiName"`
	TokenFile string `yaml:"tokenFile"`
}

// StackTenant is the login rendered into every tenant node's cloud-init.
type StackTenant struct {
	User         string `yaml:"user"`
	PasswordFile string `yaml:"passwordFile"`
}

// StackImages overrides images that default to the release file.
type StackImages struct {
	Controller string `yaml:"controller,omitempty"`
	Profiles   string `yaml:"profiles,omitempty"`
}

// StackDatabase names the file holding the Postgres password.
type StackDatabase struct {
	PasswordFile string `yaml:"passwordFile"`
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
		{"controllerStack.nico.tokenFile", in.Nico.TokenFile},
		{"controllerStack.tenant.user", in.Tenant.User},
		{"controllerStack.tenant.passwordFile", in.Tenant.PasswordFile},
		{"controllerStack.images.controller", in.Images.Controller},
		{"controllerStack.images.profiles", in.Images.Profiles},
		{"controllerStack.database.passwordFile", in.Database.PasswordFile},
		{"controllerStack.mirror", in.Mirror},
		{"controllerStack.nktpValuesFile", in.NktpValuesFile},
	}
}

// requiredStackFields are the fields the controller stack install cannot do without.
var requiredStackFields = map[string]bool{
	"controllerStack.arch": true, "controllerStack.nico.endpoint": true, "controllerStack.nico.orgID": true, "controllerStack.nico.apiName": true,
	"controllerStack.nico.tokenFile": true, "controllerStack.tenant.user": true, "controllerStack.tenant.passwordFile": true,
	"controllerStack.database.passwordFile": true,
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
// outside the SiteSpec; ValidateControllerStackReferences checks the files it
// names.
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

// ValidateControllerStackReferences checks that every file the controllerStack section
// names can be read, relative paths resolved from the site spec's directory.
// Call it where the install runs; a machine that only writes the site spec
// may not hold the secrets.
func (inv *SiteSpec) ValidateControllerStackReferences() error {
	if inv.ControllerStack == nil {
		return errors.New("the site spec has no controllerStack: section")
	}
	v := &validator{}
	for _, field := range inv.ControllerStack.fileFields() {
		path, file := field[0], field[1]
		if file == "" || strings.HasPrefix(file, PlaceholderPrefix) {
			continue
		}
		if _, err := os.Stat(inv.ResolvePath(file)); err != nil {
			v.addf("%s %q cannot be read: %v", path, file, err)
		}
	}
	return v.err()
}

// fileFields pairs each controllerStack field that names a file that must
// exist with its value, in file order. database.passwordFile is not one: the
// render creates it when it is missing.
func (in *ControllerStack) fileFields() [][2]string {
	return [][2]string{
		{"controllerStack.nico.tokenFile", in.Nico.TokenFile},
		{"controllerStack.tenant.passwordFile", in.Tenant.PasswordFile},
		{"controllerStack.nktpValuesFile", in.NktpValuesFile},
	}
}

// ResolvePath returns path made absolute: as given when absolute, else
// joined onto the directory the site spec was loaded from, else onto the
// working directory. The install reads every file the controllerStack
// section names through this.
func (inv *SiteSpec) ResolvePath(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	if inv.dir != "" {
		return filepath.Join(inv.dir, path)
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
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
