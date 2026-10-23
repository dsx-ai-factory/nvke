// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/apierr"
)

const (
	mockCloudInitStatusPath  = "/var/lib/nico-containers/cloud-init/status"
	statusUpdateTimeout      = 5 * time.Second
	systemdReadyTimeout      = 60 * time.Second
	systemdReadyPollInterval = 250 * time.Millisecond
)

type cloudConfig struct {
	WriteFiles []cloudConfigWriteFile `yaml:"write_files"`
	RunCmd     []string               `yaml:"runcmd"`
}

type cloudConfigWriteFile struct {
	Path        string `yaml:"path"`
	Owner       string `yaml:"owner"`
	Permissions string `yaml:"permissions"`
	Encoding    string `yaml:"encoding"`
	Content     string `yaml:"content"`
}

type dockerInspect struct {
	Created string `json:"Created"`
	Config  struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func (s *Service) findManagedContainerIDByInstanceID(ctx context.Context, instanceID string) (string, error) {
	containerIDs, err := s.listManagedContainerIDs(ctx, map[string]string{labelInstanceID: instanceID})
	if err != nil {
		return "", err
	}
	if len(containerIDs) == 0 {
		return "", nil
	}
	return containerIDs[0], nil
}

func (s *Service) listManagedContainerIDs(ctx context.Context, labels map[string]string) ([]string, error) {
	args := []string{"ps", "-aq", "--filter", "label=" + labelManaged + "=true"}
	for key, value := range labels {
		if value = strings.TrimSpace(value); value != "" {
			args = append(args, "--filter", "label="+key+"="+value)
		}
	}
	out, err := s.runDocker(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: list managed containers: %w", apierr.ErrUnavailable, err)
	}
	return strings.Fields(out), nil
}

func (s *Service) inspectContainer(ctx context.Context, containerID string) (*dockerInspect, error) {
	out, err := s.runDocker(ctx, "inspect", containerID)
	if err != nil {
		return nil, fmt.Errorf("inspect container %s: %w", containerID, err)
	}
	var results []dockerInspect
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		return nil, fmt.Errorf("decode docker inspect output: %w", err)
	}
	if len(results) == 0 {
		return nil, apierr.ErrNotFound
	}
	return &results[0], nil
}

func (s *Service) firstContainerIPAddress(inspectResult *dockerInspect) string {
	if inspectResult == nil {
		return ""
	}
	if network, ok := inspectResult.NetworkSettings.Networks[s.cfg.NodeNetwork]; ok && network.IPAddress != "" {
		return network.IPAddress
	}
	for _, network := range inspectResult.NetworkSettings.Networks {
		if network.IPAddress != "" {
			return network.IPAddress
		}
	}
	return ""
}

func (s *Service) removeNodeContainer(ctx context.Context, containerID string) error {
	_, _ = s.runDocker(ctx, "stop", "--time", "10", containerID)
	if _, err := s.runDocker(ctx, "rm", "--force", "--volumes", containerID); err != nil {
		return fmt.Errorf("remove node container: %w", err)
	}
	return nil
}

func (s *Service) tpmEKCertificate(ctx context.Context, containerID string) (string, error) {
	out, err := s.execInContainerOutput(
		ctx,
		containerID,
		"sh", "-lc",
		fmt.Sprintf(
			"if [ -s %q ]; then python3 -c 'import base64, pathlib; print(base64.b64encode(pathlib.Path(%q).read_bytes()).decode(), end=\"\")'; fi",
			tpmEKCertPath,
			tpmEKCertPath,
		),
	)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (s *Service) applyCloudConfig(ctx context.Context, containerID, userData string) (retErr error) {
	// The node agent waits for cloud-init to finish before continuing bootstrap,
	// so expose the progress of this synchronous mock implementation.
	if err := s.writeMockCloudInitStatus(ctx, containerID, "running"); err != nil {
		return fmt.Errorf("set mock cloud-init status to running: %w", err)
	}
	defer func() {
		if retErr == nil {
			return
		}
		statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusUpdateTimeout)
		defer cancel()
		if err := s.writeMockCloudInitStatus(statusCtx, containerID, "error"); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("set mock cloud-init status to error: %w", err))
		}
	}()

	userData = strings.TrimSpace(userData)
	if userData != "" {
		userData = strings.TrimPrefix(userData, "#cloud-config\n")
		userData = strings.TrimPrefix(userData, "#cloud-config\r\n")

		var cfg cloudConfig
		if err := yaml.Unmarshal([]byte(userData), &cfg); err != nil {
			return fmt.Errorf("parse cloud-config userData: %w", err)
		}
		for _, file := range cfg.WriteFiles {
			if strings.TrimSpace(file.Path) == "" {
				continue
			}
			content, err := decodeCloudConfigContent(file)
			if err != nil {
				return fmt.Errorf("decode %s: %w", file.Path, err)
			}
			if err := s.execInContainer(ctx, containerID, "mkdir", "-p", filepath.Dir(file.Path)); err != nil {
				return fmt.Errorf("create directory for %s: %w", file.Path, err)
			}
			if err := s.copyFileToContainer(ctx, containerID, file.Path, content); err != nil {
				return fmt.Errorf("write %s: %w", file.Path, err)
			}
			if permissions := strings.TrimSpace(file.Permissions); permissions != "" {
				if err := s.execInContainer(ctx, containerID, "chmod", permissions, file.Path); err != nil {
					return fmt.Errorf("chmod %s: %w", file.Path, err)
				}
			}
			if owner := strings.TrimSpace(file.Owner); owner != "" {
				if err := s.execInContainer(ctx, containerID, "chown", owner, file.Path); err != nil {
					return fmt.Errorf("chown %s: %w", file.Path, err)
				}
			}
		}
		systemdReady := false
		for _, command := range cfg.RunCmd {
			if strings.TrimSpace(command) == "" {
				continue
			}
			// cloud-init runs these after boot, and they call systemctl.
			if !systemdReady {
				if err := s.waitForSystemd(ctx, containerID); err != nil {
					return err
				}
				systemdReady = true
			}
			if err := s.execInContainer(ctx, containerID, "sh", "-lc", command); err != nil {
				return fmt.Errorf("run cloud-config command %q: %w", command, err)
			}
		}
	}

	if err := s.writeMockCloudInitStatus(ctx, containerID, "done"); err != nil {
		return fmt.Errorf("set mock cloud-init status to done: %w", err)
	}
	return nil
}

// waitForSystemd blocks until systemd inside the container accepts commands.
// docker start returns once the entrypoint is launched, and for about the first
// second PID 1 is not systemd yet, so systemctl fails with "System has not been
// booted with systemd". A full boot takes several seconds more, which a create
// request should not wait for, so basic.target being up ("starting") is enough.
func (s *Service) waitForSystemd(ctx context.Context, containerID string) error {
	ctx, cancel := context.WithTimeout(ctx, systemdReadyTimeout)
	defer cancel()

	state := ""
	for {
		// is-system-running exits non-zero in every state but "running", so
		// read the state from its output and ignore the exit status.
		out, err := s.execInContainerOutput(ctx, containerID, "sh", "-c", "systemctl is-system-running 2>/dev/null || true")
		if ctx.Err() != nil {
			return fmt.Errorf("wait for systemd in node container (last state %q): %w", state, ctx.Err())
		}
		if err != nil {
			return fmt.Errorf("check systemd state: %w", err)
		}
		state = out
		switch state {
		case "starting", "running", "degraded":
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for systemd in node container (last state %q): %w", state, ctx.Err())
		case <-time.After(systemdReadyPollInterval):
		}
	}
}

func decodeCloudConfigContent(file cloudConfigWriteFile) (string, error) {
	switch file.Encoding {
	case "":
		return file.Content, nil
	case "b64", "base64":
		decoded, err := base64.StdEncoding.DecodeString(file.Content)
		if err != nil {
			return "", fmt.Errorf("decode base64 content: %w", err)
		}
		return string(decoded), nil
	default:
		return "", fmt.Errorf("unsupported cloud-config encoding %q", file.Encoding)
	}
}

// writeMockCloudInitStatus atomically updates the status read by the
// cloud-init shim in mock NICo node containers.
func (s *Service) writeMockCloudInitStatus(ctx context.Context, containerID, status string) error {
	const writeStatus = `set -eu
status_dir=/var/lib/nico-containers/cloud-init
mkdir -p "$status_dir"
status_tmp="$status_dir/status.tmp.$$"
trap 'rm -f "$status_tmp"' EXIT
printf '%s\n' "$1" > "$status_tmp"
chmod 0644 "$status_tmp"
mv -f "$status_tmp" ` + mockCloudInitStatusPath + `
trap - EXIT`

	if err := s.execInContainer(ctx, containerID, "sh", "-c", writeStatus, "cloud-init-status", status); err != nil {
		return fmt.Errorf("write %s: %w", mockCloudInitStatusPath, err)
	}
	return nil
}

func (s *Service) copyFileToContainer(ctx context.Context, containerID, targetPath, content string) error {
	cmd := exec.CommandContext(ctx, s.cfg.DockerBinary, "exec", "-i", containerID, "sh", "-c", "cat > "+singleQuote(targetPath))
	cmd.Stdin = strings.NewReader(content)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("write file into container: %s", message)
	}
	return nil
}

func singleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func (s *Service) execInContainer(ctx context.Context, containerID string, args ...string) error {
	_, err := s.execInContainerOutput(ctx, containerID, args...)
	return err
}

func (s *Service) execInContainerOutput(ctx context.Context, containerID string, args ...string) (string, error) {
	command := append([]string{"exec", containerID}, args...)
	out, err := s.runDocker(ctx, command...)
	if err != nil {
		return "", fmt.Errorf("exec in container: %w", err)
	}
	return out, nil
}

func (s *Service) runDocker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.cfg.DockerBinary, args...)
	output, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(output))
	if err != nil {
		if trimmed == "" {
			trimmed = err.Error()
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("%w: %s", apierr.ErrUnavailable, trimmed)
		}
		return "", fmt.Errorf("%s %s: %s", s.cfg.DockerBinary, strings.Join(args, " "), trimmed)
	}
	return trimmed, nil
}
