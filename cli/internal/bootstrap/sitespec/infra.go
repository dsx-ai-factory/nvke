// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitespec

import (
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"strings"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"gopkg.in/yaml.v3"

	api "github.com/dsx-ai-factory/nvke/controller/api"
)

// validHTTPEndpoint reports whether ep is an http or https URL with a host.
func validHTTPEndpoint(ep string) bool {
	u, err := url.Parse(ep)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

// Provider names where a site cluster's machines come from.
type Provider string

const (
	// ProviderProvisioned takes the machines from the hardcoded list in
	// cluster.nodes: hosts that already exist and are handed out one to each
	// machine. It is the default.
	ProviderProvisioned Provider = "provisioned"

	// ProviderNico asks CAPNICo to allocate the machines from a NICo site,
	// described by infra.nico.
	ProviderNico Provider = "nico"
)

// Infra selects the infrastructure provider and holds the settings the
// provider needs. A site spec without it is a provisioned one.
type Infra struct {
	// Provider is ProviderProvisioned (the default) or ProviderNico.
	Provider Provider `yaml:"provider,omitempty"`
	// Nico describes the NICo the site controller's own machines are allocated
	// from, during bootstrap only. Used only with ProviderNico. It is not the
	// NICo the finished controller uses for tenant clusters, which the stack
	// config describes.
	Nico *NicoInfra `yaml:"nico,omitempty"`
}

// NicoInfra describes the control-plane machines CAPNICo allocates for the site
// cluster and what the bootstrap needs to create them. Where the NICo is comes
// from exactly one mode block; Remote is the only one today, and another would
// sit beside it as a sibling field, so adding one changes nothing here.
//
// Credentials for the NICo API are not part of it: they are read from the file
// the NVKECTL_NICO_CREDENTIALS variable names.
type NicoInfra struct {
	// OrgID is the NICo organization the machines are created in. It sits above
	// the mode block because every mode has one.
	OrgID string `yaml:"orgID,omitempty"`
	// ControlPlane is what the control-plane machines are built with.
	ControlPlane NicoControlPlane `yaml:"controlPlane"`
	// Remote is an existing NICo, which the bootstrap CAPNICo reaches at its
	// endpoint with the credentials in NVKECTL_NICO_CREDENTIALS. Required.
	Remote *NicoRemote `yaml:"remote,omitempty"`
	// Releases is a file, or a directory of files, holding the MachineImage and
	// ClusterRelease manifests the bootstrap applies to the Kind cluster before
	// it creates the site cluster. The controller resolves the control plane's
	// release from them. Required.
	Releases string `yaml:"releases,omitempty"`
}

// NicoRemote is an existing NICo.
type NicoRemote struct {
	// Endpoint is the NICo API's base URL, an http or https URL.
	Endpoint string `yaml:"endpoint"`
	// SiteID is the NICo site the machines are created in.
	SiteID openapi_types.UUID `yaml:"siteID"`
	// VpcID is the VPC the machines are created in.
	VpcID openapi_types.UUID `yaml:"vpcID"`
	// APIName is the API name the NICo serves its REST API under, which CAPNICo
	// puts in every request path. Optional; DefaultNicoAPIName when unset.
	APIName string `yaml:"apiName,omitempty"`
}

// EffectiveAPIName returns the API name the bootstrap CAPNICo calls the NICo
// under: APIName, or DefaultNicoAPIName when it is unset.
func (r *NicoRemote) EffectiveAPIName() string {
	if r == nil || r.APIName == "" {
		return DefaultNicoAPIName
	}
	return r.APIName
}

// NicoControlPlane is the control-plane machines' config. It is built from the
// struct the controller's REST API takes for a NICo control plane, not from a
// mirror of it, so a field the API gains is a field this block accepts and the
// field names are the API's own. Count is the one addition: the API takes it as
// the request's desiredCount.
type NicoControlPlane struct {
	// Count is the number of control-plane nodes to allocate. Odd, and at least
	// one, like the length of the hardcoded list it stands in for.
	Count                              int `yaml:"count"`
	api.NicoControlPlaneProviderCreate `yaml:",inline"`
}

// MarshalYAML writes the block with the API's own field names, in the API's
// order and omitting what is unset: the embedded struct carries json tags only,
// which yaml.v3 would lower-case and write out empty.
func (c NicoControlPlane) MarshalYAML() (any, error) {
	body, err := json.Marshal(c.NicoControlPlaneProviderCreate)
	if err != nil {
		return nil, err
	}
	// JSON is YAML, so parsing it keeps the fields in order.
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	block := doc.Content[0]
	clearStyle(block)
	count := []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "count"},
		{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(c.Count)},
	}
	block.Content = append(count, block.Content...)
	return block, nil
}

// clearStyle puts n and what it holds back to block style, as written by hand.
func clearStyle(n *yaml.Node) {
	n.Style = 0
	for _, child := range n.Content {
		clearStyle(child)
	}
}

// IsNico reports whether the cluster's machines are allocated through CAPNICo.
func (inv *SiteSpec) IsNico() bool {
	return inv.Infra.Provider == ProviderNico
}

// ControlPlaneCount returns how many control-plane nodes the site cluster is
// built from: the length of cluster.nodes, or infra.nico.controlPlane.count
// when the machines are allocated from NICo.
func (inv *SiteSpec) ControlPlaneCount() int {
	if inv.IsNico() {
		if inv.Infra.Nico == nil {
			return 0
		}
		return inv.Infra.Nico.ControlPlane.Count
	}
	return len(inv.Cluster.Nodes)
}

// ClusterProvider returns the request's nico block: the organization, site and
// VPC, assembled from the fields that hold them. It is nil without a remote.
func (n *NicoInfra) ClusterProvider() *api.NicoClusterProvider {
	if n == nil || n.Remote == nil {
		return nil
	}
	return &api.NicoClusterProvider{OrgID: n.OrgID, SiteID: n.Remote.SiteID, VpcID: n.Remote.VpcID}
}

// checkInfra validates the choice of provider and then the block that choice
// makes mandatory. The other block must be absent: a file that lists both a
// hardcoded list and a NICo site is ambiguous about which it means, and
// silently ignoring one would hide a mistake.
func (v *validator) checkInfra(inv *SiteSpec) {
	// Empty means the default, so a SiteSpec that never went through SetDefaults
	// validates the way one that did would.
	provider := inv.Infra.Provider
	if provider == "" {
		provider = ProviderProvisioned
	}
	switch provider {
	case ProviderProvisioned:
		if inv.Infra.Nico != nil {
			v.addf("%s is only valid with %s: %q", pathNico, pathInfraProvider, ProviderNico)
		}
		v.checkNodes(inv.Cluster)
	case ProviderNico:
		if len(inv.Cluster.Nodes) > 0 {
			v.addf("%s must be empty with %s: %q; the nodes are allocated, not listed",
				pathNodes, pathInfraProvider, ProviderNico)
		}
		v.checkNico(inv.Infra.Nico)
	default:
		v.addf("%s %q is not supported (one of: %s, %s)",
			pathInfraProvider, inv.Infra.Provider, ProviderProvisioned, ProviderNico)
	}
}

// checkNico validates the NICo block. It checks shape only: whether an ID names
// something that exists is NICo's to say.
func (v *validator) checkNico(n *NicoInfra) {
	if n == nil {
		v.addf("%s is required with %s: %q", pathNico, pathInfraProvider, ProviderNico)
		return
	}
	if n.OrgID == "" {
		v.addf("%s.orgID is required", pathNico)
	}
	if n.Remote == nil {
		v.addf("%s.remote is required: it is the existing NICo the bootstrap CAPNICo calls", pathNico)
	} else {
		switch {
		case n.Remote.Endpoint == "":
			v.addf("%s.endpoint is required", pathNicoRemote)
		case !validHTTPEndpoint(n.Remote.Endpoint):
			v.addf("%s.endpoint %q must be an http or https URL", pathNicoRemote, n.Remote.Endpoint)
		}
		v.checkUUID(pathNicoRemote+".siteID", n.Remote.SiteID)
		v.checkUUID(pathNicoRemote+".vpcID", n.Remote.VpcID)
		// The name is one segment of every request path CAPNICo builds.
		if name := n.Remote.APIName; name != "" && (len(name) > 63 || !dnsLabelRE.MatchString(name)) {
			v.addf("%s.apiName %q must be a lowercase name such as forge or carbide", pathNicoRemote, name)
		}
	}
	v.checkNicoControlPlane(n.ControlPlane)
	if n.Releases == "" {
		v.addf("%s is required: the controller resolves %s.release from the MachineImage and ClusterRelease manifests in it",
			pathNicoReleases, pathNicoControlPlane)
	}
}

func (v *validator) checkNicoControlPlane(cp NicoControlPlane) {
	if cp.Count < 1 {
		v.addf("%s.count must be at least 1", pathNicoControlPlane)
	} else if cp.Count%2 == 0 {
		v.addf("%s.count must be an odd number", pathNicoControlPlane)
	}
	const cpPath = pathNicoControlPlane
	if len(cp.InstanceTypePreference) == 0 {
		v.addf("%s.instanceTypePreference must list at least one instance type ID", cpPath)
	}
	v.checkUUIDs(cpPath+".instanceTypePreference", cp.InstanceTypePreference)
	v.checkUUIDs(cpPath+".vpcPrefixIDs", cp.VpcPrefixIDs)
	v.checkUUIDs(cpPath+".subnetIDs", cp.SubnetIDs)
	if len(cp.SSHKeyGroupIDs) == 0 {
		v.addf("%s.sshKeyGroupIDs must list at least one SSH key group ID", cpPath)
	}
	v.checkUUIDs(cpPath+".sshKeyGroupIDs", cp.SSHKeyGroupIDs)
	switch {
	case len(cp.VpcPrefixIDs) == 0 && len(cp.SubnetIDs) == 0:
		v.addf("%s must set one of vpcPrefixIDs (native networking) or subnetIDs", cpPath)
	case len(cp.VpcPrefixIDs) > 0 && len(cp.SubnetIDs) > 0:
		v.addf("%s must set only one of vpcPrefixIDs and subnetIDs", cpPath)
	}
	if cp.Release == "" {
		v.addf("%s.release is required", cpPath)
	}
	// configProfile is optional: the bootstrap controller reads profiles from an
	// empty directory, so a name is passed on but never resolves, and none is
	// the same as an unresolved one.
}

// checkNicoFiles checks that the files the NICo block names can be read,
// relative paths resolved from the site spec's directory, and that the release
// manifests hold what the control plane needs.
func (v *validator) checkNicoFiles(inv *SiteSpec) {
	n := inv.Infra.Nico
	if !inv.IsNico() || n == nil {
		return
	}
	v.checkBootstrapReleases(inv)
}

// checkNicoCredentials checks the file NVKECTL_NICO_CREDENTIALS names. It is not
// part of the site spec, so only the commands that run the bootstrap look.
func (v *validator) checkNicoCredentials(inv *SiteSpec) {
	if !inv.IsNico() {
		return
	}
	v.checkNicoEndpointScheme(inv)
	if _, _, err := NodeLoginFromEnv(); err != nil {
		for _, problem := range strings.Split(err.Error(), "\n") {
			v.addf("%s", problem)
		}
	}
	path := NicoCredentialsPath()
	if path == "" {
		v.addf("%s must name the file holding the NICo token: the bootstrap CAPNICo needs it, and it is not in the site spec",
			NicoCredentialsEnv)
		return
	}
	if _, err := ReadNicoToken(path); err != nil {
		v.addf("%s: %v", NicoCredentialsEnv, err)
	}
}

// checkNicoEndpointScheme refuses an http infra.nico.remote.endpoint unless
// NicoInsecureEndpointEnv is 1: the bootstrap sends the NICo bearer token there.
// It is checked beside the token, on the host that holds it, because the same
// site spec is valid for the stack install, which does not send this token.
// The opt-in has no NKECTL_ name: it is new, and turning the check off answers
// only to the name the docs give.
func (v *validator) checkNicoEndpointScheme(inv *SiteSpec) {
	n := inv.Infra.Nico
	if n == nil || n.Remote == nil || os.Getenv(NicoInsecureEndpointEnv) == "1" {
		return
	}
	if u, err := url.Parse(n.Remote.Endpoint); err == nil && u.Scheme == "http" {
		v.addf("%s.endpoint %q is http, so the NICo token would be sent in cleartext; use https, or set %s=1 for a local or simulated NICo",
			pathNicoRemote, n.Remote.Endpoint, NicoInsecureEndpointEnv)
	}
}
