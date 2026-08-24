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

	"github.com/NVIDIA/nke/agent/internal/k8s"
	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
	nkeutils "github.com/NVIDIA/nke/agent/pkg/utils"
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

	APIEncryptionConfiguredLabel          string = "nke.io/k8s-api-encryption-configured=true"
	APIEncryptionRewriteCompleteLabel     string = "nke.io/k8s-api-encryption-rewrite-complete=true"
	APIEncryptionEnabledLabel             string = "nke.io/k8s-api-encryption-enabled=true"
	APIEncryptionSecondaryKeyEnabledLabel string = "nke.io/k8s-api-encryption-secondary-key-configured=true"
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
		"secrets",    //nolint:goconst // resource plural listed once; the literal table is clearer
		"configmaps", //nolint:goconst // resource plural listed once; the literal table is clearer
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
            - name: current-primary-encryption-key
              secret: {{ .EncryptionKey }}
      - identity: {}`

	// identity first so new writes stay plaintext in etcd until every control-plane
	// apiserver has the same key, then PromoteKubernetesAPIEncryptionProviders and RewriteKubernetesAPIObjects.
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
            - name: current-primary-encryption-key
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
//     PromoteKubernetesAPIEncryptionProviders on each node, then RewriteKubernetesAPIObjects.
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
	err = k8s.LabelNode(ctx, s.NodeStatus.NodeName, APIEncryptionConfiguredLabel)
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
		err = s.createAPIEncryptionConfig(req.EncryptionKey, true, req.ObjectsToEncrypt)
		if err != nil {
			log.Printf("failed to initialize api encryption config: %v\n", err)
			s.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionErrorStatus
			response.ErrorMessage = "failed to initialize api encryption config"
			log.Printf("removing the configured label\n")
			err = k8s.RemoveNodeLabel(ctx, s.NodeStatus.NodeName, APIEncryptionConfiguredLabel)
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
			err = k8s.RemoveNodeLabel(ctx, s.NodeStatus.NodeName, APIEncryptionConfiguredLabel)
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
			err = k8s.RemoveNodeLabel(ctx, s.NodeStatus.NodeName, APIEncryptionConfiguredLabel)
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
		// which immediately calls /metrics on the local apiserver. See HIPPO-9454.
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
// RewriteKubernetesAPIObjects (no-op if config is already secretbox-first).
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

// RewriteKubernetesAPIObjects rewrites the selected kubernetes API objects
// to use the new encryption key.
// NOTE:
// * This is only supported on controlplane nodes, and only needs to be run on a single controlplane node!!!
// * This will only work if the encryption key has been updated on ALL controlplane nodes

func (n *NodeAgentServer) RewriteKubernetesAPIObjects(
	ctx context.Context,
	req *pb.RewriteKubernetesAPIObjectsRequest,
) (*pb.RewriteKubernetesAPIObjectsResponse, error) {
	response := &pb.RewriteKubernetesAPIObjectsResponse{}
	defer logRPCSpan(ctx, "RewriteKubernetesAPIObjects")()

	if n.NodeStatus.Role != ControlPlaneRole {
		log.Printf("rewrite objects only set on controlplane nodes")
		return response, errors.New("unable to rewrite kube api objects on non-controlplane node")
	}

	hostname, err := os.Hostname()
	if err != nil {
		log.Printf("failed to get hostname: %v\n", err)
		response.ErrorMessage = "failed to get hostname"
		n.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionErrorStatus
		return response, err
	}
	n.NodeStatus.NodeName = hostname

	// Verify all cp nodes are configured
	cpNodes, err := k8s.GetNodeNamesByLabel(ctx, "node-role.kubernetes.io/control-plane=")
	if err != nil {
		log.Printf("failed to get control-plane nodes: %v\n", err)
		response.ErrorMessage = "failed to get control-plane nodes"
		n.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionErrorStatus
		return response, err
	}

	if req.SkipLabelCheck {
		log.Printf("WARNING: RewriteKubernetesAPIObjects called with skip_label_check=true — " +
			"skipping node label validation; ensure this is an intentional key-rotation call\n")
	} else {
		for _, node := range cpNodes {
			hasLabel, err := k8s.NodeHasLabel(ctx, node, APIEncryptionConfiguredLabel)
			if err != nil {
				log.Printf("failed to get control-plane node %s label: %v\n", node, err)
			}
			if !hasLabel {
				log.Printf("control-plane node %s encryption is not configured", node)
				response.ErrorMessage = "control-plane node encryption is not configured"
				n.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionErrorStatus
				return response, errors.New(
					"node encryption is not configured, can't rewrite",
				)
			}
		}
	}

	log.Printf("rewriting api objects...\n")
	/*
		kcmd := "kubectl get secrets -A -o json | kubectl replace -f -"
		cmd := exec.Command(
			"bash",
			"-c",
			kcmd,
		)
		cmd.Env = os.Environ()
		cmd.Env = append(cmd.Env, "KUBECONFIG=/etc/kubernetes/admin.conf")
		output, err := cmd.CombinedOutput()
	*/

	log.Printf("rewriting api objects (client-go, kinds: %v)\n", objectsToEncrypt)
	if err := k8s.RewriteEncryptableAPIObjects(ctx, objectsToEncrypt); err != nil {
		log.Printf("failed to rewrite api objects: %v\n", err)
		return response, err
	}

	log.Printf("cpnodes for labeling: %v\n", cpNodes)
	for _, node := range cpNodes {
		if node != "" {
			log.Printf("labeling node %s\n", node)
			err := k8s.LabelNode(ctx, node, APIEncryptionRewriteCompleteLabel)
			if err != nil {
				log.Printf("failed to label node %s: %v\n", node, err)
				response.ErrorMessage = "failed to label node"
				n.NodeStatus.K8sAPIEncryption.Status = K8sAPIEncryptionErrorStatus
				return response, err
			}
		}
	}
	return response, nil
}

// RestartAPIPods restarts the kube-apiserver pods for all cp nodes in the cluster.
func (n *NodeAgentServer) RestartAPIPods(
	ctx context.Context,
	req *pb.RestartAPIPodsRequest,
) (*pb.RestartAPIPodsResponse, error) {
	response := &pb.RestartAPIPodsResponse{}
	defer logRPCSpan(ctx, "RestartAPIPods")()
	// Get a lit of all apiserver pods....
	//   kubectl get pods --no-headers -o custom-columns=":metadata.name" -n kube-system -l component=kube-apiserver

	output, err := n.Runner.Run(
		"kubectl",
		[]string{
			"get",
			"pods", //nolint:goconst // kubectl argv fragment; naming each CLI flag reads worse than the literal
			"--no-headers",
			"-o", //nolint:goconst // kubectl argv fragment; naming each CLI flag reads worse than the literal
			"custom-columns=:metadata.name",
			"-n",          //nolint:goconst // kubectl argv fragment; naming each CLI flag reads worse than the literal
			"kube-system", //nolint:goconst // kubectl argv; kept literal so the arg list reads as the command line
			"-l",
			"component=kube-apiserver",
		},
		&nkeutils.CommandOptions{
			Env: append(os.Environ(), "KUBECONFIG="+KubeConfig),
		},
	)
	if err != nil {
		log.Printf("failed to get api-server pods: %v\n", err)
		log.Printf("output: %s\n", output)
		return response, err
	}

	// restart each apiserver pod
	for _, podName := range strings.Split(string(output), "\n") {
		output, err := n.Runner.Run(
			"kubectl",
			[]string{"delete", "pod", podName, "-n", "kube-system"}, //nolint:goconst // kubectl argv fragment; naming each CLI flag reads worse than the literal
			&nkeutils.CommandOptions{
				Env: append(os.Environ(), "KUBECONFIG="+KubeConfig),
			},
		)
		if err != nil {
			log.Printf("failed to delete api-server pod: %v\n", err)
			log.Printf("output: %s\n", output)
			return response, err
		}
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

	hasLabel, err := k8s.NodeHasLabel(ctx, hostname, APIEncryptionRewriteCompleteLabel)
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

	output, err := n.Runner.Run("bash", []string{"-c", grepCmd}, &nkeutils.CommandOptions{}) //nolint:goconst // bash argv fragment; naming each CLI flag reads worse than the literal
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
		&nkeutils.CommandOptions{},
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
	err = k8s.LabelNode(ctx, n.NodeStatus.NodeName, APIEncryptionEnabledLabel)
	if err != nil {
		log.Printf("failed to add label: %v\n", err)
		return response, err
	}

	// Remove the other labels
	err = k8s.RemoveNodeLabel(ctx, n.NodeStatus.NodeName, APIEncryptionConfiguredLabel)
	if err != nil {
		log.Printf("failed to remove label: %v\n", err)
		return response, err
	}

	err = k8s.RemoveNodeLabel(ctx, n.NodeStatus.NodeName, APIEncryptionRewriteCompleteLabel)
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
			"-n",
			"kube-system",
			"get",
			"pod",
			podName,
			"-o",
			"jsonpath={.status.phase}",
		},
		&nkeutils.CommandOptions{
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
			"-n",
			"kube-system",
			"delete",
			"pod",
			podName,
		},
		&nkeutils.CommandOptions{
			Env: append(os.Environ(), "KUBECONFIG="+KubeConfig),
		})
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
			"-o",
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
		&nkeutils.CommandOptions{},
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
		&nkeutils.CommandOptions{},
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
		&nkeutils.CommandOptions{},
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
		&nkeutils.CommandOptions{},
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
	encryptionDir := filepath.Join(n.RootPath, "/etc/kubernetes/kube-api-encryption")
	err := os.MkdirAll(encryptionDir, 0o755)
	if err != nil {
		return err
	}

	configPath := filepath.Join(encryptionDir, "encryption-config.yaml")
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
		ObjectsToEncrypt []string
		EncryptionKey    string
	}{
		ObjectsToEncrypt: objs,
		EncryptionKey:    encryptionKey,
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
			"-n",
			"kube-system",
			"describe",
			"po",
			apiServerPodName,
		},
		&nkeutils.CommandOptions{
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
