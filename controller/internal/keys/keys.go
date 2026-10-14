// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package keys builds the Kubernetes label, annotation, taint and condition
// keys the controller owns, from a configurable key domain.
//
// The domain defaults to LegacyDomain, so an unconfigured install produces
// exactly the keys it always has. A site that configures another domain writes
// the new keys but still reads the legacy ones (dual-read): objects labelled
// before the change keep working, and nothing has to be relabelled in lock
// step. Each Key reports both forms; callers write String() and read Get().
package keys

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// LegacyDomain is the key domain every existing site's objects carry.
	LegacyDomain = "nke.nvidia.com"

	// LegacyManagementNamespace is the management-plane namespace every
	// existing site uses.
	LegacyManagementNamespace = "nke-system"
)

// Key is one label, annotation, taint or condition key. The zero value is an
// empty key that matches nothing.
type Key struct {
	primary string
	// legacy is the same key under LegacyDomain, or empty when the configured
	// domain already is LegacyDomain. It is only ever read, never written.
	legacy string
}

func newKey(domain, name string) Key {
	k := Key{primary: domain + "/" + name}
	if domain != LegacyDomain {
		k.legacy = LegacyDomain + "/" + name
	}
	return k
}

// String returns the key under the configured domain. It is the form to write.
func (k Key) String() string { return k.primary }

// Legacy returns the key under LegacyDomain, or "" when the configured domain
// is LegacyDomain and there is no second form to read.
func (k Key) Legacy() string { return k.legacy }

// All returns every form of the key, the configured one first. Use it to
// delete a key: removing only String() would leave a legacy copy behind.
func (k Key) All() []string {
	if k.legacy == "" {
		return []string{k.primary}
	}
	return []string{k.primary, k.legacy}
}

// Get reads the key from m, preferring the configured form over the legacy one.
func (k Key) Get(m map[string]string) (string, bool) {
	if v, ok := m[k.primary]; ok {
		return v, true
	}
	if k.legacy != "" {
		v, ok := m[k.legacy]
		return v, ok
	}
	return "", false
}

// Value is Get without the presence flag.
func (k Key) Value(m map[string]string) string {
	v, _ := k.Get(m)
	return v
}

// Has reports whether m carries the key in either form.
func (k Key) Has(m map[string]string) bool {
	_, ok := k.Get(m)
	return ok
}

// Matches reports whether name is either form of the key.
func (k Key) Matches(name string) bool {
	return name == k.primary || (k.legacy != "" && name == k.legacy)
}

// Keys holds every domain-scoped key the controller uses, plus the management
// namespace. Build one with New and pass it to whatever reads or writes these
// objects; do not reach for a package-level copy.
type Keys struct {
	// Domain is the configured key domain.
	Domain string
	// ManagementNamespace is the shared management-plane namespace for
	// tenant-access Secrets and OIDC-mode CAPI objects.
	ManagementNamespace string

	// Labels on Forge instances.
	NodeID     Key
	NodePoolID Key
	ClusterID  Key
	IPAddress  Key
	SiteID     Key

	// Labels on Kubernetes node objects. These are deliberately distinct from
	// the Forge-instance labels above even where the name matches (site-id):
	// the two families target different objects and own separate deprecation
	// contracts.
	MachineID    Key
	NodeSiteID   Key
	NodeSiteName Key
	NodeVPCID    Key
	NodeVPCName  Key

	// Labels on Kubernetes node objects for remediation.
	Reboot                Key
	RebootStarted         Key
	RebootAttempt         Key
	LastCompletedReboot   Key
	RebootStatus          Key
	RebootStatusTimestamp Key
	Terminate             Key
	TerminateStatus       Key

	// ManagedBy marks objects this controller owns. Its value is
	// config.Config.ManagedByLabelValue, not a constant.
	ManagedBy Key

	// KubeconfigMode records whether a published kubeconfig Secret holds SPIRE
	// exec-auth or a verbatim admin.conf.
	KubeconfigMode Key

	// NodeHealthIssueAnnotation is the health-issue annotation on nodes.
	NodeHealthIssueAnnotation Key

	// Annotations on CAPI Machines.
	HostID                       Key
	RebootCompleteTimestamp      Key
	MachineRebootAnnotation      Key
	MachineHealthIssueAnnotation Key
	SpireAgentSpiffeIDPath       Key

	// Taints.
	MaintenanceTaint  Key
	ProvisioningTaint Key

	// Node condition types.
	NeedsRecycle Key
	NeedsRepair  Key
}

// New builds Keys for domain and managementNamespace. An empty argument falls
// back to its legacy default, so New("", "") is today's behavior.
func New(domain, managementNamespace string) Keys {
	if domain == "" {
		domain = LegacyDomain
	}
	if managementNamespace == "" {
		managementNamespace = LegacyManagementNamespace
	}
	k := func(name string) Key { return newKey(domain, name) }
	return Keys{
		Domain:              domain,
		ManagementNamespace: managementNamespace,

		NodeID:     k("node-id"),
		NodePoolID: k("node-pool-id"),
		ClusterID:  k("cluster-id"),
		IPAddress:  k("ip-address"),
		SiteID:     k("site-id"),

		MachineID:    k("machine-id"),
		NodeSiteID:   k("site-id"),
		NodeSiteName: k("site-name"),
		NodeVPCID:    k("vpc-id"),
		NodeVPCName:  k("vpc-name"),

		Reboot:                k("reboot"),
		RebootStarted:         k("reboot-started"),
		RebootAttempt:         k("reboot-attempt"),
		LastCompletedReboot:   k("last-completed-reboot"),
		RebootStatus:          k("reboot-status"),
		RebootStatusTimestamp: k("reboot-status-timestamp"),
		Terminate:             k("terminate"),
		TerminateStatus:       k("terminate-status"),

		ManagedBy:      k("managed-by"),
		KubeconfigMode: k("kubeconfig-mode"),

		NodeHealthIssueAnnotation: k("machine-health-issue"),

		HostID:                       k("host-id"),
		RebootCompleteTimestamp:      k("reboot-complete-timestamp"),
		MachineRebootAnnotation:      k("reboot"),
		MachineHealthIssueAnnotation: k("machine-health-issue"),
		SpireAgentSpiffeIDPath:       k("spire-agent-spiffe-id-path"),

		MaintenanceTaint:  k("maintenance"),
		ProvisioningTaint: k("provisioning"),

		NeedsRecycle: k("NeedsRecycle"),
		NeedsRepair:  k("NeedsRepair"),
	}
}

// Validate rejects a domain or namespace that Kubernetes would not accept as a
// key prefix or a namespace name. Empty values are valid: they mean the legacy
// default.
func Validate(domain, managementNamespace string) error {
	if domain != "" {
		if errs := validation.IsDNS1123Subdomain(domain); len(errs) > 0 {
			return fmt.Errorf("label domain %q is invalid: %v", domain, errs)
		}
	}
	if managementNamespace != "" {
		if errs := validation.IsDNS1123Label(managementNamespace); len(errs) > 0 {
			return fmt.Errorf("management namespace %q is invalid: %v", managementNamespace, errs)
		}
	}
	return nil
}
