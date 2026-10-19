// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const (
	spireAgentConfigPath          = "/etc/spire-agent/agent.conf"
	spireAgentEnvPath             = "/etc/nke/spire.env"
	spireAgentServiceName         = "spire-agent"
	defaultSpireAgentPollInterval = 2 * time.Second
	defaultSpireAgentWaitTimeout  = 3 * time.Minute
	spireSVIDFetchTimeout         = 5 * time.Second
	systemctlShow                 = "show"
)

var (
	spireServerAddressLine = regexp.MustCompile(`(?m)^([ \t]*server_address[ \t]*=[ \t]*)(.*)$`)
	spireServerPortLine    = regexp.MustCompile(`(?m)^([ \t]*server_port[ \t]*=[ \t]*)(.*)$`)
	spireEnvAddressLine    = regexp.MustCompile(`(?m)^SPIRE_SERVER_ADDRESS=.*$`)
	spireEnvPortLine       = regexp.MustCompile(`(?m)^SPIRE_SERVER_PORT=.*$`)
	spireAgentShowArgs     = []string{
		systemctlShow, spireAgentServiceName, "--property=ActiveState", "--property=ActiveEnterTimestamp", "--timestamp=unix",
	}
)

// ReconfigureSpireAgent points the node's SPIRE agent at a new SPIRE server,
// restarts it, and waits until it serves a valid X.509 SVID. Operational
// failures are reported as OUTCOME_FAILED with a nil gRPC error.
func (s *NodeAgentServer) ReconfigureSpireAgent(
	ctx context.Context,
	req *pb.ReconfigureSpireAgentRequest,
) (*pb.ReconfigureSpireAgentResponse, error) {
	defer logRPCSpan(ctx, "ReconfigureSpireAgent")()

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	address, port := req.GetServerAddress(), req.GetServerPort()
	if err := validateSpireServerAddress(address); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid server_address: %v", err)
	}
	if port == 0 || port > 65535 {
		return nil, status.Errorf(codes.InvalidArgument, "invalid server_port %d: must be 1-65535", port)
	}
	if s.SPIFFESocketPath == "" {
		return nil, status.Error(codes.FailedPrecondition, "SPIFFE workload API socket is not configured; cannot verify the SPIRE agent's SVID")
	}

	s.spireAgentMu.Lock()
	defer s.spireAgentMu.Unlock()
	// A request that waited behind another one must not change the node after its caller gave up.
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}

	confPath := s.resolveFilePath(spireAgentConfigPath)
	conf, err := readSpireAgentConfig(confPath)
	if err != nil {
		return nil, err
	}
	currentAddress, currentPort, err := parseSpireAgentServer(conf.content)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "invalid SPIRE agent config %s: %v", spireAgentConfigPath, err)
	}

	envPath := s.resolveFilePath(spireAgentEnvPath)
	env, err := capturePriorFileIfExists(envPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read SPIRE env file %s: %v", spireAgentEnvPath, err)
	}

	if currentAddress == address && currentPort == port {
		current, statusErr := s.spireAgentStatus(ctx)
		if statusErr != nil {
			return nil, statusErr
		}
		if current.GetHasValidSvid() {
			if err := s.writeSpireEnv(env, address, port); err != nil {
				return nil, status.Errorf(codes.Internal, "write SPIRE env file %s: %v", spireAgentEnvPath, err)
			}
			slog.Info("SPIRE agent already uses requested server", "server_address", address, "server_port", port)
			return &pb.ReconfigureSpireAgentResponse{Outcome: pb.ReconfigureSpireAgentResponse_OUTCOME_UNCHANGED}, nil
		}
	}

	slog.Info("reconfiguring SPIRE agent",
		"previous_server_address", currentAddress, "previous_server_port", currentPort,
		"server_address", address, "server_port", port)
	updated := rewriteSpireAgentServer(conf.content, address, port)
	if string(updated) != string(conf.content) {
		if err := s.writeWithBackup(confPath, conf.priorFile, updated); err != nil {
			return nil, status.Errorf(codes.Internal, "write SPIRE agent config %s: %v", spireAgentConfigPath, err)
		}
	}
	if err := s.writeSpireEnv(env, address, port); err != nil {
		return nil, status.Errorf(codes.Internal, "write SPIRE env file %s: %v", spireAgentEnvPath, err)
	}

	if _, err := s.Runner.Run("systemctl", []string{"restart", spireAgentServiceName}, nil); err != nil {
		return spireAgentFailed(address, port, fmt.Errorf("restart %s: %w", spireAgentServiceName, err)), nil
	}

	return s.waitForSpireAgentSVID(ctx, address, port)
}

// GetSpireAgentStatus reports the SPIRE server the node's agent config points
// at and whether the running agent serves a valid X.509 SVID from it.
func (s *NodeAgentServer) GetSpireAgentStatus(ctx context.Context, _ *pb.GetSpireAgentStatusRequest) (*pb.GetSpireAgentStatusResponse, error) {
	defer logRPCSpan(ctx, "GetSpireAgentStatus")()
	return s.spireAgentStatus(ctx)
}

// waitForSpireAgentSVID polls until the agent serves a valid SVID. The agent
// may exit once after the restart while it re-attests to a server that does
// not know it, and systemd restarts it, so intermediate inactive states are
// not failures.
func (s *NodeAgentServer) waitForSpireAgentSVID(ctx context.Context, address string, port uint32) (*pb.ReconfigureSpireAgentResponse, error) {
	interval := s.spireAgentPollInterval
	if interval <= 0 {
		interval = defaultSpireAgentPollInterval
	}
	timeout := s.spireAgentWaitTimeout
	if timeout <= 0 {
		timeout = defaultSpireAgentWaitTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastReason string
	for {
		current, err := s.spireAgentStatus(waitCtx)
		switch {
		case err != nil:
			lastReason = err.Error()
		case current.GetHasValidSvid():
			slog.Info(
				"reconfigured SPIRE agent",
				"server_address", address,
				"server_port", port,
				"spiffe_id", current.GetSvidSpiffeId(),
			)
			return &pb.ReconfigureSpireAgentResponse{Outcome: pb.ReconfigureSpireAgentResponse_OUTCOME_APPLIED}, nil
		default:
			lastReason = current.GetMessage()
		}

		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return nil, status.FromContextError(ctx.Err()).Err()
			}
			return spireAgentFailed(address, port, fmt.Errorf("SPIRE agent did not serve a valid SVID within %s: %s", timeout, lastReason)), nil
		case <-time.After(interval):
		}
	}
}

func spireAgentFailed(address string, port uint32, err error) *pb.ReconfigureSpireAgentResponse {
	slog.Error("failed to reconfigure SPIRE agent", "server_address", address, "server_port", port, "err", err)
	return &pb.ReconfigureSpireAgentResponse{
		Outcome:      pb.ReconfigureSpireAgentResponse_OUTCOME_FAILED,
		ErrorMessage: err.Error(),
	}
}

func (s *NodeAgentServer) spireAgentStatus(ctx context.Context) (*pb.GetSpireAgentStatusResponse, error) {
	conf, err := readSpireAgentConfig(s.resolveFilePath(spireAgentConfigPath))
	if err != nil {
		return nil, err
	}
	address, port, err := parseSpireAgentServer(conf.content)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "invalid SPIRE agent config %s: %v", spireAgentConfigPath, err)
	}
	resp := &pb.GetSpireAgentStatusResponse{ServerAddress: address, ServerPort: port}

	out, err := s.Runner.RunSplitOutput("systemctl", spireAgentShowArgs, nil)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "systemctl show %s: %v: %s", spireAgentServiceName, err, strings.TrimSpace(string(out.Stderr)))
	}
	activeState, activeSince := parseSpireAgentUnitState(string(out.Stdout))
	switch {
	case activeState != "active":
		resp.Message = fmt.Sprintf("%s is %s", spireAgentServiceName, activeState)
		return resp, nil
	case activeSince < conf.modTime.Truncate(time.Second).Unix():
		resp.Message = fmt.Sprintf("%s was started before %s was last written", spireAgentServiceName, spireAgentConfigPath)
		return resp, nil
	}
	resp.ConfigApplied = true

	if s.SPIFFESocketPath == "" {
		resp.Message = "SPIFFE workload API socket is not configured"
		return resp, nil
	}
	fetch := s.fetchX509SVID
	if fetch == nil {
		fetch = fetchX509SVIDFromWorkloadAPI
	}
	svid, err := fetch(ctx, s.SPIFFESocketPath)
	if err != nil {
		resp.Message = fmt.Sprintf("fetch X.509 SVID: %v", err)
		return resp, nil
	}
	if len(svid.Certificates) == 0 {
		resp.Message = "fetched X.509 SVID has no certificates"
		return resp, nil
	}
	notAfter := svid.Certificates[0].NotAfter
	resp.SvidSpiffeId = svid.ID.String()
	resp.SvidExpiresAtUnixSeconds = notAfter.Unix()

	now := time.Now
	if s.now != nil {
		now = s.now
	}
	if !now().Before(notAfter) {
		resp.Message = fmt.Sprintf("X.509 SVID expired at %s", notAfter.UTC().Format(time.RFC3339))
		return resp, nil
	}
	// SPIRE agents keep workload SVIDs only in memory, so an SVID served by an
	// agent started after the config was written was minted by the configured server.
	resp.HasValidSvid = true
	return resp, nil
}

func fetchX509SVIDFromWorkloadAPI(ctx context.Context, socket string) (*x509svid.SVID, error) {
	ctx, cancel := context.WithTimeout(ctx, spireSVIDFetchTimeout)
	defer cancel()
	return workloadapi.FetchX509SVID(ctx, workloadapi.WithAddr(socket))
}

type spireAgentConfig struct {
	priorFile
	modTime time.Time
}

func readSpireAgentConfig(path string) (spireAgentConfig, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return spireAgentConfig{}, status.Errorf(codes.FailedPrecondition, "SPIRE agent config %s does not exist", spireAgentConfigPath)
		}
		return spireAgentConfig{}, status.Errorf(codes.Internal, "read SPIRE agent config %s: %v", spireAgentConfigPath, err)
	}
	prior, err := capturePriorFile(path, content)
	if err != nil {
		return spireAgentConfig{}, status.Errorf(codes.Internal, "stat SPIRE agent config %s: %v", spireAgentConfigPath, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return spireAgentConfig{}, status.Errorf(codes.Internal, "stat SPIRE agent config %s: %v", spireAgentConfigPath, err)
	}
	return spireAgentConfig{priorFile: prior, modTime: info.ModTime()}, nil
}

// capturePriorFileIfExists returns nil when path does not exist.
func capturePriorFileIfExists(path string) (*priorFile, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	prior, err := capturePriorFile(path, content)
	if err != nil {
		return nil, err
	}
	return &prior, nil
}

// writeSpireEnv rewrites the boot-time env file when it exists and differs;
// spire-configure.sh re-renders agent.conf from it on every boot.
func (s *NodeAgentServer) writeSpireEnv(env *priorFile, address string, port uint32) error {
	if env == nil {
		return nil
	}
	updated := rewriteSpireEnv(env.content, address, port)
	if string(updated) == string(env.content) {
		return nil
	}
	return s.writeWithBackup(s.resolveFilePath(spireAgentEnvPath), *env, updated)
}

// writeWithBackup saves the file's current content to <path>.bak, with the
// same mode and owner since spire.env can hold a join token, then writes updated.
func (s *NodeAgentServer) writeWithBackup(path string, prior priorFile, updated []byte) error {
	backup := fileSpec{path: path + ".bak", content: prior.content, mode: prior.mode, uid: prior.uid, gid: prior.gid}
	if err := atomicWriteFileWithOwnership(backup, s.kubeadmFileOps); err != nil {
		return fmt.Errorf("back up %s: %w", path, err)
	}
	return atomicWriteFileWithOwnership(fileSpec{path: path, content: updated, mode: prior.mode, uid: prior.uid, gid: prior.gid}, s.kubeadmFileOps)
}

func validateSpireServerAddress(address string) error {
	if address == "" {
		return errors.New("must be set")
	}
	if net.ParseIP(address) != nil {
		return nil
	}
	if problems := validation.IsDNS1123Subdomain(address); len(problems) != 0 {
		return fmt.Errorf("must be an IP address or DNS name: %s", strings.Join(problems, ", "))
	}
	return nil
}

// parseSpireAgentServer returns the server_address and server_port values of
// an agent.conf. Each key must appear exactly once.
func parseSpireAgentServer(content []byte) (string, uint32, error) {
	rawAddress, err := singleSpireConfigValue(content, spireServerAddressLine, "server_address")
	if err != nil {
		return "", 0, err
	}
	rawPort, err := singleSpireConfigValue(content, spireServerPortLine, "server_port")
	if err != nil {
		return "", 0, err
	}
	address, err := strconv.Unquote(rawAddress)
	if err != nil {
		return "", 0, fmt.Errorf("server_address %s is not a quoted string", rawAddress)
	}
	if unquoted, unquoteErr := strconv.Unquote(rawPort); unquoteErr == nil {
		rawPort = unquoted
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || port == 0 {
		return "", 0, fmt.Errorf("server_port %s is not a port number", rawPort)
	}
	return address, uint32(port), nil
}

func singleSpireConfigValue(content []byte, line *regexp.Regexp, key string) (string, error) {
	matches := line.FindAllSubmatch(content, -1)
	if len(matches) != 1 {
		return "", fmt.Errorf("expected exactly one %s line, found %d", key, len(matches))
	}
	return strings.TrimRight(string(matches[0][2]), " \t\r"), nil
}

// rewriteSpireAgentServer replaces the server_address and server_port values,
// keeping indentation and every other byte. Callers must have checked with
// parseSpireAgentServer that each key appears exactly once.
func rewriteSpireAgentServer(content []byte, address string, port uint32) []byte {
	content = spireServerAddressLine.ReplaceAll(content, []byte("${1}"+strconv.Quote(address)))
	return spireServerPortLine.ReplaceAll(content, []byte("${1}"+strconv.Quote(strconv.FormatUint(uint64(port), 10))))
}

// rewriteSpireEnv sets SPIRE_SERVER_ADDRESS and SPIRE_SERVER_PORT, which
// os-image/SPEC.md requires in the file. The address is validated, so it needs
// no quoting.
func rewriteSpireEnv(content []byte, address string, port uint32) []byte {
	content = spireEnvAddressLine.ReplaceAll(content, []byte("SPIRE_SERVER_ADDRESS="+address))
	return spireEnvPortLine.ReplaceAll(content, []byte("SPIRE_SERVER_PORT="+strconv.FormatUint(uint64(port), 10)))
}

// parseSpireAgentUnitState parses systemctl show output. activeSince is zero
// when the unit never entered the active state.
func parseSpireAgentUnitState(output string) (string, int64) {
	var activeState string
	var activeSince int64
	for _, line := range strings.Split(output, "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "ActiveState":
			activeState = value
		case "ActiveEnterTimestamp":
			activeSince, _ = strconv.ParseInt(strings.TrimPrefix(value, "@"), 10, 64)
		}
	}
	return activeState, activeSince
}
