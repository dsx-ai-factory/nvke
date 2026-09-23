// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package container manages NICo container instances with Docker.
package container

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	nicosdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/apierr"
)

const (
	defaultNodeNetwork = "kind"
	defaultNodeImage   = "cwe-capd-kindest-node:local"
	defaultDocker      = "docker"

	tpmEKCertPath      = "/var/lib/swtpm/ek.cert"
	mockBootIDDir      = "/var/lib/nico-containers"
	mockBootIDPath     = mockBootIDDir + "/boot-id"
	mockNextBootIDPath = mockBootIDDir + "/next-boot-id"

	// Docker containers share the host kernel's boot ID. This wrapper gives each
	// mock node a persistent boot ID and installs a staged replacement before
	// kubelet starts, so restarting the container looks like a real reboot to
	// Kubernetes. Docker reruns the wrapper on every container start.
	mockNodeEntrypoint = `set -e
mkdir -p ` + mockBootIDDir + `
if [ -f ` + mockNextBootIDPath + ` ]; then
  mv ` + mockNextBootIDPath + ` ` + mockBootIDPath + `
fi
if [ ! -f ` + mockBootIDPath + ` ]; then
  cat /proc/sys/kernel/random/uuid > ` + mockBootIDPath + `
fi
mount --bind ` + mockBootIDPath + ` /proc/sys/kernel/random/boot_id
exec /usr/local/bin/entrypoint /sbin/init`

	labelManaged      = "nvidia.com/nico-containers.managed"
	labelInstanceID   = "nvidia.com/nico-containers.instance-id"
	labelInstanceName = "nvidia.com/nico-containers.instance-name"
	labelSiteID       = "nvidia.com/nico-containers.site-id"
	labelVPCID        = "nvidia.com/nico-containers.vpc-id"
	labelTenantID     = "nvidia.com/nico-containers.tenant-id"
	labelInstanceType = "nvidia.com/nico-containers.instance-type-id"
	labelSubnetID     = "nvidia.com/nico-containers.subnet-id"
	labelVPCPrefixID  = "nvidia.com/nico-containers.vpc-prefix-id"
	labelMachineID    = "nvidia.com/nico-containers.machine-id"

	labelIpxeScript           = "nvidia.com/nico-containers.ipxe-script"
	labelSSHKeyGroupIDs       = "nvidia.com/nico-containers.ssh-key-group-ids"
	labelInterfaces           = "nvidia.com/nico-containers.interfaces"
	labelInfiniBandInterfaces = "nvidia.com/nico-containers.infiniband-interfaces"
	labelNVLinkInterfaces     = "nvidia.com/nico-containers.nvlink-interfaces"
)

type Config struct {
	OrgID                  string
	TenantID               string
	SiteID                 string
	VPCID                  string
	SubnetID               string
	SSHKeyGroupID          string
	InstanceTypeID         string
	NodeNetwork            string
	NodeImage              string
	SpireAttestationMethod string
	DockerBinary           string
}

func (c Config) withDefaults() Config {
	if strings.TrimSpace(c.NodeNetwork) == "" {
		c.NodeNetwork = defaultNodeNetwork
	}
	if strings.TrimSpace(c.NodeImage) == "" {
		c.NodeImage = defaultNodeImage
	}
	c.SpireAttestationMethod = strings.TrimSpace(c.SpireAttestationMethod)
	if c.SpireAttestationMethod == "" {
		c.SpireAttestationMethod = "tpm"
	}
	if strings.TrimSpace(c.DockerBinary) == "" {
		c.DockerBinary = defaultDocker
	}
	return c
}

func (c Config) validate() error {
	if c.SpireAttestationMethod != "tpm" && c.SpireAttestationMethod != "joinToken" && c.SpireAttestationMethod != "dsxImds" {
		return fmt.Errorf("container backend unsupported SPIRE attestation method %q", c.SpireAttestationMethod)
	}
	for key, value := range map[string]string{
		"org": c.OrgID, "tenant": c.TenantID, "site": c.SiteID, "vpc": c.VPCID,
		"subnet": c.SubnetID, "ssh key group": c.SSHKeyGroupID,
		"instance type": c.InstanceTypeID, "node network": c.NodeNetwork,
		"node image": c.NodeImage, "docker binary": c.DockerBinary,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("container backend %s is required", key)
		}
	}
	return nil
}

type Service struct {
	cfg    Config
	logger *slog.Logger
}

func New(cfg Config, logger *slog.Logger) (*Service, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Service{
		cfg:    cfg,
		logger: logger.With("component", "nico-containers"),
	}, nil
}

func (s *Service) Health(ctx context.Context) error {
	if _, err := s.runDocker(ctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("%w: %v", apierr.ErrUnavailable, err)
	}
	return nil
}

func (s *Service) Create(ctx context.Context, instanceID string, req nicosdk.InstanceCreateRequest) (*nicosdk.Instance, error) {
	if strings.TrimSpace(instanceID) == "" {
		return nil, fmt.Errorf("instance ID is required")
	}
	if strings.TrimSpace(req.GetName()) == "" {
		return nil, fmt.Errorf("name is required")
	}
	if len(req.GetInterfaces()) == 0 {
		return nil, fmt.Errorf("interfaces must contain at least one entry")
	}
	if req.GetTenantId() != s.cfg.TenantID || req.GetVpcId() != s.cfg.VPCID {
		return nil, fmt.Errorf("%w: request tenant or vpc does not match container bundle", apierr.ErrOwnershipConflict)
	}

	_, err := s.findInstanceByName(ctx, req.GetName())
	switch {
	case err == nil:
		return nil, fmt.Errorf("%w: instance %q already exists", apierr.ErrConflict, req.GetName())
	case !errors.Is(err, apierr.ErrNotFound):
		return nil, err
	}

	return s.createInstance(ctx, instanceID, req)
}

func (s *Service) Get(ctx context.Context, instanceID string) (*nicosdk.Instance, error) {
	containerID, err := s.findManagedContainerIDByInstanceID(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if containerID == "" {
		return nil, apierr.ErrNotFound
	}
	return s.loadInstance(ctx, containerID)
}

func (s *Service) Update(ctx context.Context, instanceID string, req nicosdk.InstanceUpdateRequest) (*nicosdk.Instance, error) {
	containerID, err := s.findManagedContainerIDByInstanceID(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if containerID == "" {
		return nil, apierr.ErrNotFound
	}

	if req.GetTriggerReboot() {
		stageBootID := "cat /proc/sys/kernel/random/uuid > " + mockNextBootIDPath
		if _, err := s.runDocker(ctx, "exec", containerID, "/bin/sh", "-c", stageBootID); err != nil {
			return nil, fmt.Errorf("stage instance boot ID: %w", err)
		}
		if _, err := s.runDocker(ctx, "restart", "--time", "10", containerID); err != nil {
			return nil, fmt.Errorf("restart instance container: %w", err)
		}
		s.logger.Info("restarted mock instance container", "instance_id", instanceID, "container_id", containerID)
	}
	return s.loadInstance(ctx, containerID)
}

func (s *Service) Delete(ctx context.Context, instanceID string, req *nicosdk.InstanceDeleteRequest) error {
	containerID, err := s.findManagedContainerIDByInstanceID(ctx, instanceID)
	if err != nil {
		return err
	}
	if containerID == "" {
		return apierr.ErrNotFound
	}
	if req != nil && req.MachineHealthIssue != nil {
		s.logger.Info("flagging container instance for repair before deletion",
			"instance_id", instanceID,
			"summary", req.MachineHealthIssue.GetSummary(),
			"category", req.MachineHealthIssue.GetCategory(),
		)
	}
	return s.removeNodeContainer(ctx, containerID)
}

func (s *Service) createInstance(ctx context.Context, instanceID string, req nicosdk.InstanceCreateRequest) (*nicosdk.Instance, error) {
	containerName := req.GetName() + "-" + instanceID[:8]
	machineID := req.GetMachineId()
	if machineID == "" {
		machineID = "machine-" + instanceID[:8]
	}

	labels := make(map[string]string, len(req.GetLabels())+16)
	for key, value := range req.GetLabels() {
		if strings.TrimSpace(key) != "" {
			labels[key] = value
		}
	}
	labels[labelManaged] = "true"
	labels[labelInstanceID] = instanceID
	labels[labelInstanceName] = req.GetName()
	labels[labelSiteID] = s.cfg.SiteID
	labels[labelVPCID] = req.GetVpcId()
	labels[labelTenantID] = req.GetTenantId()
	labels[labelInstanceType] = firstNonEmpty(req.GetInstanceTypeId(), s.cfg.InstanceTypeID)
	labels[labelMachineID] = machineID
	if req.HasIpxeScript() {
		labels[labelIpxeScript] = encodeLabelValue(req.GetIpxeScript())
	}
	if len(req.GetSshKeyGroupIds()) > 0 {
		labels[labelSSHKeyGroupIDs] = encodeLabelValue(req.GetSshKeyGroupIds())
	}
	if len(req.GetInterfaces()) > 0 {
		labels[labelInterfaces] = encodeLabelValue(req.GetInterfaces())
		if subnetID := req.GetInterfaces()[0].GetSubnetId(); subnetID != "" {
			labels[labelSubnetID] = subnetID
		}
		if vpcPrefixID := req.GetInterfaces()[0].GetVpcPrefixId(); vpcPrefixID != "" {
			labels[labelVPCPrefixID] = vpcPrefixID
		}
	}
	if len(req.GetInfinibandInterfaces()) > 0 {
		labels[labelInfiniBandInterfaces] = encodeLabelValue(req.GetInfinibandInterfaces())
	}
	if len(req.GetNvLinkInterfaces()) > 0 {
		labels[labelNVLinkInterfaces] = encodeLabelValue(req.GetNvLinkInterfaces())
	}

	createArgs := []string{
		"create",
		"--name", containerName,
		"--hostname", req.GetName(),
		"--privileged",
		"--security-opt", "seccomp=unconfined",
		"--security-opt", "apparmor=unconfined",
		"--tmpfs", "/tmp",
		"--tmpfs", "/run",
		"--tmpfs", "/var/lib/etcd:size=4g",
		"--restart", "always",
		"--network", s.cfg.NodeNetwork,
		"--env", "KUBECONFIG=/etc/kubernetes/admin.conf",
		"--env", "KIND_EXPERIMENTAL_CONTAINERD_SNAPSHOTTER=fuse-overlayfs",
		"--volume", "/lib/modules:/lib/modules:ro",
		"--entrypoint", "/bin/sh",
		"--tty",
	}
	for key, value := range labels {
		createArgs = append(createArgs, "--label", key+"="+value)
	}
	createArgs = append(createArgs, s.cfg.NodeImage, "-c", mockNodeEntrypoint)

	containerID, err := s.runDocker(ctx, createArgs...)
	if err != nil {
		return nil, fmt.Errorf("create node container: %w", err)
	}
	instance, err := s.startInstanceContainer(ctx, containerID, req)
	if err != nil {
		_ = s.removeNodeContainer(context.WithoutCancel(ctx), containerID)
		return nil, err
	}
	return instance, nil
}

func (s *Service) startInstanceContainer(ctx context.Context, containerID string, req nicosdk.InstanceCreateRequest) (*nicosdk.Instance, error) {
	if _, err := s.runDocker(ctx, "start", containerID); err != nil {
		return nil, fmt.Errorf("start node container: %w", err)
	}
	if err := s.applyCloudConfig(ctx, containerID, req.GetUserData()); err != nil {
		return nil, err
	}
	return s.loadInstance(ctx, containerID)
}

func (s *Service) findInstanceByName(ctx context.Context, name string) (*nicosdk.Instance, error) {
	containerIDs, err := s.listManagedContainerIDs(ctx, map[string]string{labelInstanceName: name})
	if err != nil {
		return nil, err
	}
	if len(containerIDs) == 0 {
		return nil, apierr.ErrNotFound
	}
	return s.loadInstance(ctx, containerIDs[0])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
