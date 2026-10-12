// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const (
	kubeletKubeConfigPath = "/etc/kubernetes/kubelet.conf"
	kubeletServiceName    = "kubelet"
)

// ReconfigureKubeletAPIServer atomically updates the kubelet API endpoint,
// restarts kubelet, and verifies that the service is active. A failure after
// mutation triggers restoration of both the original config and kubelet.
func (s *NodeAgentServer) ReconfigureKubeletAPIServer(
	ctx context.Context,
	req *pb.ReconfigureKubeletAPIServerRequest,
) (*pb.ReconfigureKubeletAPIServerResponse, error) {
	defer logRPCSpan(ctx, "ReconfigureKubeletAPIServer")()

	server := req.GetServer()
	if err := validateKubeletAPIServer(server); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid server: %v", err)
	}

	s.kubeletConfigMu.Lock()
	defer s.kubeletConfigMu.Unlock()

	configPath := s.resolveFilePath(kubeletKubeConfigPath)
	original, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, status.Errorf(codes.FailedPrecondition, "kubelet config %s does not exist", kubeletKubeConfigPath)
		}
		return nil, status.Errorf(codes.Internal, "read kubelet config %s: %v", kubeletKubeConfigPath, err)
	}

	config, cluster, err := loadKubeletConfig(original)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "invalid kubelet config %s: %v", kubeletKubeConfigPath, err)
	}

	changed := cluster.Server != server
	if changed {
		cluster.Server = server
		updated, marshalErr := clientcmd.Write(*config)
		if marshalErr != nil {
			return nil, status.Errorf(codes.Internal, "marshal kubelet config: %v", marshalErr)
		}
		if writeErr := s.writeKubeletConfig(configPath, updated); writeErr != nil {
			return nil, status.Errorf(codes.Internal, "write kubelet config atomically: %v", writeErr)
		}
	}

	if serviceErr := s.restartAndVerifyKubelet(); serviceErr != nil {
		if !changed {
			slog.Error("failed to restart kubelet with unchanged config", "server", server, "err", serviceErr)
			return &pb.ReconfigureKubeletAPIServerResponse{
				Outcome:      pb.ReconfigureKubeletAPIServerResponse_OUTCOME_FAILED_NOT_RESTORED,
				ErrorMessage: serviceErr.Error(),
			}, nil
		}
		return s.restoreKubeletConfig(configPath, original, serviceErr), nil
	}

	outcome := pb.ReconfigureKubeletAPIServerResponse_OUTCOME_UNCHANGED
	if changed {
		outcome = pb.ReconfigureKubeletAPIServerResponse_OUTCOME_APPLIED
	}
	slog.Info("reconfigured kubelet API server", "server", server, "changed", changed)
	return &pb.ReconfigureKubeletAPIServerResponse{Outcome: outcome}, nil
}

func validateKubeletAPIServer(server string) error {
	if server == "" || strings.TrimSpace(server) != server {
		return errors.New("must be set without surrounding whitespace")
	}

	parsed, err := url.ParseRequestURI(server)
	if err != nil {
		return fmt.Errorf("must be a valid URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return errors.New("scheme must be https")
	}
	if parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("must have the form https://<host-or-ip>:6443")
	}
	if parsed.Port() != "6443" {
		return errors.New("port must be 6443")
	}

	host := parsed.Hostname()
	if host == "" {
		return errors.New("host is required")
	}
	if net.ParseIP(host) == nil {
		if problems := validation.IsDNS1123Subdomain(host); len(problems) != 0 {
			return fmt.Errorf("host must be an IP address or DNS name: %s", strings.Join(problems, ", "))
		}
	}

	return nil
}

func loadKubeletConfig(contents []byte) (*clientcmdapi.Config, *clientcmdapi.Cluster, error) {
	config, err := clientcmd.Load(contents)
	if err != nil {
		return nil, nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	if config.CurrentContext == "" {
		return nil, nil, errors.New("current-context is required")
	}
	currentContext, ok := config.Contexts[config.CurrentContext]
	if !ok || currentContext == nil || currentContext.Cluster == "" {
		return nil, nil, fmt.Errorf("current-context %q does not reference a cluster", config.CurrentContext)
	}
	cluster, ok := config.Clusters[currentContext.Cluster]
	if !ok || cluster == nil {
		return nil, nil, fmt.Errorf("cluster %q referenced by current-context does not exist", currentContext.Cluster)
	}
	return config, cluster, nil
}

func (s *NodeAgentServer) writeKubeletConfig(path string, contents []byte) error {
	write := s.kubeletConfigAtomicWrite
	if write == nil {
		write = atomicWriteFile
	}
	return write(path, contents, 0o600)
}

func (s *NodeAgentServer) restartAndVerifyKubelet() error {
	var errs []error
	if _, err := s.Runner.Run("systemctl", []string{"restart", kubeletServiceName}, nil); err != nil {
		errs = append(errs, fmt.Errorf("restart kubelet: %w", err))
	}
	if _, err := s.Runner.Run("systemctl", []string{"is-active", "--quiet", kubeletServiceName}, nil); err != nil {
		errs = append(errs, fmt.Errorf("verify kubelet is active: %w", err))
	}
	return errors.Join(errs...)
}

func (s *NodeAgentServer) restoreKubeletConfig(
	configPath string,
	original []byte,
	reconfigurationErr error,
) *pb.ReconfigureKubeletAPIServerResponse {
	restoreErr := s.writeKubeletConfig(configPath, original)
	serviceErr := s.restartAndVerifyKubelet()

	errParts := []string{reconfigurationErr.Error()}
	outcome := pb.ReconfigureKubeletAPIServerResponse_OUTCOME_FAILED_RESTORED
	if restoreErr != nil {
		outcome = pb.ReconfigureKubeletAPIServerResponse_OUTCOME_FAILED_NOT_RESTORED
		errParts = append(errParts, fmt.Sprintf("restore original kubelet config: %v", restoreErr))
	}
	if serviceErr != nil {
		outcome = pb.ReconfigureKubeletAPIServerResponse_OUTCOME_FAILED_NOT_RESTORED
		errParts = append(errParts, fmt.Sprintf("restore kubelet service: %v", serviceErr))
	}

	slog.Error(
		"failed to reconfigure kubelet API server",
		"outcome", outcome.String(),
		"err", strings.Join(errParts, "; "),
	)
	return &pb.ReconfigureKubeletAPIServerResponse{
		Outcome:      outcome,
		ErrorMessage: strings.Join(errParts, "; "),
	}
}
