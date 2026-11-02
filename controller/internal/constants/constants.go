// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package constants

import (
	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const (
	ProvisionClusterWorkflowPrefix     = "provision-cluster-"
	TeardownClusterWorkflowPrefix      = "teardown-cluster-"
	EnableEtcdEncryptionWorkflowPrefix = "enable-etcd-encryption-"
	EnableOIDCWorkflowPrefix           = "enable-oidc-"
	UpdateSpireServerWorkflowPrefix    = "update-spire-server-"

	EtcdEncryptionKeySecretPrefix     = "etcd-encryption-key-"
	EtcdEncryptionKeyFieldActive      = "active"
	EtcdEncryptionKeyFieldActiveSince = "active-since"
	EtcdEncryptionKeyFieldPending     = "pending"
	EtcdEncryptionKeyFieldActiveName  = "active-name"
	EtcdEncryptionKeyFieldPendingName = "pending-name"
	LegacyAPIEncryptionKeyName        = "current-primary-encryption-key"

	ProvisionNodePoolWorkflowPrefix = "provision-node-pool-"
	TeardownNodePoolWorkflowPrefix  = "teardown-node-pool-"

	ProvisionNodeWorkflowPrefix = "provision-node-"
	JoinNodeWorkflowPrefix      = "join-node-"
	TeardownNodeWorkflowPrefix  = "teardown-node-"
	PatchNodeWorkflowPrefix     = "patch-node-"

	TeardownNodeWorkflowType         = "TeardownNode"
	EnableEtcdEncryptionWorkflowType = "EnableEtcdEncryption"
	EnableOIDCWorkflowType           = "EnableOIDC"
	UpdateSpireServerWorkflowType    = "UpdateSpireServer"

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

	// NamespaceNVKESystem is the shared management-plane namespace for tenant-access
	// Secrets and OIDC-mode CAPI objects. The Helm chart creates it unconditionally.
	//
	// The value lags the name on purpose. The authoritative default is the
	// chart's nvkeSystemNamespace.name; this is only the compiled fallback, and
	// it has to stay "nke-system" for the same reason as
	// LegacyManagementNamespace below. It is not renamed with the product.
	NamespaceNVKESystem = "nke-system"

	// LegacyManagementNamespace is the shared management-plane namespace every
	// site installed before the namespace became configurable uses. It is the
	// value config.Config.ManagementNamespaceOrDefault falls back to when unset,
	// so a controller handed a config.yaml written before managementNamespace
	// existed keeps reading and writing exactly where it always did.
	//
	// It is not the default a new site gets. That is the chart's
	// nvkeSystemNamespace.name, which the chart always renders into
	// managementNamespace. The new default is deliberately not duplicated here:
	// the controller must never pick it on its own, or an existing site that has
	// not pinned the old name would be moved by a binary upgrade alone.
	LegacyManagementNamespace = "nke-system"

	// LabelValueKubeconfigMode* record whether a published kubeconfig Secret holds
	// SPIRE exec-auth or a verbatim admin.conf. The mode is rewritten in place when
	// EnableOIDC flips a cluster, so it is descriptive, not an ownership key:
	// publish and delete key off cluster-id alone, and the consume path checks the
	// payload itself rather than keys.KubeconfigMode.
	LabelValueKubeconfigModeSPIRE = "spire"
	LabelValueKubeconfigModeAdmin = "admin"

	// NVKE Field Managers for server-side apply. This controller's own three
	// (config.Config.FieldManagerName, FieldManagerRebootMachineName,
	// FieldManagerMachineHealthIssueName) are per-install configurable, not
	// constants here. The NICo manager retains its existing identity when the
	// controller starts managing DSX IMDS entries too, avoiding SSA ownership churn.
	FieldManagerNicoSpiffe        = "nico-tpm-spiffe-controller"
	FieldManagerLaunchLayerSpiffe = "launchlayer-spiffe-controller"
	FieldManagerMachineSpireEntry = "machine-spire-entry-controller"

	LabelValueTerminateStatusInProgress = "InProgress"
	LabelValueTerminateStatusNotAllowed = "NotAllowed"

	TaintEffectNoSchedule = "NoSchedule"

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
	return NodeTaint{Key: keys.ProvisioningTaint.String(), Value: "true", Effect: TaintEffectNoSchedule}
}

func EtcdEncryptionKeySecretName(clusterID uuid.UUID) string {
	return EtcdEncryptionKeySecretPrefix + clusterID.String()
}
