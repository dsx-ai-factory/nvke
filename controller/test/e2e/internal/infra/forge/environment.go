// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/infra"
)

const (
	// ProviderName is the provider name environments/forge.yaml declares.
	ProviderName = "forge"
)

// Environment is Forge's dialect of an infrastructure declaration. None of it
// is generic: an org, a site and a VPC are Forge's own vocabulary, which is
// why this lives here rather than in the environment package.
type Environment struct {
	Org   Org             `json:"org"`
	Sites map[string]Site `json:"sites"`
}

// Org identifies the Forge tenancy the run creates things in.
type Org struct {
	ID string `json:"id"`
}

// Site is one location, carrying the network identifiers a cluster is
// created against and the machines the site can offer.
type Site struct {
	// Key is the name this site is filed under, populated on lookup so callers
	// can report which site answered without threading the name alongside.
	Key string `json:"-"`

	ID    string `json:"id"`
	VpcID string `json:"vpcID"`
	// SubnetID or VpcPrefixID, never both: a VPC exposes subnets or prefixes
	// according to its network virtualization, and which one a site uses is a
	// property of its fabric rather than a choice.
	SubnetID    string `json:"subnetID"`
	VpcPrefixID string `json:"vpcPrefixID"`

	// SSHKeyGroupID is attached to the nodes Forge creates. It governs
	// Serial-over-LAN console access rather than the OS account's
	// authorized_keys, so attaching one grants no shell on a node -- that
	// comes from the account baked into the image, or from an authorized_keys
	// written by cloud-init. Nothing here logs into a tenant node, so this is
	// carried for parity with what a Forge node normally gets rather than
	// because a test uses it. SADD 3.5.3 invites the opposite reading by
	// passing the same field to CreateInstance beside a wait-for-SSH step.
	SSHKeyGroupID string `json:"sshKeyGroupID"`

	// JumpHost is the node the site's machines are reached through; they are
	// not Teleport-enrolled themselves. Optional here, because only a lane
	// that provisions machines on the site needs it, and forge.New rejects an
	// empty one. The Teleport proxy is not declared: tsh takes it from
	// $TELEPORT_PROXY or the logged-in profile.
	JumpHost string `json:"jumpHost"`
	// TeleportLogin is the jump host login; empty is tsh's default.
	TeleportLogin string `json:"teleportLogin"`
	// JumpHostUser is the jump host login for the plain-SSH route, taken
	// when JUMP_HOST_PASSWORD is set. Empty falls back to TeleportLogin, so a
	// site that uses the same account for both names it once.
	JumpHostUser string `json:"jumpHostUser"`
	// JumpHostKeyFingerprint pins the jump host's host key on the plain-SSH
	// route. Empty accepts whatever it presents and warns on every dial; see
	// teleport.SSHJumpHost for how to read one.
	JumpHostKeyFingerprint string `json:"jumpHostKeyFingerprint"`

	// No control-plane VIP. The controller treats an empty one as "do not configure
	// kube-vip", which is what create-forge-nico-cluster.sh relies on: it
	// sends no VIP at all. The simulator backends need one because their BGP
	// speakers advertise it, and a real site does not work that way.

	InstanceTypes []InstanceType `json:"instanceTypes"`
}

// InstanceType is a machine shape the site offers, and what it provides.
// Matching is against Provides rather than against the name, so a request is
// answered by what a machine is rather than by what it is called.
type InstanceType struct {
	Name     string            `json:"name"`
	ID       string            `json:"id"`
	Provides map[string]string `json:"provides"`
}

// Arch is the architecture this type provides, or empty if it declares none.
// A method rather than a caller reaching into Provides, so the capability key
// stays in the package that defines it.
func (t InstanceType) Arch() string { return t.Provides[infra.CapabilityArch] }

func isInfrastructureCapability(key string) bool {
	switch key {
	case infra.CapabilityMachineType, infra.CapabilityArch, infra.CapabilityGPU:
		return true
	default:
		return false
	}
}

// Decode reads an environment's opaque infrastructure block as Forge's
// dialect.
//
// Raw bytes rather than the environment they came from: this package speaks
// one dialect and has no reason to know the generic declaration that carries
// it. Which provider wrote the block is the caller's to establish before
// calling, and the caller that dispatches on it has already done so.
func Decode(raw json.RawMessage) (*Environment, error) {
	if len(raw) == 0 {
		return nil, errors.New("no infrastructure declared")
	}
	// Strict: a misspelled optional key, such as the jump host's key
	// fingerprint, would otherwise drop the pin without a word.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f Environment
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decoding forge infrastructure: %w", err)
	}
	if f.Org.ID == "" {
		return nil, errors.New("no org id declared")
	}
	if len(f.Sites) == 0 {
		return nil, errors.New("no sites declared")
	}
	return &f, nil
}

// Site returns the named site. An empty name resolves to the only site when
// there is exactly one, which is the common case and saves every caller naming
// it; with several declared, the caller has to choose.
func (f *Environment) Site(name string) (Site, error) {
	if name == "" {
		if len(f.Sites) != 1 {
			return Site{}, fmt.Errorf("no site named and %d are declared (%s)", len(f.Sites), strings.Join(f.siteNames(), ", "))
		}
		for key := range f.Sites {
			name = key
		}
	}

	site, ok := f.Sites[name]
	if !ok {
		return Site{}, fmt.Errorf("no site %q; declared sites are %s", name, strings.Join(f.siteNames(), ", "))
	}
	site.Key = name
	if err := site.validate(); err != nil {
		return Site{}, err
	}
	return site, nil
}

// validate rejects a site missing anything a cluster cannot be created
// without. Checked on lookup so the run stops at the environment file, naming
// the field, rather than at an API rejection naming a field the caller never
// set.
func (s Site) validate() error {
	missing := make([]string, 0, 6)
	for _, field := range []struct{ name, value string }{
		{"id", s.ID},
		{"vpcID", s.VpcID},
		{"sshKeyGroupID", s.SSHKeyGroupID},
	} {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}
	if len(s.InstanceTypes) == 0 {
		missing = append(missing, "instanceTypes")
	}
	// A type with no id reaches CreateInstance as instanceTypeId "", and the
	// error names Forge's field rather than the variable that was never set --
	// the same late failure the site identifiers above are checked to avoid.
	// The name is what a lane selects a type by, so a type without one is
	// unreachable rather than wrong, and either way it is worth saying now.
	for i, instanceType := range s.InstanceTypes {
		where := fmt.Sprintf("instanceTypes[%d]", i)
		if name := strings.TrimSpace(instanceType.Name); name != "" {
			where = "instanceTypes[" + name + "]"
		} else {
			missing = append(missing, where+".name")
		}
		if strings.TrimSpace(instanceType.ID) == "" {
			missing = append(missing, where+".id")
		}
	}
	switch {
	case s.SubnetID == "" && s.VpcPrefixID == "":
		missing = append(missing, "subnetID or vpcPrefixID")
	case s.SubnetID != "" && s.VpcPrefixID != "":
		missing = append(missing, "both subnetID and vpcPrefixID, which a VPC never exposes together")
	}
	if len(missing) > 0 {
		return fmt.Errorf("site %q is missing %s", s.Key, strings.Join(missing, ", "))
	}
	return nil
}

func (f *Environment) siteNames() []string {
	names := make([]string, 0, len(f.Sites))
	for name := range f.Sites {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Selection is the machines one run resolves to at one site.
//
// Two shapes rather than one, because a run's CPU machines and its GPU
// machines need not be the same architecture. A GB200 tenant puts arm64 GPU
// workers behind an amd64 control plane, and the site controller in front of
// both is a third machine of the CPU kind.
type Selection struct {
	Site Site

	// CPU carries the site controller and the tenant control plane. Its
	// architecture is the run's `arch` capability, because that is the one a
	// caller chooses: the CPU machines are supporting infrastructure and
	// nothing about the workload decides what they are.
	CPU InstanceType

	// GPU carries the tenant workers, and is the zero value when the run
	// asked for no GPU. Its architecture is read from the shape rather than
	// requested, because a GPU model comes on the architecture it comes on:
	// asking for a GB200 has already decided arm64, and stating it separately
	// could only contradict the hardware.
	GPU InstanceType
}

// HasGPU reports whether the run asked for a GPU shape.
func (s Selection) HasGPU() bool { return s.GPU.ID != "" }

// WorkerType is the shape a tenant's workers take: the GPU shape when the run
// asked for one, else the same CPU shape as everything else.
func (s Selection) WorkerType() InstanceType {
	if s.HasGPU() {
		return s.GPU
	}
	return s.CPU
}

// Resolve picks the site and the machine shapes that satisfy a run's
// capabilities, searching every declared site rather than a pre-chosen one.
//
// Selection is by capability because that is what a stream names. A run asks
// for {gpu: gb200, arch: amd64}; which site owns both such machines is
// infrastructure, and nothing above this should have to know it. Keying on a
// site name instead works only while exactly one site is declared, and turns
// declaring a second into an error for every run that did not name one.
//
// The two capabilities are read differently, and deliberately. `arch` applies
// to the CPU shape alone: it is the architecture of the supporting machines,
// which is a choice. `gpu` selects by model with no architecture attached,
// because the model already determines one. Applying `arch` to both would ask
// a single machine to be a GB200 and amd64 at once, which no site offers.
//
// Sites are searched in name order so two sites offering the same shapes
// resolve the same way twice. Within a site, MatchInstanceType takes the first
// declared type, so a file lists shapes cheapest first.
func (f *Environment) Resolve(capabilities map[string]string) (Selection, error) {
	if len(f.Sites) == 0 {
		return Selection{}, errors.New("the environment declares no sites")
	}
	arch := capabilities[infra.CapabilityArch]
	if strings.TrimSpace(arch) == "" {
		arch = infra.DefaultArch
	}
	gpu := strings.TrimSpace(capabilities[infra.CapabilityGPU])

	// A site that does not validate is skipped rather than fatal: the file
	// declares what exists, and a repository that has not onboarded one of
	// them leaves its identifiers unset. Breaking every run over a site this
	// one was never going to use would make declaring a second site a
	// coordinated change instead of an additive one.
	var unusable []string
	for _, name := range f.siteNames() {
		site, err := f.Site(name)
		if err != nil {
			unusable = append(unusable, fmt.Sprintf("%s (%v)", name, err))
			continue
		}
		cpu, err := site.MatchCPU(arch)
		if err != nil {
			continue
		}
		selection := Selection{Site: site, CPU: cpu}
		if gpu != "" {
			accelerator, err := site.MatchGPU(gpu)
			if err != nil {
				continue
			}
			selection.GPU = accelerator
		}
		return selection, nil
	}

	// Naming every site and what each offers, because the usual cause is a
	// shape no declared site has rather than a typo in one of them.
	wanted := fmt.Sprintf("a %s CPU shape", arch)
	if gpu != "" {
		wanted += fmt.Sprintf(" and a %s GPU shape", gpu)
	}
	offered := make([]string, 0, len(f.Sites))
	for _, name := range f.siteNames() {
		var shapes []string
		for _, candidate := range f.Sites[name].InstanceTypes {
			shapes = append(shapes, candidate.Name)
		}
		offered = append(offered, fmt.Sprintf("%s (%s)", name, strings.Join(shapes, ", ")))
	}
	msg := fmt.Sprintf("no declared site offers %s; sites are %s", wanted, strings.Join(offered, "; "))
	if len(unusable) > 0 {
		msg += fmt.Sprintf(". Skipped as incomplete: %s", strings.Join(unusable, "; "))
	}
	return Selection{}, errors.New(msg)
}

// MatchInstanceType returns the first declared type providing everything the
// run asks for. First wins, so the file declares types cheapest first.
//
// There is no fallback. A request nothing satisfies fails, because the site
// genuinely cannot host that configuration and a near-miss machine would
// qualify something other than what was asked for.
func (s Site) MatchInstanceType(capabilities map[string]string) (InstanceType, error) {
	for _, candidate := range s.InstanceTypes {
		if candidate.provides(capabilities) {
			return candidate, nil
		}
	}

	offered := make([]string, 0, len(s.InstanceTypes))
	for _, candidate := range s.InstanceTypes {
		offered = append(offered, candidate.Name)
	}
	return InstanceType{}, fmt.Errorf(
		"site %q offers no instance type providing %s; it offers %s",
		s.Key, formatCapabilities(capabilities), strings.Join(offered, ", "),
	)
}

// MatchCPU returns this site's CPU instance type for an architecture. It
// exists so a caller wanting a non-GPU machine does not have to spell the
// capability keys itself, which would put them in two packages and let them
// drift apart.
//
// The architecture is a parameter rather than derived, because the caller
// asking for this already has one to match: a cluster's control plane and its
// workers cannot run different OS images.
func (s Site) MatchCPU(arch string) (InstanceType, error) {
	return s.MatchInstanceType(map[string]string{
		infra.CapabilityMachineType: infra.MachineTypeCPU,
		infra.CapabilityArch:        arch,
	})
}

// MatchGPU returns this site's shape for a GPU model. No architecture is
// asked for: a model comes on the architecture it comes on, and this is read
// back off the result rather than requested.
func (s Site) MatchGPU(model string) (InstanceType, error) {
	return s.MatchInstanceType(map[string]string{
		infra.CapabilityMachineType: infra.MachineTypeGPU,
		infra.CapabilityGPU:         model,
	})
}

func (t InstanceType) provides(capabilities map[string]string) bool {
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
