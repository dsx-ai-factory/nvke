// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package keys holds the label, annotation, taint and node-condition keys this
// controller owns, under the nvke.nvidia.com domain.
//
// The rename from nke.nvidia.com is not atomic: objects labeled by an earlier
// build keep the old keys until something rewrites them, and the labels a
// human or an automation applies by hand (reboot, terminate) may be written
// under either domain for a long time. So every key carries both forms. The
// controller writes Key.String() -- always the new domain -- and reads with
// Get/Value/Has/Matches, which accept either. Key.All() is the delete form: it
// lists both, because removing only the new one leaves a legacy copy behind
// that the next read would happily find.
//
// Nothing here is configurable. Both domains are compiled in, exactly as
// cluster-api-provider-nico did for its own rename, because there is no third
// domain anything could read. A Key is the bare name, so every key below is a
// constant and the domains are only ever joined on at the point of use.
package keys

import (
	"strings"
)

const (
	// Domain is the key domain this controller writes.
	Domain = "nvke.nvidia.com"

	// LegacyDomain is the key domain it still reads. Objects created before
	// the rename carry it, and so do hand-applied requests.
	LegacyDomain = "nke.nvidia.com"
)

// Key is the name of one label, annotation, taint or node-condition this
// controller owns -- the part after the domain, such as "node-id". It renders
// into a full key under either domain.
type Key string

// String returns the key under Domain. This is the only form to write.
func (k Key) String() string { return Domain + "/" + string(k) }

// Legacy returns the key under LegacyDomain. Read it, never write it.
func (k Key) Legacy() string { return LegacyDomain + "/" + string(k) }

// All returns both forms, the written one first. Use it to delete a key:
// removing only String() would leave the legacy copy in place.
func (k Key) All() []string { return []string{k.String(), k.Legacy()} }

// Get reads the key from m, preferring the current domain over the legacy one.
func (k Key) Get(m map[string]string) (string, bool) {
	if v, ok := m[k.String()]; ok {
		return v, true
	}
	v, ok := m[k.Legacy()]
	return v, ok
}

// Value is Get without the presence flag. An absent key reads as "".
func (k Key) Value(m map[string]string) string {
	v, _ := k.Get(m)
	return v
}

// Has reports whether m carries the key in either domain.
func (k Key) Has(m map[string]string) bool {
	_, ok := k.Get(m)
	return ok
}

// Matches reports whether name is either form of the key.
func (k Key) Matches(name string) bool {
	return name == k.String() || name == k.Legacy()
}

// DeleteLegacy removes only the legacy form, for a caller that is about to
// write the current one and wants the object left carrying a single domain.
func (k Key) DeleteLegacy(m map[string]string) { delete(m, k.Legacy()) }

// Delete removes both forms of the key from m.
func (k Key) Delete(m map[string]string) {
	delete(m, k.String())
	delete(m, k.Legacy())
}

// Selector renders "<key>=<value>" against the current domain, for kubectl-style
// label arguments.
func (k Key) Selector(value string) string { return k.String() + "=" + value }

// RemoveSelectors renders the kubectl "<key>-" removal argument for both forms.
func (k Key) RemoveSelectors() []string { return []string{k.String() + "-", k.Legacy() + "-"} }

// IsOurs reports whether key sits under either domain, so that generic sweeps
// over an object's labels can tell this controller's keys from everyone else's.
func IsOurs(key string) bool {
	return strings.HasPrefix(key, Domain+"/") || strings.HasPrefix(key, LegacyDomain+"/")
}

// BothForms returns key under both domains when it is one of ours, and key
// alone otherwise. Use it where the key arrives as a plain string rather than
// a Key -- a Temporal activity input, say, whose recorded value may predate
// the rename -- and the operation is a removal that must not leave the other
// form behind.
func BothForms(key string) []string {
	switch {
	case strings.HasPrefix(key, Domain+"/"):
		return []string{key, LegacyDomain + strings.TrimPrefix(key, Domain)}
	case strings.HasPrefix(key, LegacyDomain+"/"):
		return []string{Domain + strings.TrimPrefix(key, LegacyDomain), key}
	default:
		return []string{key}
	}
}

const (
	// Labels on Forge/NICo instances. NICo caps an instance at ten labels,
	// which these three share with cluster-api-provider-nico's five topology
	// labels and Cluster API's two, so they are written in one domain only --
	// there is no room to carry both.
	NodeID     Key = "node-id"
	NodePoolID Key = "node-pool-id"
	ClusterID  Key = "cluster-id"
	IPAddress  Key = "ip-address"
	SiteID     Key = "site-id"

	// Labels on Kubernetes Node objects. These are deliberately distinct from
	// the instance labels above even where the name matches (site-id): the two
	// families target different objects and own separate deprecation
	// contracts.
	//
	// NOTE: node.kubernetes.io/instance-type is intentionally NOT set here.
	// The prod tenant Prometheus SLO rules (prometheus/slo.rules) exact-match
	// instance_type="cpu" to exclude CPU nodes from GPU capacity/health math,
	// and the Forge instance-type name (e.g. "cpu.t1") does not match "cpu".
	// Leave the upstream/kubelet-provided value untouched.
	MachineID    Key = "machine-id"
	NodeSiteID   Key = "site-id"
	NodeSiteName Key = "site-name"
	NodeVPCID    Key = "vpc-id"
	NodeVPCName  Key = "vpc-name"

	// Node labels driving remediation. Reboot and Terminate are requests: an
	// operator or an external automation applies them by hand, so they are
	// read in both domains indefinitely. The rest the controller writes
	// itself.
	Reboot                Key = "reboot"
	RebootStarted         Key = "reboot-started"
	RebootAttempt         Key = "reboot-attempt"
	LastCompletedReboot   Key = "last-completed-reboot"
	RebootStatus          Key = "reboot-status"
	RebootStatusTimestamp Key = "reboot-status-timestamp"
	Terminate             Key = "terminate"
	TerminateStatus       Key = "terminate-status"

	// ManagedBy marks objects this controller owns. Its value is
	// config.Config.ManagedByLabelValue, not a constant.
	ManagedBy Key = "managed-by"

	// KubeconfigMode records whether a published kubeconfig Secret holds SPIRE
	// exec-auth or a verbatim admin.conf.
	KubeconfigMode Key = "kubeconfig-mode"

	// NodeMachineHealthIssue is the health-issue annotation on a Kubernetes
	// Node.
	NodeMachineHealthIssue Key = "machine-health-issue"

	// Annotations on Cluster API Machines.
	//
	// MachineReboot and MachineHealthIssue are the handshake with
	// cluster-api-provider-nico, which watches exactly one key for each
	// (--reboot-annotation, --repair-annotation) and silently ignores a
	// request under any other, so those two flags must name the current
	// domain. They are the only keys here with a single configured reader.
	MachineHostID                  Key = "host-id"
	MachineRebootCompleteTimestamp Key = "reboot-complete-timestamp"
	MachineReboot                  Key = "reboot"
	MachineHealthIssue             Key = "machine-health-issue"
	MachineSpireAgentSpiffeIDPath  Key = "spire-agent-spiffe-id-path"

	// Taints.
	MaintenanceTaint  Key = "maintenance"
	ProvisioningTaint Key = "provisioning"

	// Node condition types.
	NeedsRecycle Key = "NeedsRecycle"
	NeedsRepair  Key = "NeedsRepair"
)
