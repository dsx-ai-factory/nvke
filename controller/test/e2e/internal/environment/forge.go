// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// ForgeProvider is the provider name environments/forge.yaml declares.
	ForgeProvider = "forge"

	// The capability keys a machine can answer. A run asks for more than
	// these -- kube names a software version, not a property of any machine --
	// so matching considers these and ignores the rest, which is what stops a
	// Kubernetes version from excluding every instance type.
	capabilityMachineType = "machineType"
	capabilityArch        = "arch"
	capabilityGPU         = "gpu"
)

// Forge is Forge's dialect of an infrastructure declaration. None of it is
// generic: an org, a site and a VPC are Forge's own vocabulary.
type Forge struct {
	Org   ForgeOrg             `json:"org"`
	Sites map[string]ForgeSite `json:"sites"`
}

// ForgeOrg identifies the Forge tenancy the run creates things in.
type ForgeOrg struct {
	ID string `json:"id"`
}

// ForgeSite is one location, carrying the network identifiers a cluster is
// created against and the machines the site can offer.
type ForgeSite struct {
	// Key is the name this site is filed under, populated on lookup so callers
	// can report which site answered without threading the name alongside.
	Key string `json:"-"`

	ID            string `json:"id"`
	VpcID         string `json:"vpcID"`
	SubnetID      string `json:"subnetID"`
	SSHKeyGroupID string `json:"sshKeyGroupID"`

	// No control-plane VIP. CWE treats an empty one as "do not configure
	// kube-vip", which is what create-forge-nico-cluster.sh relies on: it
	// sends no VIP at all. The simulator backends need one because their BGP
	// speakers advertise it, and a real site does not work that way.

	InstanceTypes []ForgeInstanceType `json:"instanceTypes"`
}

// ForgeInstanceType is a machine shape the site offers, and what it provides.
// Matching is against Provides rather than against the name, so a request is
// answered by what a machine is rather than by what it is called.
type ForgeInstanceType struct {
	Name     string            `json:"name"`
	ID       string            `json:"id"`
	Provides map[string]string `json:"provides"`
}

func isInfrastructureCapability(key string) bool {
	switch key {
	case capabilityMachineType, capabilityArch, capabilityGPU:
		return true
	default:
		return false
	}
}

// Forge decodes the environment as a Forge declaration, failing when it is
// provided by something else.
func (e *Environment) Forge() (*Forge, error) {
	var f Forge
	if err := e.decodeInfrastructure(ForgeProvider, &f); err != nil {
		return nil, err
	}
	if f.Org.ID == "" {
		return nil, fmt.Errorf("environment %q declares no org id", e.Name)
	}
	if len(f.Sites) == 0 {
		return nil, fmt.Errorf("environment %q declares no sites", e.Name)
	}
	return &f, nil
}

// Site returns the named site. An empty name resolves to the only site when
// there is exactly one, which is the common case and saves every caller naming
// it; with several declared, the caller has to choose.
func (f *Forge) Site(name string) (ForgeSite, error) {
	if name == "" {
		if len(f.Sites) != 1 {
			return ForgeSite{}, fmt.Errorf("no site named and %d are declared (%s)", len(f.Sites), strings.Join(f.siteNames(), ", "))
		}
		for key := range f.Sites {
			name = key
		}
	}

	site, ok := f.Sites[name]
	if !ok {
		return ForgeSite{}, fmt.Errorf("no site %q; declared sites are %s", name, strings.Join(f.siteNames(), ", "))
	}
	site.Key = name
	if err := site.validate(); err != nil {
		return ForgeSite{}, err
	}
	return site, nil
}

// validate rejects a site missing anything a cluster cannot be created
// without. Checked on lookup so the run stops at the environment file, naming
// the field, rather than at an API rejection naming a field the caller never
// set.
func (s ForgeSite) validate() error {
	missing := make([]string, 0, 6)
	for _, field := range []struct{ name, value string }{
		{"id", s.ID},
		{"vpcID", s.VpcID},
		{"subnetID", s.SubnetID},
		{"sshKeyGroupID", s.SSHKeyGroupID},
	} {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}
	if len(s.InstanceTypes) == 0 {
		missing = append(missing, "instanceTypes")
	}
	if len(missing) > 0 {
		return fmt.Errorf("site %q is missing %s", s.Key, strings.Join(missing, ", "))
	}
	return nil
}

func (f *Forge) siteNames() []string {
	names := make([]string, 0, len(f.Sites))
	for name := range f.Sites {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// MatchInstanceType returns the first declared type providing everything the
// run asks for. First wins, so the file declares types cheapest first.
//
// There is no fallback. A request nothing satisfies fails, because the site
// genuinely cannot host that configuration and a near-miss machine would
// qualify something other than what was asked for.
func (s ForgeSite) MatchInstanceType(capabilities map[string]string) (ForgeInstanceType, error) {
	for _, candidate := range s.InstanceTypes {
		if candidate.provides(capabilities) {
			return candidate, nil
		}
	}

	offered := make([]string, 0, len(s.InstanceTypes))
	for _, candidate := range s.InstanceTypes {
		offered = append(offered, candidate.Name)
	}
	return ForgeInstanceType{}, fmt.Errorf(
		"site %q offers no instance type providing %s; it offers %s",
		s.Key, formatCapabilities(capabilities), strings.Join(offered, ", "),
	)
}

func (t ForgeInstanceType) provides(capabilities map[string]string) bool {
	for key, want := range capabilities {
		if !isInfrastructureCapability(key) {
			continue
		}
		if t.Provides[key] != want {
			return false
		}
	}
	return true
}

// formatCapabilities renders a capability set for an error message, sorted so
// the same failure reads the same way twice.
func formatCapabilities(capabilities map[string]string) string {
	pairs := make([]string, 0, len(capabilities))
	for key, value := range capabilities {
		if !isInfrastructureCapability(key) {
			continue
		}
		pairs = append(pairs, key+"="+value)
	}
	if len(pairs) == 0 {
		return "{}"
	}
	sort.Strings(pairs)
	return "{" + strings.Join(pairs, ", ") + "}"
}
