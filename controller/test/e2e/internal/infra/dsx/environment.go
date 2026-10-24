// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package dsx describes the infrastructure a DSX site offers, in DSX's own
// vocabulary.
//
// A DSX site runs its own site-local control plane rather than the hosted
// Forge API, and the two dialects are separate declarations even though they
// describe similar things. This mirrors the forge package deliberately rather
// than sharing with it: see the comment on forge.Environment, which makes the
// case from the other side. What actually differs is below --- a datahall
// offers machines as well as instance types, exposes prefixes rather than
// subnets, and carries its tenant in the declaration.
package dsx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra"
)

const (
	// ProviderName is the provider name environments/dsx.yaml declares.
	ProviderName = "dsx"
)

// Environment is DSX's dialect of an infrastructure declaration.
type Environment struct {
	Org   Org             `json:"org"`
	Sites map[string]Site `json:"sites"`
}

// Org identifies the DSX tenancy the run creates things in. It is a name
// rather than a number --- Forge orgs are numeric ids and DSX's is the literal
// string "ncx", which is also the path segment every request carries.
type Org struct {
	ID string `json:"id"`
}

// Site is one datahall, carrying the identifiers a cluster is created against
// and the machines it can offer.
//
// THE KEY IS THE DATAHALL, and every identifier below belongs to it. Two
// datahalls at one site --- dh2 and dh3 of the same one --- hold completely
// disjoint UUIDs for the same kinds of object, and nothing in an
// API response says which datahall an id came from. Nesting them under the key
// is what stops a VPC from one being paired with a prefix from the other.
type Site struct {
	// Key is the datahall this site is filed under, populated on lookup so
	// callers can report which site answered without threading the name
	// alongside. Endpoint derives the API host from it.
	Key string `json:"-"`

	// API is the carbide host this datahall answers on. Required: the key is
	// deliberately generic, so nothing derives a host from it, and an unset one
	// would dial a name that does not resolve.
	API string `json:"api"`

	// ID is the site id. Recorded, not sent: the host derived from Key already
	// names the datahall, and no request this package builds carries it. Kept
	// because it is what the datahall is called in the DSX APIs and in a
	// bring-up procedure, so a reader comparing the two has it.
	//
	// Not required, for that reason --- requiring a field nothing reads would
	// skip an otherwise usable datahall over a variable no run needs.
	ID string `json:"id"`

	// TenantID is the tenant every object is created under. Declared rather
	// than resolved from GET /tenant/current the way Forge does it: that call
	// needs a token, and a token needs a credential, so resolving it would put
	// an authenticated round trip in front of the validation that exists to
	// fail before anything is created.
	TenantID string `json:"tenantID"`

	VpcID string `json:"vpcID"`

	// VpcPrefixID, and no subnet. A DSX fabric is FNN, which exposes prefixes
	// only --- where Forge declares whichever of the two its site uses, there
	// is nothing to choose here.
	//
	// Each instance consumes a /31 out of the prefix and lands on the odd
	// address, so a /29 holds four nodes. A /31 leaves no spare address, which
	// is why a site controller here takes no control-plane VIP.
	VpcPrefixID string `json:"vpcPrefixID"`

	// SSHKeyGroupID is attached to the instances created here. It governs
	// Serial-over-LAN console access rather than the OS account's
	// authorized_keys, so attaching one grants no shell on a node --- that
	// comes from the account baked into the image, or from an authorized_keys
	// written by cloud-init. A group must also be associated with the site
	// before an instance naming it is accepted; the create call rejects an
	// unassociated group with 400 rather than ignoring it.
	SSHKeyGroupID string `json:"sshKeyGroupID"`

	// NodeUser is the account the OS image bakes, and the login every session
	// uses. Declared per site because a datahall running an internal-pipeline
	// image bakes a different account than the OSS one, and a mismatch writes
	// authorized_keys for an account that does not exist.
	NodeUser string `json:"nodeUser"`

	// JumpHost is the node this datahall's machines are reached through; they
	// are not Teleport-enrolled themselves. Named outright rather than derived
	// from Key: the naming happens to be regular today and that is not a
	// property worth depending on.
	JumpHost string `json:"jumpHost"`
	// TeleportLogin is the jump host login; empty is tsh's default.
	TeleportLogin string `json:"teleportLogin"`
	// JumpHostUser is the jump host login for the plain-SSH route, taken when
	// JUMP_HOST_PASSWORD is set. Empty falls back to TeleportLogin.
	JumpHostUser string `json:"jumpHostUser"`
	// JumpHostKeyFingerprint pins the jump host's host key on the plain-SSH
	// route. Empty accepts whatever it presents and warns on every dial.
	JumpHostKeyFingerprint string `json:"jumpHostKeyFingerprint"`

	// Machines are the hosts a site controller can be built on, targeted by
	// id. This is the half Forge has no equivalent for: its create request
	// names an instance type and the provider picks a machine, where DSX's
	// takes a machineId and names one.
	Machines []Machine `json:"machines"`

	// InstanceTypes are what the site controller's own CWE places TENANT
	// nodes on. A node pool names an instance type and has no machine field,
	// so the two halves of a run are placed by different mechanisms and both
	// have to be declared.
	InstanceTypes []InstanceType `json:"instanceTypes"`
}

// Endpoint is the API base for this datahall.
//
// TrimRight, not TrimSuffix: a value ending in "//" would otherwise keep one
// slash, and every path the caller joins would carry it. The duplicate check
// in Resolve compares endpoints as strings, so the same host written with two
// slashes would also read as a second datahall.
func (s Site) Endpoint() string {
	return strings.TrimRight(strings.TrimSpace(s.API), "/")
}

// Machine is one host a site controller can be built on.
//
// Name is the serial the datahall's operators use --- J701L43T --- rather than
// anything the API derives, because that is what a capacity conversation is
// held in and what an error has to name to be actionable.
type Machine struct {
	Name     string            `json:"name"`
	ID       string            `json:"id"`
	Provides map[string]string `json:"provides"`
}

// Arch is the architecture this machine provides, or empty if it declares none.
func (m Machine) Arch() string { return m.Provides[infra.CapabilityArch] }

// InstanceType is a machine shape the datahall offers tenants, and what it
// provides.
//
// An instance type here is not abstract sizing. It is a named allowlist of
// specific machines, bound to it one at a time, and a datahall can ship none at
// all --- so an id in this file states that someone has already created the
// type and bound hosts to it.
type InstanceType struct {
	Name     string            `json:"name"`
	ID       string            `json:"id"`
	Provides map[string]string `json:"provides"`
}

// Arch is the architecture this type provides, or empty if it declares none.
func (t InstanceType) Arch() string { return t.Provides[infra.CapabilityArch] }

func isInfrastructureCapability(key string) bool {
	switch key {
	case infra.CapabilityMachineType, infra.CapabilityArch, infra.CapabilityGPU:
		return true
	default:
		return false
	}
}

// Decode reads an environment's opaque infrastructure block as DSX's dialect.
//
// Raw bytes rather than the environment they came from: this package speaks one
// dialect and has no reason to know the generic declaration that carries it.
func Decode(raw json.RawMessage) (*Environment, error) {
	if len(raw) == 0 {
		return nil, errors.New("no infrastructure declared")
	}
	// Strict: a misspelled optional key, such as the jump host's key
	// fingerprint, would otherwise drop the pin without a word.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d Environment
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("decoding dsx infrastructure: %w", err)
	}
	if d.Org.ID == "" {
		return nil, errors.New("no org id declared")
	}
	if len(d.Sites) == 0 {
		return nil, errors.New("no sites declared")
	}
	return &d, nil
}

// Site returns the named datahall. An empty name resolves to the only one when
// there is exactly one, which is the common case and saves every caller naming
// it; with several declared, the caller has to choose.
func (d *Environment) Site(name string) (Site, error) {
	if name == "" {
		if len(d.Sites) != 1 {
			return Site{}, fmt.Errorf("no site named and %d are declared (%s)", len(d.Sites), strings.Join(d.siteNames(), ", "))
		}
		for key := range d.Sites {
			name = key
		}
	}

	site, ok := d.Sites[name]
	if !ok {
		return Site{}, fmt.Errorf("no site %q; declared sites are %s", name, strings.Join(d.siteNames(), ", "))
	}
	site.Key = name
	if err := site.validate(); err != nil {
		return Site{}, err
	}
	return site, nil
}

// validate rejects a datahall missing anything a cluster cannot be created
// without. Checked on lookup so a run stops at the environment file, naming the
// field, rather than at an API rejection naming a field the caller never set.
func (s Site) validate() error {
	missing := make([]string, 0, 8)
	for _, field := range []struct{ name, value string }{
		{"api", s.API},
		{"tenantID", s.TenantID},
		{"vpcID", s.VpcID},
		{"vpcPrefixID", s.VpcPrefixID},
		{"sshKeyGroupID", s.SSHKeyGroupID},
	} {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}
	// Instance types are required: they place the tenant nodes, which no
	// other field can, and they are also where the site controller goes
	// unless the site pins a machine.
	//
	// Machines are optional, and a file is expected to omit them. They pin the
	// site controller to one host, which is what a hand bring-up wants and
	// what a lane does not: a pinned machine that is held or unhealthy blocks
	// every run while the datahall's others sit free.
	if len(s.InstanceTypes) == 0 {
		missing = append(missing, "instanceTypes")
	}
	for i, machine := range s.Machines {
		where := fmt.Sprintf("machines[%d]", i)
		if name := strings.TrimSpace(machine.Name); name != "" {
			where = "machines[" + name + "]"
		} else {
			missing = append(missing, where+".name")
		}
		if strings.TrimSpace(machine.ID) == "" {
			missing = append(missing, where+".id")
		}
	}
	// A type with no id reaches the create call as instanceTypeId "", and the
	// error names the API's field rather than the variable that was never set.
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
	if len(missing) > 0 {
		return fmt.Errorf("site %q is missing %s", s.Key, strings.Join(missing, ", "))
	}
	return nil
}

func (d *Environment) siteNames() []string {
	names := make([]string, 0, len(d.Sites))
	for name := range d.Sites {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Selection is what one run resolves to at one datahall.
//
// Three members rather than Forge's two, because DSX can place the site
// controller and the tenant by different mechanisms: a site may pin the site
// controller to a machine targeted by id, while the tenant's nodes are always
// instance types its node pools name.
//
// Machine is optional and usually empty. A site that declares no machines ---
// which is what a lane wants, so a held or unhealthy host does not wedge every
// run --- places the site controller on the CPU instance type too, and Place
// returns that. See Resolve and Place.
type Selection struct {
	Site Site

	// Machine is the host the site controller itself is built on, targeted by
	// id. Its architecture is the run's `arch` capability.
	Machine Machine

	// CPU carries the tenant control plane. Its architecture is the run's
	// `arch` capability, because that is the one a caller chooses: these are
	// supporting machines and nothing about the workload decides what they are.
	CPU InstanceType

	// GPU carries the tenant workers, and is the zero value when the run asked
	// for no GPU. Its architecture is read from the shape rather than
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

// Resolve picks the datahall, the site-controller machine and the tenant
// shapes that satisfy a run's capabilities, searching every declared site
// rather than a pre-chosen one.
//
// Selection is by capability because that is what a stream names. A run asks
// for {gpu: gb200, arch: amd64}; which datahall owns both is infrastructure,
// and nothing above this should have to know it.
//
// Sites are searched in name order so two datahalls offering the same shapes
// resolve the same way twice. Within one, the first declared machine or type
// that satisfies the request wins, so a file lists them cheapest first.
func (d *Environment) Resolve(capabilities map[string]string) (Selection, error) {
	if len(d.Sites) == 0 {
		return Selection{}, errors.New("the environment declares no sites")
	}
	arch := capabilities[infra.CapabilityArch]
	if strings.TrimSpace(arch) == "" {
		arch = infra.DefaultArch
	}
	gpu := strings.TrimSpace(capabilities[infra.CapabilityGPU])

	// A site that does not validate is skipped rather than fatal: the file
	// declares what exists, and a repository that has not onboarded one of
	// them leaves its identifiers unset. Breaking every run over a datahall
	// this one was never going to use would make declaring a second one a
	// coordinated change instead of an additive one.
	var unusable []string
	for _, name := range d.siteNames() {
		site, err := d.Site(name)
		if err != nil {
			unusable = append(unusable, fmt.Sprintf("%s (%v)", name, err))
			continue
		}
		// Machines are optional: a site that declares none places the site
		// controller on an instance type and lets the datahall choose. Only a
		// site that pins has a machine to match, and failing to match a pinned
		// one still rules the site out.
		var machine Machine
		if len(site.Machines) > 0 {
			machine, err = site.MatchMachine(arch)
			if err != nil {
				continue
			}
		}
		cpu, err := site.matchSiteControllerType(arch)
		if err != nil {
			continue
		}
		selection := Selection{Site: site, Machine: machine, CPU: cpu}
		if gpu != "" {
			accelerator, err := site.MatchGPU(gpu)
			if err != nil {
				continue
			}
			selection.GPU = accelerator
		}
		return selection, nil
	}

	// Naming every datahall and what each offers, because the usual cause is a
	// shape no declared site has rather than a typo in one of them.
	wanted := fmt.Sprintf("a %s CPU shape", arch)
	if gpu != "" {
		wanted += fmt.Sprintf(" and a %s GPU shape", gpu)
	}
	offered := make([]string, 0, len(d.Sites))
	for _, name := range d.siteNames() {
		var shapes []string
		for _, candidate := range d.Sites[name].Machines {
			shapes = append(shapes, "machine "+candidate.Name)
		}
		for _, candidate := range d.Sites[name].InstanceTypes {
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

// Placement is where a node goes. A DSX create names either a specific
// machine or an instance type, and this says which the site asked for.
//
// An instance type is the default and the one a lane wants: the site picks a
// machine that is free and healthy, the same way Forge does. Pinning a machine
// is for a person standing up a datahall by hand, where choosing the host is
// the point -- and it is a liability in CI, because a pinned machine that is
// held or in Error blocks every run while the rest of the datahall sits idle.
// That cost a full day on 2026-10-12.
type Placement struct {
	// MachineID is set only when the site pins one. It needs the tenant's
	// targetedInstanceCreation capability; InstanceTypeID does not.
	MachineID string
	// InstanceTypeID is set when the site leaves the choice to the datahall.
	InstanceTypeID string
	// Arch is what was matched on, and decides the node images and host tools.
	Arch string
	// Name is what the file called it, for errors a reader can act on.
	Name string
}

// Place chooses where the site controller goes: a pinned machine when the site
// declares any, otherwise a CPU instance type of the requested architecture.
//
// Machines win when present so a datahall can still pin deliberately, and the
// file is expected to leave them out.
//
// CPU, not merely the architecture: a GPU type declares an architecture too,
// so matching on arch alone can put a site controller on accelerator hardware
// whenever a datahall declares a GPU type of that arch first. That is also the
// shape Resolve selected on, so asking for less here would place the run
// somewhere Resolve did not agree to -- which is why both go through
// matchSiteControllerType rather than each deciding for itself.
func (s Site) Place(arch string) (Placement, error) {
	if len(s.Machines) > 0 {
		machine, err := s.MatchMachine(arch)
		if err != nil {
			return Placement{}, err
		}
		return Placement{MachineID: machine.ID, Arch: machine.Arch(), Name: machine.Name}, nil
	}
	instanceType, err := s.matchSiteControllerType(arch)
	if err != nil {
		return Placement{}, err
	}
	return Placement{InstanceTypeID: instanceType.ID, Arch: instanceType.Arch(), Name: instanceType.Name}, nil
}

// matchSiteControllerType picks the instance type the site controller goes on.
//
// A datahall with CPU boxes resolves to one, which is what Place's comment
// above describes. datahall-1 has no CPU machine at all -- every machine it
// owns is a GPU tray -- so fall back to a GPU shape rather than refuse to
// place. Resolve and Place must agree on the shape or a run resolves against
// one type and is placed on another, so both call this.
func (s Site) matchSiteControllerType(arch string) (InstanceType, error) {
	instanceType, err := s.MatchCPU(arch)
	if err == nil {
		return instanceType, nil
	}
	return s.MatchInstanceType(map[string]string{
		infra.CapabilityMachineType: infra.MachineTypeGPU,
		infra.CapabilityArch:        arch,
	})
}

// MatchMachine returns the first declared machine providing the architecture
// asked for. First wins, so the file declares machines in the order it would
// rather spend them. There is deliberately no fallback to another
// architecture.
//
// Only reached when a site pins machines, which is the hand bring-up path --
// see Placement.
func (s Site) MatchMachine(arch string) (Machine, error) {
	for _, candidate := range s.Machines {
		if providesAll(candidate.Provides, map[string]string{infra.CapabilityArch: arch}) {
			return candidate, nil
		}
	}

	offered := make([]string, 0, len(s.Machines))
	for _, candidate := range s.Machines {
		offered = append(offered, fmt.Sprintf("%s (%s)", candidate.Name, candidate.Arch()))
	}
	return Machine{}, fmt.Errorf(
		"site %q offers no %s machine for the site controller; it offers %s",
		s.Key, arch, strings.Join(offered, ", "),
	)
}

// MatchInstanceType returns the first declared type providing everything the
// run asks for. First wins, so the file declares types cheapest first.
//
// There is no fallback. A request nothing satisfies fails, because the datahall
// genuinely cannot host that configuration and a near-miss machine would
// qualify something other than what was asked for.
func (s Site) MatchInstanceType(capabilities map[string]string) (InstanceType, error) {
	for _, candidate := range s.InstanceTypes {
		if providesAll(candidate.Provides, capabilities) {
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

// MatchCPU returns this datahall's CPU instance type for an architecture. It
// exists so a caller wanting a non-GPU shape does not have to spell the
// capability keys itself, which would put them in two packages and let them
// drift apart.
func (s Site) MatchCPU(arch string) (InstanceType, error) {
	return s.MatchInstanceType(map[string]string{
		infra.CapabilityMachineType: infra.MachineTypeCPU,
		infra.CapabilityArch:        arch,
	})
}

// MatchGPU returns this datahall's shape for a GPU model. No architecture is
// asked for: a model comes on the architecture it comes on, and this is read
// back off the result rather than requested.
func (s Site) MatchGPU(model string) (InstanceType, error) {
	return s.MatchInstanceType(map[string]string{
		infra.CapabilityMachineType: infra.MachineTypeGPU,
		infra.CapabilityGPU:         model,
	})
}

// providesAll reports whether provides satisfies every infrastructure
// capability wanted. Non-infrastructure keys are ignored, so a caller can pass
// a stream's whole capability set without filtering it first.
func providesAll(provides, wanted map[string]string) bool {
	for key, want := range wanted {
		if !isInfrastructureCapability(key) {
			continue
		}
		if provides[key] != want {
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
