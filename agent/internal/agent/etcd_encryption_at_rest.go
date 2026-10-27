// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/google/logger"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	yaml "gopkg.in/yaml.v2"

	"github.com/dsx-ai-factory/nvke/agent/internal/k8s"
	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
)

const (
	K8sAPIEncryptionEnabledStatus K8sAPIEncryptionStatusType = "Enabled"
	K8sAPIEncryptionPendingStatus K8sAPIEncryptionStatusType = "Pending"
	K8sAPIEncryptionErrorStatus   K8sAPIEncryptionStatusType = "Error"
	K8sAPIEncryptionNotConfigured K8sAPIEncryptionStatusType = "NotConfigured"

	InitializeK8sAPIEncryptionPhase      K8sAPIEncryptionPhaseType = "Initialize"
	InitializeK8sAPIEncryptionErrorPhase K8sAPIEncryptionPhaseType = "InitializeError"
	K8sAPIEncryptionInitializedPhase     K8sAPIEncryptionPhaseType = "EncryptionInitialized"
	K8sAPIEncryptionPhaseConfiguring     K8sAPIEncryptionPhaseType = "Configuring"
	K8sAPIEncryptionPhaseConfigured      K8sAPIEncryptionPhaseType = "Configured"

	RewriteObjectsEncryptionPhase      K8sAPIEncryptionPhaseType = "RewriteObjects"
	RewriteObjectsErrorEncryptionPhase K8sAPIEncryptionPhaseType = "RewriteObjectsError"
	ObjectsRewrittenEncryptionPhase    K8sAPIEncryptionPhaseType = "ObjectsRewritten"

	RestartAPIServerEncryptionPhase      K8sAPIEncryptionPhaseType = "RestartAPIServer"
	RestartAPIServerErrorEncryptionPhase K8sAPIEncryptionPhaseType = "RestartAPIServerError"
	RestartedAPIServerEncryptionPhase    K8sAPIEncryptionPhaseType = "RestartedAPIServer"

	APIEncryptionConfiguredLabel          string = "nvke.nvidia.com/k8s-api-encryption-configured=true"
	APIEncryptionRewriteCompleteLabel     string = "nvke.nvidia.com/k8s-api-encryption-rewrite-complete=true"
	APIEncryptionEnabledLabel             string = "nvke.nvidia.com/k8s-api-encryption-enabled=true"
	APIEncryptionSecondaryKeyEnabledLabel string = "nvke.nvidia.com/k8s-api-encryption-secondary-key-configured=true"

	kubectlNamespaceFlag = "-n"
	kubectlOutputFlag    = "-o"
	kubectlGetCommand    = "get"
)

type (
	K8sAPIEncryptionStatusType string
	K8sAPIEncryptionStatus     K8sAPIEncryptionStatusType

	K8sAPIEncryptionPhaseType string
	K8sAPIEncryptionPhase     K8sAPIEncryptionPhaseType

	K8sAPIEncryption struct {
		Status K8sAPIEncryptionStatusType `json:"status" yaml:"status"`
		Phase  K8sAPIEncryptionPhaseType  `json:"phase"  yaml:"phase"`
	}
)

var (
	objectsToEncrypt = []string{
		"secrets",
		"configmaps",
		"persistentvolumeclaims",
		"persistentvolumes",
		"serviceaccounts",
		"events",
		"services",
	}
	encryptionConfigTemplate = `
---
apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources:
  {{- range .ObjectsToEncrypt }}
    - {{ . }}
  {{- end }} 
    providers:
      - secretbox:
          keys:
            - name: {{ .EncryptionKeyName }}
              secret: {{ .EncryptionKey }}
      - identity: {}`

	// identity first so new writes stay plaintext in etcd until every control-plane
	// apiserver has the same key. Then promote the providers on every node and start
	// one asynchronous object rewrite with StartEtcdEncryptionAtRestObjectsRewrite.
	encryptionConfigTemplateExistingCluster = `
---
apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources:
  {{- range .ObjectsToEncrypt }}
    - {{ . }}
  {{- end }} 
    providers:
      - identity: {}
      - secretbox:
          keys:
            - name: {{ .EncryptionKeyName }}
              secret: {{ .EncryptionKey }}`

	volumeEntry = `- hostPath:
      path: /etc/kubernetes/kube-api-encryption
      type: DirectoryOrCreate
    name: encryption-config`

	volumeMountsEntry = `- mountPath: /etc/kubernetes/kube-api-encryption
      name: encryption-config
      readOnly: true`
)

// InitializeKubernetesAPIEncryption initializes the kubernetes API encryption configuration
// on the controlplane nodes.
// Includes a restart of the nodes kube-apiserver pod.
// NOTE:
//   - This is only supported on controlplane nodes
//   - Must be run on EVERY controlplane node!
//   - Must use the same key on all controlplane nodes
//   - Provider order is always identity then secretbox (HA-safe); call
//     PromoteKubernetesAPIEncryptionProviders on each node, then start one rewrite with
//     StartEtcdEncryptionAtRestObjectsRewrite and poll GetEtcdEncryptionAtRestObjectsRewriteStatus.
//     New clusters use kubeadm init with encryption instead of this RPC.
//
// We are NOT blocking on the error status or re-checking phase here
// instead, we check against the API manifest each time, try to recover from
// errors where possible.
func (s *NodeAgentServer) InitializeKubernetesAPIEncryption(
	ctx context.Context,
	req *pb.InitializeKubernetesAPIEncryptionRequest,
) (*pb.InitializeKubernetesAPIEncryptionResponse, error) {
	response := &pb.InitializeKubernetesAPIEncryptionResponse{}

	defer logRPCSpan(ctx, "InitializeKubernetesAPIEncryption")()
	defer s.PersistNodeStatus()

	log.Printf("performing InitializeKubernetesAPIEncryption...\n")

	hostname, err := os.Hostname()
	if err != nil {
		log.Printf("failed to get hostname: %v\n", err)
		response.ErrorMessage = "failed to get hostname"
		s.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionErrorStatus
		return response, err
	}

	s.NodeStatus.NodeName = hostname

	if s.NodeStatus.Role != ControlPlaneRole {
		log.Printf(
			"initialize encryption can only be run on controlplane nodes",
		)
		response.ErrorMessage = "initialize encryption can only be run on controlplane nodes"
		s.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionErrorStatus
		return response, errors.New(
			"initialize encryption can only be run on controlplane nodes",
		)
	}

	log.Printf("checking if encryption is already enabled...\n")
	log.Printf("check the api pod manifest for encryption config...\n")
	configured, err := s.verifyNodeAPIEncryptionEnabled()
	if err != nil {
		log.Printf("failed to verify node api encryption: %v\n", err)
		response.ErrorMessage = "failed to verify node api encryption"
		s.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionErrorStatus
		return response, err
	}
	if configured {
		s.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionPendingStatus
		s.NodeStatus.K8sAPIEncryption.Phase = K8sAPIEncryptionPhaseConfigured
		log.Printf("encryption already configured on node\n")
	} else {
		s.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionPendingStatus
		s.NodeStatus.K8sAPIEncryption.Phase = K8sAPIEncryptionPhaseConfiguring
	}

	// Verify kube-apiserver pod is up and running
	status, err := s.apiServerPodIsRunning()
	if err != nil {
		log.Printf("failed to check api-server pod status: %v\n", err)
		response.ErrorMessage = "failed to check api-server pod status"
		s.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionErrorStatus
		return response, err
	}

	if !status {
		log.Printf("kube-apiserver pod is not running")
		response.ErrorMessage = "kube-apiserver pod is not running"
		s.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionErrorStatus
		return response, errors.New("kube-apiserver pod is not running")
	}

	// We set the label first and then remove it if config fails.
	// The reason for this is that if things work as expected, as soon as
	// the config file is written the api pod will be restarted and the label
	// call will fail.  This way we can set everything up and not have to do
	// any sort of waiting or polling, just set the values; let the auto-restart
	// handle the rest.
	err = labelAPIEncryptionNode(ctx, s.NodeStatus.NodeName, APIEncryptionConfiguredLabel)
	if err != nil {
		log.Printf("failed to set api encryption label to `configured`: %v\n", err)
		response.ErrorMessage = "failed to set api encryption label to `configured`"
		return response, err
	}

	// Create the encryption-config file and update the kube-apiserver manifest
	// DO NOT run configure if already configured, the configure function needs to be
	// smarter, right now it's really dumb and will duplicate entries causing the pod
	// to fail
	if !configured {
		err = s.createNamedAPIEncryptionConfig(req.EncryptionKey, req.EncryptionKeyName, true, req.ObjectsToEncrypt)
		if err != nil {
			log.Printf("failed to initialize api encryption config: %v\n", err)
			s.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionErrorStatus
			response.ErrorMessage = "failed to initialize api encryption config"
			log.Printf("removing the configured label\n")
			err = removeAPIEncryptionNodeLabel(ctx, s.NodeStatus.NodeName, APIEncryptionConfiguredLabel)
			if err != nil {
				log.Printf("failed to remove api encryption configured label: %v\n", err)
			}
			return response, err
		}

		// Create the volume mount entries
		err = s.createVolumeMountEntries()
		if err != nil {
			log.Printf("failed to create volume mount entries: %v\n", err)
			s.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionErrorStatus
			response.ErrorMessage = "failed to create volume mount entries"
			log.Printf("removing the configured label\n")
			err = removeAPIEncryptionNodeLabel(ctx, s.NodeStatus.NodeName, APIEncryptionConfiguredLabel)
			if err != nil {
				log.Printf("failed to remove api encryption configured label: %v\n", err)
			}
			return response, err
		}

		// Modify the api manifest
		err = s.modifyAPIServerManifest()
		if err != nil {
			log.Printf("failed to modify api server manifest: %v\n", err)
			s.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionErrorStatus
			response.ErrorMessage = "failed to modify api server manifest"
			log.Printf("removing the configured label\n")
			err = removeAPIEncryptionNodeLabel(ctx, s.NodeStatus.NodeName, APIEncryptionConfiguredLabel)
			if err != nil {
				log.Printf("failed to remove api encryption configured label: %v\n", err)
			}
			return response, err
		}

		// Above will trigger a restart of api server, need to wait until it's
		// READY (not just Running) before returning. pod.status.phase flips to
		// "Running" within ~1-2s of kubelet recreating the static pod, but the
		// apiserver itself takes ~15-30s longer to bind 6443 and pass its
		// readiness probe (etcd connect, leader election, listener bind).
		// Returning at pod-phase=Running races with PromoteKubernetesAPIEncryptionProviders,
		// which immediately calls /metrics on the local apiserver.
		timeoutCounter := 0
		for {
			time.Sleep(10 * time.Second)
			if timeoutCounter > 10 {
				log.Printf("timed out waiting for kube-apiserver to become ready\n")
				response.ErrorMessage = "timed out waiting for kube-apiserver to become ready"
				return response, errors.New("timed out waiting for kube-apiserver to become ready")
			}
			if s.apiServerIsReady() {
				log.Printf("kube-apiserver is now ready\n")
				break
			}
			log.Printf("waiting for kube-apiserver to become ready...\n")
			timeoutCounter++
		}
	}
	if s.NodeStatus.IsMaster {
		if err := s.updateEncryptionEntryForKubeadmConfigMap(); err != nil {
			return response, err
		}
	}
	return response, nil
}

// commitPromoteEncryptionProvidersLocked reads and optionally rewrites the encryption
// config under encryptionMu. It always unlocks before return (via defer). Reload waiting
// must happen after this returns when wroteConfig is true.
func (n *NodeAgentServer) commitPromoteEncryptionProvidersLocked() (int, bool, error) {
	n.encryptionMu.Lock()
	defer n.encryptionMu.Unlock()

	cfg, err := n.readEncryptionConfig()
	if err != nil {
		return 0, false, err
	}
	if err := cfg.ValidateSecretbox(); err != nil {
		return 0, false, err
	}
	if cfg.SecretboxPrimaryIdentitySecondary() {
		return 0, false, nil
	}
	if !cfg.IdentityPrimarySecretboxSecondary() {
		return 0, false, errors.New("encryption config is not in identity-then-secretbox migration state; cannot promote")
	}
	reloadCountBefore, err := n.getEncryptionReloadSuccessCount()
	if err != nil {
		return 0, false, fmt.Errorf("cannot establish reload baseline (metrics unavailable): %w", err)
	}
	if err := cfg.PromoteSecretboxBeforeIdentity(); err != nil {
		return 0, false, err
	}
	configPath := filepath.Join(n.RootPath, "/etc/kubernetes/kube-api-encryption/encryption-config.yaml")
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return 0, false, fmt.Errorf("marshal encryption config: %w", err)
	}
	if err := atomicWriteFile(configPath, b, 0o600); err != nil {
		return 0, false, fmt.Errorf("write encryption config: %w", err)
	}
	return reloadCountBefore, true, nil
}

// PromoteKubernetesAPIEncryptionProviders moves secretbox before identity in the
// local encryption-config file (existing-cluster migration phase 2), then waits
// until the local apiserver reloads the config.
//
// Success returns an empty response and a nil error. Failures use gRPC status (see
// waitForEncryptionReload for DeadlineExceeded retry semantics), not response fields.
//
// Preconditions: phase 1 used InitializeKubernetesAPIEncryption (identity before secretbox)
// on every control-plane node (same key everywhere). Run on every control-plane node before
// StartEtcdEncryptionAtRestObjectsRewrite. Promotion is a no-op if config is already secretbox-first.
func (n *NodeAgentServer) PromoteKubernetesAPIEncryptionProviders(
	ctx context.Context,
	_ *pb.PromoteKubernetesAPIEncryptionProvidersRequest,
) (*pb.PromoteKubernetesAPIEncryptionProvidersResponse, error) {
	response := &pb.PromoteKubernetesAPIEncryptionProvidersResponse{}
	defer logRPCSpan(ctx, "PromoteKubernetesAPIEncryptionProviders")()

	if n.NodeStatus.Role != ControlPlaneRole {
		return response, errors.New("PromoteKubernetesAPIEncryptionProviders can only be run on controlplane nodes")
	}
	if n.clientRegistry == nil {
		return response, errors.New("apiserver metrics fetch is not configured")
	}

	reloadCountBefore, wroteConfig, err := n.commitPromoteEncryptionProvidersLocked()
	if err != nil {
		return response, err
	}
	if !wroteConfig {
		return response, nil
	}

	if R, metricErr := n.getEncryptionReloadSuccessCount(); metricErr == nil && R > reloadCountBefore {
		return response, nil
	}
	if err := n.waitForEncryptionReload(ctx, reloadCountBefore); err != nil {
		return response, err
	}
	return response, nil
}

// RemovePlainTextSecondaryEncryptionEntry removes the secondary kubernetes API encryption key
// RemovePlainTextSecondaryEncryptionEntry removes the default "identity: {}" entry after encyprtion
// initialization
// NOTE:
//   - This checks to ensure that the existing objects have been rewritten to use the new encryption key via labels
//   - This is only supported on controlplane nodes
//   - MUST ensure that the existing objects have been rewritten to use the new encryption key
//   - This needs to be run on EVERY controlplane node in the cluster!
//   - This is confused by some as suggesting that there is a secondary path to read secrets without an
//     encryption key; this is NOT the case.  This simply allows any plain-text secrets that were not
//     dated/rewritten to still be readable.  May be better to leave in place?
func (n *NodeAgentServer) RemovePlainTextSecondaryEncryptionEntry(
	ctx context.Context,
	req *pb.RemovePlainTextSecondaryEncryptionEntryRequest,
) (*pb.RemovePlainTextSecondaryEncryptionEntryResponse, error) {
	response := &pb.RemovePlainTextSecondaryEncryptionEntryResponse{}
	defer logRPCSpan(ctx, "RemovePlainTextSecondaryEncryptionEntry")()

	hostname, err := os.Hostname()
	if err != nil {
		log.Printf("failed to get hostname: %v\n", err)
		return response, err
	}

	hasLabel, err := k8s.NodeHasLabelOrMigrate(ctx, hostname, APIEncryptionRewriteCompleteLabel, "nke.io/k8s-api-encryption-rewrite-complete=true")
	if err != nil {
		response.ErrorMessage = fmt.Sprintf("failed to check node label: %v", err)
		return response, err
	}
	if !hasLabel {
		log.Printf(
			"node %s does not have required labels to remove plain text secondary entry from encryption-config.yaml...\n",
			hostname,
		)
		err := fmt.Errorf(
			"node does not have required labels to remove plain text secondary entry from encryption-config.yaml",
		)
		response.ErrorMessage = fmt.Sprintf(
			"node %s does not have required labels to remove plain text secondary entry from encryption-config.yaml",
			hostname,
		)
		response.Status = ErrorStatus
		return response, err
	}

	defer os.Remove("/tmp/encyrption-cfg-update.yaml")

	if n.clientRegistry == nil {
		slog.Error(
			"apiserver metrics fetch is not configured",
			"rpc", "RemovePlainTextSecondaryEncryptionEntry",
		)
		return response, status.Errorf(codes.FailedPrecondition, "apiserver metrics fetch is not configured")
	}
	reloadCountBefore, err := n.getEncryptionReloadSuccessCount()
	if err != nil {
		slog.Error(
			"cannot establish apiserver encryption reload baseline",
			"rpc", "RemovePlainTextSecondaryEncryptionEntry",
			"err", err,
		)
		return response, status.Errorf(codes.Unavailable, "cannot establish reload baseline (metrics unavailable): %v", err)
	}

	log.Printf("removing plain text secondary entry from encryption-config.yaml...\n")
	/*
			"grep",
			"-v",
			"\"identity: {}\"",
			"/etc/kubernetes/kube-api-encryption/encryption-config.yaml",
			">",
			"/tmp/encyrption-cfg-update.yaml",
		)
	*/
	grepCmd := "grep -v \"identity: {}\" /etc/kubernetes/kube-api-encryption/encryption-config.yaml > /tmp/encyrption-cfg-update.yaml"

	output, err := n.Runner.Run("bash", []string{"-c", grepCmd}, &nvkeutils.CommandOptions{}) //nolint:goconst // bash argv fragment; naming each CLI flag reads worse than the literal
	if err != nil {
		log.Printf("failed to remove secondary key entry: %v\n", err)
		log.Printf("output: %s\n", output)
		return response, err
	}

	log.Printf(
		"replace /etc/kubernetes/kube-api-encryption/encryption-config.yaml with updated file...\n",
	)

	output, err = n.Runner.Run(
		"mv",
		[]string{
			"/tmp/encyrption-cfg-update.yaml",
			"/etc/kubernetes/kube-api-encryption/encryption-config.yaml",
		},
		&nvkeutils.CommandOptions{},
	)
	if err != nil {
		log.Printf("failed to remove secondary key entry: %v\n", err)
		log.Printf("output: %s\n", output)
		return response, err
	}

	if reloadCount, metricErr := n.getEncryptionReloadSuccessCount(); metricErr == nil && reloadCount > reloadCountBefore {
		log.Printf("kube-apiserver encryption config reload confirmed\n")
	} else if err := n.waitForEncryptionReload(ctx, reloadCountBefore); err != nil {
		response.ErrorMessage = "timed out waiting for kube-apiserver to reload encryption config"
		return response, err
	}

	// Finally we can add the APIEncryptionEnabledLabel
	err = labelAPIEncryptionNode(ctx, n.NodeStatus.NodeName, APIEncryptionEnabledLabel)
	if err != nil {
		log.Printf("failed to add label: %v\n", err)
		return response, err
	}

	// Remove the other labels
	err = removeAPIEncryptionNodeLabel(ctx, n.NodeStatus.NodeName, APIEncryptionConfiguredLabel)
	if err != nil {
		log.Printf("failed to remove label: %v\n", err)
		return response, err
	}

	err = removeAPIEncryptionNodeLabel(ctx, n.NodeStatus.NodeName, APIEncryptionRewriteCompleteLabel)
	if err != nil {
		log.Printf("failed to remove label: %v\n", err)
		return response, err
	}

	return response, nil
}

/*
TBD
func getPodByLabel(label string) (string, error) {
	cmd := exec.Command(
		"kubectl",
		"get",
		"pod",
		"-l",
		label,
		"-o",
		"jsonpath={.items[0].metadata.name}",
	)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "KUBECONFIG=/etc/kubernetes/admin.conf")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("failed to get pod by label: %v\n", err)
		return "", err
	}

	return string(out), nil
}
*/

func (n *NodeAgentServer) getPodStatus(podName string) (string, error) {
	out, err := n.Runner.Run(
		"kubectl",
		[]string{
			kubectlNamespaceFlag,
			kubeSystemNamespace,
			kubectlGetCommand,
			"pod",
			podName,
			kubectlOutputFlag,
			"jsonpath={.status.phase}",
		},
		&nvkeutils.CommandOptions{
			Env: append(os.Environ(), "KUBECONFIG="+KubeConfig),
		},
	)
	if err != nil {
		log.Printf("failed to get pod status: %v\n", err)
		return "", err
	}

	return string(out), nil
}

func (n *NodeAgentServer) restartAPIServer() error {
	hostname, err := os.Hostname()
	if err != nil {
		log.Printf("failed to get hostname: %v\n", err)
		return err
	}

	podName := fmt.Sprintf("kube-apiserver-%s", hostname)
	log.Printf("attempting to restart api-server pod: %s\n", podName)

	_, err = n.Runner.Run(
		"kubectl",
		[]string{
			kubectlNamespaceFlag,
			kubeSystemNamespace,
			"delete",
			"pod",
			podName,
		},
		&nvkeutils.CommandOptions{
			Env: append(os.Environ(), "KUBECONFIG="+KubeConfig),
		},
	)
	if err != nil {
		logger.Errorf("failed to delete api-server pod: %v\n", err)
		return err
	}

	return nil
}

func (n *NodeAgentServer) apiServerPodIsRunning() (bool, error) {
	hostname, err := os.Hostname()
	if err != nil {
		log.Printf("failed to get hostname: %v\n", err)
		return false, err
	}

	apiServerPodName := fmt.Sprintf("kube-apiserver-%s", hostname)
	status, err := n.getPodStatus(apiServerPodName)
	if err != nil {
		log.Printf("failed to get api-server pod status: %v\n", err)
		return false, err
	}
	if status != "Running" {
		return false, nil
	}
	return true, nil
}

/*
func rewriteAPIObjects(objects []string) error {
	for _, object := range objects {
		cmd := exec.Command(
			"kubectl",
			"get",
			object,
			"--all-namespaces",
			kubectlOutputFlag,
			"json",
			"|",
			"kubectl",
			"replace",
			"-f",
			"-",
		)
		cmd.Env = os.Environ()
		cmd.Env = append(cmd.Env, "KUBECONFIG=/etc/kubernetes/admin.conf")
		_, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("failed to rewrite api objects: %v\n", err)
			return err
		}
	}

	return nil
}

// TBD
func removeSecondaryAPIEncryptionKey() error {
	// TODO
	return nil
}
*/

func (n *NodeAgentServer) createVolumeMountEntries() error {
	// Begin volume and mount entries for kube-apiserver.yaml...
	// Order matters here, set the volume mounts first as they'll be benign if the pod restarts
	log.Printf("add volume entry to kube-apiserver.yaml...\n")
	// Need to put in the new-line delims for sed to work properly...
	volumeEntry = strings.ReplaceAll(volumeEntry, "\n", "\\n")
	_, err := n.Runner.Run(
		"bash",
		[]string{
			"-c",
			fmt.Sprintf(
				`sed -i '/volumes:/a \ \ %s' /etc/kubernetes/manifests/kube-apiserver.yaml`,
				volumeEntry,
			),
		},
		&nvkeutils.CommandOptions{},
	)
	if err != nil {
		log.Printf("Failed to add volumeEntry: %v\n", err)
		return err
	}

	log.Printf("add volume mount entry to kube-apiserver.yaml...\n")
	volumeMountsEntry = strings.ReplaceAll(volumeMountsEntry, "\n", "\\n")
	_, err = n.Runner.Run(
		"bash",
		[]string{
			"-c",
			fmt.Sprintf(
				`sed -i '/volumeMounts:/a \ \ \ \ %s' /etc/kubernetes/manifests/kube-apiserver.yaml`,
				volumeMountsEntry,
			),
		},
		&nvkeutils.CommandOptions{},
	)
	if err != nil {
		log.Printf("failed to add volumeMounts entry: %v\n", err)
		return err
	}
	return nil
}

func (n *NodeAgentServer) modifyAPIServerManifest() error {
	// Begin add encryption-config to kube-apiserver.yaml...
	log.Printf("add encryption-config to kube-apiserver.yaml...\n")
	_, err := n.Runner.Run(
		"bash",
		[]string{
			"-c",
			`sed -i '/- kube-apiserver/a \ \ \ \ - --encryption-provider-config=/etc/kubernetes/kube-api-encryption/encryption-config.yaml' /etc/kubernetes/manifests/kube-apiserver.yaml`,
		},
		&nvkeutils.CommandOptions{},
	)
	if err != nil {
		log.Printf("failed to add --encryption-provider-config: %v\n", err)
		return err
	}

	log.Printf("add encryption-config monitoring to kube-apiserver.yaml...\n")
	_, err = n.Runner.Run(
		"bash",
		[]string{
			"-c",
			`sed -i '/- kube-apiserver/a \ \ \ \ - --encryption-provider-config-automatic-reload=true' /etc/kubernetes/manifests/kube-apiserver.yaml`,
		},
		&nvkeutils.CommandOptions{},
	)
	if err != nil {
		log.Printf("failed to add --encryption-provider-config-automatic-reload: %v\n", err)
		return err
	}
	// End add encryption-config to kube-apiserver.yaml...
	log.Printf(
		"encryption-config added to kube-apiserver on this node, api pod should restart within a few minutes\n",
	)

	return nil
}

// identityFirst selects the migration template (identity then secretbox). Kubeadm init uses false
// (secretbox then identity). objects, if non-empty, overrides the default resource kinds list.
func (n *NodeAgentServer) createAPIEncryptionConfig(encryptionKey string, identityFirst bool, objects []string) error {
	return n.createNamedAPIEncryptionConfig(encryptionKey, "", identityFirst, objects)
}

func (n *NodeAgentServer) createNamedAPIEncryptionConfig(encryptionKey, keyName string, identityFirst bool, objects []string) error {
	return n.writeAPIEncryptionConfig(encryptionKey, keyName, identityFirst, objects, false)
}

// createBootstrapAPIEncryptionConfig writes the encryption config for kubeadm init or join.
// kubeadm reset leaves /etc/kubernetes/kube-api-encryption behind, so a config that no local
// etcd data or kube-apiserver manifest still depends on is replaced rather than rejected.
func (n *NodeAgentServer) createBootstrapAPIEncryptionConfig(encryptionKey, keyName string) error {
	return n.writeAPIEncryptionConfig(encryptionKey, keyName, false, nil, true)
}

// localEncryptedDataMayExist reports whether this node still holds etcd member data or a
// kube-apiserver static pod manifest, the state kubeadm reset removes. The agent cannot see a
// non-default etcd dataDir, so it checks kubeadm's default /var/lib/etcd.
func (n *NodeAgentServer) localEncryptedDataMayExist() (bool, error) {
	if _, err := os.Stat(filepath.Join(n.RootPath, "/etc/kubernetes/manifests/kube-apiserver.yaml")); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	entries, err := os.ReadDir(filepath.Join(n.RootPath, "/var/lib/etcd/member"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

func (n *NodeAgentServer) writeAPIEncryptionConfig(encryptionKey, keyName string, identityFirst bool, objects []string, bootstrap bool) error {
	n.encryptionMu.Lock()
	defer n.encryptionMu.Unlock()
	if keyName == "" {
		keyName = legacyAPIEncryptionKeyName
	}
	if err := validateEncryptionKeyName(keyName); err != nil {
		return err
	}
	// A retry may encounter an existing configuration. Matching key bytes do
	// not permit changing its live storage name without rewriting the objects.
	configPath := filepath.Join(n.RootPath, "/etc/kubernetes/kube-api-encryption/encryption-config.yaml")
	if _, err := os.Stat(configPath); err == nil {
		cfg, err := n.readEncryptionConfig()
		if err != nil {
			return err
		}
		identityErr := cfg.validateMutationIdentity(&pb.UpdateEncryptionConfigRequest{
			Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{
				AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: encryptionKey, KeyName: keyName},
			},
		})
		if identityErr != nil {
			if !bootstrap {
				return identityErr
			}
			protected, err := n.localEncryptedDataMayExist()
			if err != nil {
				return fmt.Errorf("checking for local etcd data before replacing encryption config: %w", err)
			}
			if protected {
				return identityErr
			}
			var staleDigests []string
			for _, keys := range cfg.secretboxKeySlices() {
				for _, key := range *keys {
					staleDigests = append(staleDigests, encryptionKeyDigest(key.Secret))
				}
			}
			slog.Warn("replacing stale encryption config left without local etcd data",
				"config_path", configPath,
				"key_name", keyName,
				"key_sha256", encryptionKeyDigest(encryptionKey),
				"stale_key_sha256s", staleDigests,
			)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	encryptionDir := filepath.Join(n.RootPath, "/etc/kubernetes/kube-api-encryption")
	err := os.MkdirAll(encryptionDir, 0o755)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	tmplStr := encryptionConfigTemplate
	if identityFirst {
		tmplStr = encryptionConfigTemplateExistingCluster
	}
	// Mark up encryption config with objects to encrypt and encryption key
	log.Printf("parse encryption config template...\n")
	tmpl, err := template.New("yaml").Parse(tmplStr)
	if err != nil {
		log.Printf("Error parsing template: %v", err)
		return err
	}
	objs := objectsToEncrypt
	if len(objects) > 0 {
		objs = objects
	}
	templateValues := struct {
		ObjectsToEncrypt  []string
		EncryptionKey     string
		EncryptionKeyName string
	}{
		ObjectsToEncrypt:  objs,
		EncryptionKey:     encryptionKey,
		EncryptionKeyName: keyName,
	}
	var encryptionConfig strings.Builder
	err = tmpl.Execute(&encryptionConfig, templateValues)
	if err != nil {
		log.Printf("Error executing template: %v", err)
		return err
	}

	// Write the marked up encryption config to file
	log.Printf("write encryption config file...\n")
	_, err = f.WriteString(encryptionConfig.String())
	if err != nil {
		log.Printf("Error writing encryption config file: %v", err)
		return err
	}
	return nil
}

func (n *NodeAgentServer) verifyNodeAPIEncryptionEnabled() (bool, error) {
	hostname, err := os.Hostname()
	if err != nil {
		log.Printf("failed to get hostname: %v\n", err)
		return false, err
	}

	apiServerPodName := fmt.Sprintf("kube-apiserver-%s", hostname)

	out, err := n.Runner.Run(
		"kubectl",
		[]string{
			kubectlNamespaceFlag,
			kubeSystemNamespace,
			"describe",
			"po",
			apiServerPodName,
		},
		&nvkeutils.CommandOptions{
			Env: append(os.Environ(), "KUBECONFIG="+KubeConfig),
		},
	)
	if err != nil {
		log.Printf("failed to get api-server pod info: %v\n", err)
		return false, err
	}
	if strings.Contains(string(out), "--encryption-provider-config=") {
		return true, nil
	}
	return false, nil
}

func (n *NodeAgentServer) updateEncryptionEntryForKubeadmConfigMap() error {
	cm, err := k8s.GetConfigMap(context.Background(), kubeSystemNamespace, kubeadmConfigMapName)
	if err != nil {
		log.Printf("failed to retrieve kubeadm-config configmap: %v\n", err)
		return err
	}

	if cm == nil {
		log.Printf("kubeadm-config configmap not found\n")
		return errors.New("kubeadm-config configmap not found")
	}

	clusterYAML := cm.Data["ClusterConfiguration"]

	var configMap map[string]any
	if err := yaml.Unmarshal([]byte(clusterYAML), &configMap); err != nil {
		log.Printf("error unmarshaling kubeadm config: %v", err)
		return err
	}

	// Ensure apiServer entry exists
	apiServer, ok := configMap["apiServer"].(map[string]any)
	if !ok {
		apiServer = make(map[string]any)
		configMap["apiServer"] = apiServer
	}

	apiServer["extraVolumes"] = []any{
		map[string]any{
			"hostPath":  "/etc/kubernetes/encryption-config.yaml",
			"mountPath": "/etc/kubernetes/encryption-config.yaml",
			"name":      "encryption-config",
			"pathType":  "File",
			"readOnly":  true,
		},
	}

	modifiedYAML, err := yaml.Marshal(configMap)
	if err != nil {
		log.Printf("error marshaling kubeadm config: %v", err)
		return err
	}

	cm.Data["ClusterConfiguration"] = string(modifiedYAML)

	err = k8s.UpdateConfigMap(context.Background(), kubeSystemNamespace, cm)
	if err != nil {
		log.Printf("error applying updated kubeadm-config configmap: %v", err)
		return err
	}

	return nil
}

// labelAPIEncryptionNode writes the new key and prunes its legacy counterpart.
func labelAPIEncryptionNode(ctx context.Context, node, label string) error {
	if err := k8s.LabelNode(ctx, node, label); err != nil {
		return err
	}
	return k8s.RemoveNodeLabel(ctx, node, strings.Replace(label, "nvke.nvidia.com/", "nke.io/", 1))
}

func removeAPIEncryptionNodeLabel(ctx context.Context, node, label string) error {
	return errors.Join(
		k8s.RemoveNodeLabel(ctx, node, label),
		k8s.RemoveNodeLabel(ctx, node, strings.Replace(label, "nvke.nvidia.com/", "nke.io/", 1)),
	)
}
