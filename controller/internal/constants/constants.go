// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package constants

import (
	"github.com/google/uuid"
)

const (
	ProvisionClusterWorkflowPrefix     = "provision-cluster-"
	TeardownClusterWorkflowPrefix      = "teardown-cluster-"
	EnableEtcdEncryptionWorkflowPrefix = "enable-etcd-encryption-"
	EnableOIDCWorkflowPrefix           = "enable-oidc-"

	EtcdEncryptionKeySecretPrefix     = "etcd-encryption-key-"
	EtcdEncryptionKeyFieldActive      = "active"
	EtcdEncryptionKeyFieldActiveSince = "active-since"
	EtcdEncryptionKeyFieldPending     = "pending"

	ProvisionNodePoolWorkflowPrefix = "provision-node-pool-"
	TeardownNodePoolWorkflowPrefix  = "teardown-node-pool-"

	ProvisionNodeWorkflowPrefix = "provision-node-"
	JoinNodeWorkflowPrefix      = "join-node-"
	TeardownNodeWorkflowPrefix  = "teardown-node-"
	PatchNodeWorkflowPrefix     = "patch-node-"

	TeardownNodeWorkflowType         = "TeardownNode"
	EnableEtcdEncryptionWorkflowType = "EnableEtcdEncryption"
	EnableOIDCWorkflowType           = "EnableOIDC"

	RebootNodeWorkflowPrefix       = "reboot-node-"
	RebootNodeUptimeWorkflowPrefix = "reboot-node-uptime-"

	ApplyK8sNodeConditionWorkflowPrefix = "apply-k8s-node-condition-"

	UpgradeControlPlaneAddonsWorkflowPrefix = "kubeadm-upgrade-"
	UpdateKubeadmCMWorkflowPrefix           = "update-kubeadm-configmap-"
	RenewK8sCertsWorkflowPrefix             = "renew-k8s-certs-"

	RebootNodeReasonProvisioningWorkflow = "provisioning-workflow"

	MachineCapabilityTypeInfiniBand = "InfiniBand"
	MachineCapabilityTypeNVLink     = "NVLink Interface"
	MachineCapabilityTypeGPU        = "GPU"

	MachineCapabilityDeviceTypeNVLink = "NVLink"

	// NVKE label keys for Forge instances
	LabelKeyNodeID     = "nke.nvidia.com/node-id"
	LabelKeyNodePoolID = "nke.nvidia.com/node-pool-id"
	LabelKeyClusterID  = "nke.nvidia.com/cluster-id"
	LabelKeyIPAddress  = "nke.nvidia.com/ip-address"
	LabelKeySiteID     = "nke.nvidia.com/site-id"

	// NVKE label keys applied to Kubernetes node objects
	LabelKeyMachineID = "nke.nvidia.com/machine-id"

	// Forge topology label keys applied to Kubernetes node objects. These are
	// deliberately distinct from the Forge-instance label keys above (LabelKey*)
	// even where the string value matches (e.g. site-id): the two families target
	// different objects (K8s node vs. Forge instance) and own separate
	// deprecation contracts.
	//
	// NOTE: node.kubernetes.io/instance-type is intentionally NOT set here. The
	// prod tenant Prometheus SLO rules (prometheus/slo.rules) exact-match
	// instance_type="cpu" to exclude CPU nodes from GPU capacity/health math, and
	// the Forge instance-type name (e.g. "cpu.t1") does not match "cpu". Leave the
	// upstream/kubelet-provided value untouched.
	LabelKeyNodeSiteID   = "nke.nvidia.com/site-id"
	LabelKeyNodeSiteName = "nke.nvidia.com/site-name"
	LabelKeyNodeVPCID    = "nke.nvidia.com/vpc-id"
	LabelKeyNodeVPCName  = "nke.nvidia.com/vpc-name"

	// NVKE label keys for node remediation
	LabelKeyNodeLabelReboot                = "nke.nvidia.com/reboot"
	LabelKeyNodeLabelRebootStarted         = "nke.nvidia.com/reboot-started"
	LabelKeyNodeLabelRebootAttempt         = "nke.nvidia.com/reboot-attempt"
	LabelKeyNodeLabelLastCompletedReboot   = "nke.nvidia.com/last-completed-reboot"
	LabelKeyNodeLabelRebootStatus          = "nke.nvidia.com/reboot-status"
	LabelKeyNodeLabelRebootStatusTimestamp = "nke.nvidia.com/reboot-status-timestamp"
	LabelKeyNodeLabelTerminate             = "nke.nvidia.com/terminate"
	LabelKeyNodeLabelTerminateStatus       = "nke.nvidia.com/terminate-status"

	// NVKE label key for cluster static entries. The value is
	// config.Config.ManagedByLabelValue, not a constant -- it is
	// per-install configurable.
	LabelKeyNvkeManagedBy = "nke.nvidia.com/managed-by"

	// NamespaceNVKESystem is the shared management-plane namespace for tenant-access
	// Secrets and OIDC-mode CAPI objects. The Helm chart creates it unconditionally.
	NamespaceNVKESystem = "nke-system"

	// LabelKeyKubeconfigMode records whether a published kubeconfig Secret holds
	// SPIRE exec-auth or a verbatim admin.conf. The mode is rewritten in place when
	// EnableOIDC flips a cluster, so it is descriptive, not an ownership key:
	// publish and delete key off cluster-id alone, and the consume path checks the
	// payload itself rather than this label.
	LabelKeyKubeconfigMode        = "nke.nvidia.com/kubeconfig-mode"
	LabelValueKubeconfigModeSPIRE = "spire"
	LabelValueKubeconfigModeAdmin = "admin"

	// NVKE Field Managers for server-side apply. This controller's own three
	// (config.Config.FieldManagerName, FieldManagerRebootMachineName,
	// FieldManagerMachineHealthIssueName) are per-install configurable, not
	// constants here. The NICo manager retains its existing identity when the
	// controller starts managing DSX IMDS entries too, avoiding SSA ownership churn.
	FieldManagerNicoSpiffe        = "nico-tpm-spiffe-controller"
	FieldManagerMachineSpireEntry = "machine-spire-entry-controller"

	// Node annotations
	NodeAnnotationKeyMachineHealthIssue = "nke.nvidia.com/machine-health-issue"

	// CAPI Machine annotations
	MachineAnnotationKeyHostID                  = "nke.nvidia.com/host-id"
	MachineAnnotationKeyRebootCompleteTimestamp = "nke.nvidia.com/reboot-complete-timestamp"
	MachineAnnotationKeyReboot                  = "nke.nvidia.com/reboot"
	MachineAnnotationKeyMachineHealthIssue      = "nke.nvidia.com/machine-health-issue"
	MachineAnnotationKeySpireAgentSpiffeIDPath  = "nke.nvidia.com/spire-agent-spiffe-id-path"

	LabelValueTerminateStatusInProgress = "InProgress"
	LabelValueTerminateStatusNotAllowed = "NotAllowed"

	TaintEffectNoSchedule    = "NoSchedule"
	TaintKeyMaintenance      = "nke.nvidia.com/maintenance"
	TaintKeyNodeProvisioning = "nke.nvidia.com/provisioning"

	// NVKE Node Condition types
	NodeConditionTypeNeedsRecycle = "nke.nvidia.com/NeedsRecycle"
	NodeConditionTypeNeedsRepair  = "nke.nvidia.com/NeedsRepair"

	// SPIFFE ID paths expected from SPIRE agents
	TPMSpireAgentPath                = "/spire/agent/tpm/"
	DSXIMDSSpireAgentPath            = "/spire/agent/dsx_imds/tenant/"
	JoinTokenAlternateSpireAgentPath = "/join-token-spire-agent/host-id/" //nolint:gosec // This is a SPIFFE path prefix, not a credential.

	CAPISpireAgentConfigPath = "/etc/spire-agent/agent.conf"
	CAPISpireTrustBundlePath = "/etc/spire-agent/initial-trust-bundle.pem"
)

type RebootStatus string

const (
	RebootStatusInProgress             RebootStatus = "InProgress"
	RebootStatusCompleted              RebootStatus = "Completed"
	RebootStatusFailed                 RebootStatus = "Failed"
	RebootStatusNotAllowed             RebootStatus = "NotAllowed"
	RebootStatusCanceled               RebootStatus = "Canceled"
	RebootStatusFailedInstanceNotFound RebootStatus = "FailedInstanceNotFound"
)

type NodeTaint struct {
	Key    string
	Value  string
	Effect string
}

func NodeTaintProvisioning() NodeTaint {
	return NodeTaint{Key: TaintKeyNodeProvisioning, Value: "true", Effect: TaintEffectNoSchedule}
}

func EtcdEncryptionKeySecretName(clusterID uuid.UUID) string {
	return EtcdEncryptionKeySecretPrefix + clusterID.String()
}
