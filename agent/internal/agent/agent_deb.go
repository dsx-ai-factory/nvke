// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	"github.com/dsx-ai-factory/nvke/agent/pkg/deb"
)

func (s *NodeAgentServer) InstallPackages(ctx context.Context, req *pb.InstallPackagesRequest) (*pb.InstallPackagesResponse, error) {
	defer logRPCSpan(ctx, "InstallPackages")()
	if len(req.GetPackages()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no packages specified")
	}
	for _, pkg := range req.GetPackages() {
		if isNodeAgentPackageArgument(pkg) {
			return nil, status.Error(codes.FailedPrecondition, "use StartNodeAgentUpgrade to replace nke-agent")
		}
	}

	requestID := generateRequestID()
	pkgStatus := &Bundle{
		RequestID:   requestID,
		Name:        "deb-packages",
		Status:      InstallingStatus,
		InstallTime: time.Now().Format(time.RFC3339),
		Type:        "deb-packages",
		Output:      strings.Join(req.GetPackages(), ", "),
	}

	defer s.PersistNodeStatus()
	s.AppendBundleToStatus(pkgStatus)

	if req.GetAsync() {
		go func() {
			if err := s.processInstallPackages(context.Background(), req, pkgStatus); err != nil {
				slog.Error("error installing packages async", "request_id", requestID, "err", err)
			}
		}()
		return &pb.InstallPackagesResponse{RequestId: requestID}, nil
	}

	if err := s.processInstallPackages(ctx, req, pkgStatus); err != nil {
		return nil, err
	}
	return &pb.InstallPackagesResponse{RequestId: requestID}, nil
}

func isNodeAgentPackageArgument(value string) bool {
	value = strings.TrimSpace(value)
	packageName, _, _ := strings.Cut(value, "=")
	packageName, _, _ = strings.Cut(packageName, ":")
	packageName, _, _ = strings.Cut(packageName, "/")
	if packageName == "nke-agent" {
		return true
	}
	base := filepath.Base(value)
	return strings.HasPrefix(base, "nke-agent_") && strings.HasSuffix(base, ".deb")
}

func (s *NodeAgentServer) processInstallPackages(ctx context.Context, req *pb.InstallPackagesRequest, pkgStatus *Bundle) error {
	defer s.PersistNodeStatus()
	defer s.UpdateBundleStatus(pkgStatus)

	if req.GetUpdate() {
		if err := deb.Update(ctx); err != nil {
			slog.ErrorContext(ctx, "apt-get update failed", "error", err)
			pkgStatus.Status = ErrorStatus
			pkgStatus.Error = fmt.Sprintf("update error: %v", err)
			return status.Errorf(codes.Internal, "update error: %v", err)
		}
	}

	if err := deb.Install(ctx, req.GetPackages()...); err != nil {
		slog.ErrorContext(ctx, "package installation failed", "packages", req.GetPackages(), "error", err)
		pkgStatus.Status = ErrorStatus
		pkgStatus.Error = err.Error()
		return status.Errorf(codes.Internal, "install error: %v", err)
	}

	s.refreshNodeStatusInstalledPackages(ctx)
	pkgStatus.Status = SuccessStatus
	slog.InfoContext(ctx, "packages installed successfully", "packages", req.GetPackages(), "request_id", pkgStatus.RequestID)
	return nil
}

func (s *NodeAgentServer) ListPackages(ctx context.Context, req *pb.ListPackagesRequest) (*pb.ListPackagesResponse, error) {
	defer logRPCSpan(ctx, "ListPackages")()
	iter, err := deb.List(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list packages", "error", err)
		return nil, status.Errorf(codes.Internal, "list error: %v", err)
	}

	var packages []*pb.InstalledPackage
	for p := range iter {
		packages = append(packages, &pb.InstalledPackage{
			Name:    p.Name,
			Version: p.Version,
			Status:  p.Status,
		})
	}

	return &pb.ListPackagesResponse{Packages: packages}, nil
}

func (s *NodeAgentServer) RemovePackages(ctx context.Context, req *pb.RemovePackagesRequest) (*pb.RemovePackagesResponse, error) {
	defer logRPCSpan(ctx, "RemovePackages")()
	if len(req.GetPackages()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no packages specified")
	}

	if err := deb.Remove(ctx, req.GetPurge(), req.GetPackages()...); err != nil {
		slog.ErrorContext(ctx, "package removal failed", "packages", req.GetPackages(), "error", err)
		return nil, status.Errorf(codes.Internal, "remove error: %v", err)
	}

	s.refreshNodeStatusInstalledPackages(ctx)

	return &pb.RemovePackagesResponse{}, nil
}

func (s *NodeAgentServer) refreshNodeStatusInstalledPackages(ctx context.Context) {
	iter, err := deb.List(ctx)
	if err != nil {
		slog.WarnContext(ctx, "failed to refresh installed package list in node status", "error", err)
		return
	}
	list := make([]InstalledDebPackage, 0)
	for p := range iter {
		list = append(list, InstalledDebPackage{
			Name:    p.Name,
			Version: p.Version,
			Status:  p.Status,
		})
	}
	s.mu.Lock()
	s.NodeStatus.InstalledPackages = list
	ns := *s.NodeStatus
	s.mu.Unlock()
	if err := s.Persister.Persist(&ns); err != nil {
		slog.WarnContext(ctx, "failed to persist node status after refreshing package list", "error", err)
	}
}

func (s *NodeAgentServer) ConfigureAptRepo(ctx context.Context, req *pb.ConfigureAptRepoRequest) (*pb.ConfigureAptRepoResponse, error) {
	defer logRPCSpan(ctx, "ConfigureAptRepo")()
	raw, err := deb.ConfigureAptRepo(ctx, deb.AptRepoConfig{
		KeyringName:         req.GetKeyringName(),
		RepoName:            req.GetRepoName(),
		SigningKey:          req.GetRepoSigningKey(),
		RepoPriority:        req.GetRepoPriority(),
		DebURI:              req.GetDebUri(),
		Suite:               req.GetSuite(),
		Components:          req.GetComponents(),
		SourcesListBasename: req.GetSourcesListBasename(),
	})
	if err != nil {
		slog.ErrorContext(ctx, "configure apt repo failed", "keyring_name", req.GetKeyringName(), "repo_name", req.GetRepoName(), "error", err)
		return &pb.ConfigureAptRepoResponse{
			Status:       ErrorStatus,
			ErrorMessage: err.Error(),
			RawOutput:    raw,
		}, nil
	}

	return &pb.ConfigureAptRepoResponse{
		Status:    ReadyStatus,
		RawOutput: raw,
	}, nil
}

func (s *NodeAgentServer) RemoveAptRepo(ctx context.Context, req *pb.RemoveAptRepoRequest) (*pb.RemoveAptRepoResponse, error) {
	defer logRPCSpan(ctx, "RemoveAptRepo")()
	raw, err := deb.RemoveAptRepo(ctx, req.GetKeyringName(), req.GetSourcesListBasename())
	if err != nil {
		slog.ErrorContext(ctx, "remove apt repo failed", "keyring_name", req.GetKeyringName(), "sources_list_basename", req.GetSourcesListBasename(), "error", err)
		return &pb.RemoveAptRepoResponse{
			Status:       ErrorStatus,
			ErrorMessage: err.Error(),
			RawOutput:    raw,
		}, nil
	}

	return &pb.RemoveAptRepoResponse{
		Status:    ReadyStatus,
		RawOutput: raw,
	}, nil
}
