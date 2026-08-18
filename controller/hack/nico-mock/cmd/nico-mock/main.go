// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// nico-mock is a local, Docker-backed stand-in for the small subset of NICo and
// Forge APIs that CWE exercises during development. It provisions kind-style
// node containers, serves the discovery and instance routes needed by the local
// NICo and legacy Forge cluster-create flows, and reconstructs API responses
// from Docker labels plus `docker inspect`.
//
// Limitations:
//   - Only the routes wired below are implemented; most NICo and Forge
//     behavior is not simulated.
//   - Resource discovery is intentionally static: the mock exposes one site,
//     one VPC, one subnet, one SSH key group, and one instance type unless a
//     caller overrides those IDs on created instances.
//   - Instance identity and bootstrap state live in local Docker containers;
//     labels applied after creation are process-local. There is no multi-host
//     coordination.
//   - User data handling is intentionally narrow: only `write_files` and string
//     `runcmd` entries from cloud-config are applied.
//   - TPM behavior is local-dev oriented: instance readiness is inferred from
//     whether the node image has produced a DER-encoded EK certificate at
//     `/var/lib/swtpm/ek.cert`, and no full attestation handshake is simulated.
//   - The mock assumes a local Docker daemon and a kind-like node image whose
//     bootstrap patch files can be managed from inside the container.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	nicosdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

const (
	defaultListenAddr = ":8080"

	defaultOrgID          = "local-dev-org"
	defaultTenantID       = "00000000-0000-0000-0000-000000000010"
	defaultSiteID         = "00000000-0000-0000-0000-000000000001"
	defaultVPCID          = "00000000-0000-0000-0000-000000000002"
	defaultSubnetID       = "00000000-0000-0000-0000-000000000003"
	defaultSSHKeyGroupID  = "00000000-0000-0000-0000-000000000004"
	defaultInstanceTypeID = "00000000-0000-0000-0000-000000000005"

	defaultNodeNetwork = "kind"
	defaultNodeImage   = "cwe-capd-kindest-node:local"

	defaultSiteName                 = "mock-site"
	defaultVPCName                  = "mock-vpc"
	defaultSubnetName               = "mock-subnet"
	defaultSSHKeyGroupName          = "mock-ssh-key-group"
	defaultInstanceTypeName         = "mock-instance-type"
	defaultSubnetIPv4Prefix         = "172.18.254.0"
	defaultSubnetPrefixLength       = 24
	defaultInstanceTypeTotal        = 32
	defaultInstanceTypeUnused       = 32
	defaultInstanceTypeUnusedUsable = 32

	// The local node image's swtpm-local.service normalizes its EK certificate to
	// this path. Reading the file directly lets the mock return Forge-compatible
	// `tpmEkCertificate` data without duplicating TPM state outside the container.
	tpmEKCertPath = "/var/lib/swtpm/ek.cert"

	mockCloudInitStatusPath = "/var/lib/nico-mock-cloud-init/status"
	statusUpdateTimeout     = 5 * time.Second
	mockBootIDDir           = "/var/lib/nico-mock"
	mockBootIDPath          = mockBootIDDir + "/boot-id"
	mockNextBootIDPath      = mockBootIDDir + "/next-boot-id"

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

	labelManaged      = "nvidia.com/nico-mock.managed"
	labelInstanceID   = "nvidia.com/nico-mock.instance-id"
	labelInstanceName = "nvidia.com/nico-mock.instance-name"
	labelSiteID       = "nvidia.com/nico-mock.site-id"
	labelVPCID        = "nvidia.com/nico-mock.vpc-id"
	labelTenantID     = "nvidia.com/nico-mock.tenant-id"
	labelInstanceType = "nvidia.com/nico-mock.instance-type-id"
	labelSubnetID     = "nvidia.com/nico-mock.subnet-id"
	labelVPCPrefixID  = "nvidia.com/nico-mock.vpc-prefix-id"
	labelMachineID    = "nvidia.com/nico-mock.machine-id"

	labelIpxeScript           = "nvidia.com/nico-mock.ipxe-script"
	labelSSHKeyGroupIDs       = "nvidia.com/nico-mock.ssh-key-group-ids"
	labelInterfaces           = "nvidia.com/nico-mock.interfaces"
	labelInfiniBandInterfaces = "nvidia.com/nico-mock.infiniband-interfaces"
	labelNVLinkInterfaces     = "nvidia.com/nico-mock.nvlink-interfaces"
)

var errNotFound = errors.New("not found")

type config struct {
	ListenAddr     string
	OrgID          string
	TenantID       string
	SiteID         string
	VPCID          string
	SubnetID       string
	SSHKeyGroupID  string
	InstanceTypeID string
	NodeNetwork    string
	NodeImage      string
}

type cloudConfig struct {
	WriteFiles []cloudConfigWriteFile `yaml:"write_files"`
	RunCmd     []string               `yaml:"runcmd"`
}

type cloudConfigWriteFile struct {
	Path        string `yaml:"path"`
	Owner       string `yaml:"owner"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
}

type dockerInspect struct {
	Config struct {
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

type server struct {
	cfg config

	// instanceTypeCapacity maps an instance type ID to its unusedUsable
	// allocation count. capnico's preflight (GetInstanceTypeWithAllocationStats)
	// treats an unregistered ID as 404 (InstanceTypeNotFound), a registered ID
	// with unusedUsable==0 as InstanceTypeUnavailable, and unusedUsable>0 as
	// available. The map is mutable at runtime via handleSetInstanceTypeCapacity
	// so tests can flip a type's availability to exercise rotation/convergence.
	//
	// instanceTypeName maps an instance type ID to the display name returned in
	// the InstanceType response. It is optional per ID (set via the ?name= query
	// param on handleSetInstanceTypeCapacity); when unset the mock falls back to
	// defaultInstanceTypeName so multiple registered types can be told apart in
	// the API/UI. Both maps are guarded by mu.
	mu                   sync.Mutex
	instanceTypeCapacity map[string]int32
	instanceTypeName     map[string]string
	instanceLabels       map[string]map[string]string
}

// version is set at build time via -ldflags "-X main.version=<version>".
var version = "dev"

func main() {
	versionFlag := flag.Bool("version", false, "Print the version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("nico-mock %s\n", version)
		os.Exit(0)
	}

	cfg := config{
		ListenAddr:     getenv("NICO_MOCK_LISTEN_ADDR", defaultListenAddr),
		OrgID:          getenv("NICO_MOCK_ORG_ID", defaultOrgID),
		TenantID:       getenv("NICO_MOCK_TENANT_ID", defaultTenantID),
		SiteID:         getenv("NICO_MOCK_SITE_ID", defaultSiteID),
		VPCID:          getenv("NICO_MOCK_VPC_ID", defaultVPCID),
		SubnetID:       getenv("NICO_MOCK_SUBNET_ID", defaultSubnetID),
		SSHKeyGroupID:  getenv("NICO_MOCK_SSH_KEY_GROUP_ID", defaultSSHKeyGroupID),
		InstanceTypeID: getenv("NICO_MOCK_INSTANCE_TYPE_ID", defaultInstanceTypeID),
		NodeNetwork:    getenv("NICO_MOCK_NODE_NETWORK", defaultNodeNetwork),
		NodeImage:      getenv("NICO_MOCK_NODE_IMAGE", defaultNodeImage),
	}
	s := &server{
		cfg: cfg,
		instanceTypeCapacity: map[string]int32{
			cfg.InstanceTypeID: int32(defaultInstanceTypeUnusedUsable),
		},
		instanceTypeName: map[string]string{
			cfg.InstanceTypeID: defaultInstanceTypeName,
		},
		instanceLabels: map[string]map[string]string{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	// Test-only admin knob: set/clear an instance type's unusedUsable capacity at
	// runtime, e.g. PUT /admin/instance-type/<id>?unusedUsable=0 to make a type
	// exist-but-unavailable, or ?unusedUsable=32 to bring it back. An optional
	// ?name=<display-name> sets the display name returned for that type so
	// multiple registered types can be distinguished in the API/UI.
	mux.HandleFunc("PUT /admin/instance-type/{instanceTypeId}", s.handleSetInstanceTypeCapacity)
	s.registerAPIRoutes(mux)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout is generous because instance creation streams docker
		// build/run output back to the caller and can legitimately take minutes.
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}

	slog.Info("nico mock listening", "addr", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) registerAPIRoutes(mux *http.ServeMux) {
	register := func(method, suffix string, handler http.HandlerFunc) {
		for _, apiName := range []string{"carbide", "forge"} {
			mux.HandleFunc(method+" /v2/org/{org}/"+apiName+"/"+suffix, handler)
		}
	}

	register(http.MethodGet, "tenant/current", s.handleGetCurrentTenant)
	register(http.MethodGet, "instance", s.handleListInstances)
	register(http.MethodPost, "instance", s.handleCreateInstance)
	register(http.MethodGet, "instance/type", s.handleListInstanceTypes)
	register(http.MethodGet, "instance/type/{instanceTypeId}", s.handleGetInstanceType)
	register(http.MethodGet, "instance/{instanceID}", s.handleGetInstance)
	register(http.MethodPatch, "instance/{instanceID}", s.handleUpdateInstance)
	register(http.MethodDelete, "instance/{instanceID}", s.handleDeleteInstance)
	register(http.MethodGet, "site", s.handleListSites)
	register(http.MethodGet, "site/{siteID}", s.handleGetSite)
	register(http.MethodGet, "subnet", s.handleListSubnets)
	register(http.MethodGet, "vpc", s.handleListVPCs)
	register(http.MethodGet, "vpc/{vpcID}", s.handleGetVPC)
	register(http.MethodGet, "sshkeygroup", s.handleListSSHKeyGroups)
	register(http.MethodGet, "vpc-prefix", emptyListHandler[nicosdk.VpcPrefix]())
	register(http.MethodGet, "ipblock", emptyListHandler[nicosdk.IpBlock]())
	register(http.MethodGet, "infiniband-partition", emptyListHandler[nicosdk.InfiniBandPartition]())
	register(http.MethodGet, "nvlink-logical-partition", emptyListHandler[nicosdk.NVLinkLogicalPartition]())
}

func emptyListHandler[T any]() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []T{})
	}
}

func (s *server) handleGetCurrentTenant(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"id": s.cfg.TenantID})
}

func (s *server) handleListSites(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []nicosdk.Site{s.staticSite()})
}

func (s *server) handleGetSite(w http.ResponseWriter, r *http.Request) {
	siteID := strings.TrimSpace(r.PathValue("siteID"))
	if siteID == "" {
		http.NotFound(w, r)
		return
	}

	site := s.staticSite()
	if site.GetId() != siteID {
		writeError(w, http.StatusNotFound, fmt.Sprintf("site %s not found", siteID))
		return
	}

	writeJSON(w, http.StatusOK, site)
}

func (s *server) handleListSubnets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []nicosdk.Subnet{s.staticSubnet()})
}

func (s *server) handleListVPCs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []nicosdk.VPC{s.staticVPC()})
}

func (s *server) handleGetVPC(w http.ResponseWriter, r *http.Request) {
	vpcID := strings.TrimSpace(r.PathValue("vpcID"))
	if vpcID == "" {
		http.NotFound(w, r)
		return
	}

	vpc := s.staticVPC()
	if vpc.GetId() != vpcID {
		writeError(w, http.StatusNotFound, fmt.Sprintf("vpc %s not found", vpcID))
		return
	}

	writeJSON(w, http.StatusOK, vpc)
}

func (s *server) handleListSSHKeyGroups(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []nicosdk.SshKeyGroup{s.staticSSHKeyGroup()})
}

func (s *server) handleListInstanceTypes(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.instanceTypeCapacity))
	caps := make(map[string]int32, len(s.instanceTypeCapacity))
	for id, c := range s.instanceTypeCapacity {
		ids = append(ids, id)
		caps[id] = c
	}
	s.mu.Unlock()
	sort.Strings(ids)

	types := make([]nicosdk.InstanceType, 0, len(ids))
	for _, id := range ids {
		types = append(types, s.instanceType(id, caps[id]))
	}
	writeJSON(w, http.StatusOK, types)
}

func (s *server) handleGetInstanceType(w http.ResponseWriter, r *http.Request) {
	instanceTypeID := strings.TrimSpace(r.PathValue("instanceTypeId"))
	if instanceTypeID == "" {
		http.NotFound(w, r)
		return
	}

	s.mu.Lock()
	unusedUsable, ok := s.instanceTypeCapacity[instanceTypeID]
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("instance type %s not found", instanceTypeID))
		return
	}

	writeJSON(w, http.StatusOK, s.instanceType(instanceTypeID, unusedUsable))
}

// handleSetInstanceTypeCapacity is a test-only knob to register or update an
// instance type's unusedUsable capacity at runtime. Setting it to 0 makes the
// type exist-but-unavailable (InstanceTypeUnavailable); a positive value makes
// it schedulable again, which is how the wrap-around convergence test brings a
// previously-unavailable preferred type back.
//
// An optional ?name=<display-name> sets the display name returned for the type;
// when present and non-empty it is stored, otherwise any existing name (or the
// default fallback) is left in place. Only unusedUsable is required.
func (s *server) handleSetInstanceTypeCapacity(w http.ResponseWriter, r *http.Request) {
	instanceTypeID := strings.TrimSpace(r.PathValue("instanceTypeId"))
	if instanceTypeID == "" {
		http.NotFound(w, r)
		return
	}
	// Parse with a 32-bit bound and reject negatives so large or negative inputs
	// cannot silently wrap/overflow into instanceTypeCapacity and the derived
	// allocation stats. unusedUsable is a count, so only >= 0 is meaningful.
	unusedUsable, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("unusedUsable")), 10, 32)
	if err != nil || unusedUsable < 0 {
		writeError(w, http.StatusBadRequest, "query param unusedUsable is required and must be an integer between 0 and 2147483647")
		return
	}

	name := strings.TrimSpace(r.URL.Query().Get("name"))

	s.mu.Lock()
	s.instanceTypeCapacity[instanceTypeID] = int32(unusedUsable)
	if name != "" {
		s.instanceTypeName[instanceTypeID] = name
	}
	s.mu.Unlock()

	slog.Info("set instance type capacity", "instanceTypeID", instanceTypeID, "unusedUsable", unusedUsable, "name", name)
	writeJSON(w, http.StatusOK, map[string]any{"instanceTypeId": instanceTypeID, "unusedUsable": unusedUsable, "name": name})
}

func (s *server) staticSite() nicosdk.Site {
	site := nicosdk.NewSite()
	site.SetId(s.cfg.SiteID)
	site.SetName(defaultSiteName)
	site.SetOrg(s.cfg.OrgID)
	site.SetIsOnline(true)

	capabilities := nicosdk.NewSiteCapabilities()
	capabilities.SetNativeNetworking(false)
	capabilities.SetNetworkSecurityGroup(false)
	site.SetCapabilities(*capabilities)

	return *site
}

func (s *server) staticVPC() nicosdk.VPC {
	vpc := nicosdk.NewVPC()
	vpc.SetId(s.cfg.VPCID)
	vpc.SetName(defaultVPCName)
	vpc.SetOrg(s.cfg.OrgID)
	vpc.SetTenantId(s.cfg.TenantID)
	vpc.SetSiteId(s.cfg.SiteID)
	return *vpc
}

func (s *server) staticSubnet() nicosdk.Subnet {
	subnet := nicosdk.NewSubnet()
	subnet.SetId(s.cfg.SubnetID)
	subnet.SetName(defaultSubnetName)
	subnet.SetSiteId(s.cfg.SiteID)
	subnet.SetVpcId(s.cfg.VPCID)
	subnet.SetTenantId(s.cfg.TenantID)
	subnet.SetIpv4Prefix(defaultSubnetIPv4Prefix)
	subnet.SetPrefixLength(int32(defaultSubnetPrefixLength))
	return *subnet
}

func (s *server) staticSSHKeyGroup() nicosdk.SshKeyGroup {
	sshKeyGroup := nicosdk.NewSshKeyGroup()
	sshKeyGroup.SetId(s.cfg.SSHKeyGroupID)
	sshKeyGroup.SetName(defaultSSHKeyGroupName)
	sshKeyGroup.SetOrg(s.cfg.OrgID)
	sshKeyGroup.SetTenantId(s.cfg.TenantID)
	return *sshKeyGroup
}

// instanceType builds an InstanceType response with allocation stats derived
// from unusedUsable. Total stays at least defaultInstanceTypeTotal so a fully
// available type looks realistic; used is derived so total = used + unused.
func (s *server) instanceType(id string, unusedUsable int32) nicosdk.InstanceType {
	instanceType := nicosdk.NewInstanceType()
	instanceType.SetId(id)
	instanceType.SetName(s.instanceTypeDisplayName(id))
	instanceType.SetSiteId(s.cfg.SiteID)

	total := int32(defaultInstanceTypeTotal)
	if unusedUsable > total {
		total = unusedUsable
	}
	unused := unusedUsable

	allocationStats := nicosdk.NewInstanceTypeAllocationStats()
	allocationStats.SetTotal(total)
	allocationStats.SetUnused(unused)
	allocationStats.SetUnusedUsable(unusedUsable)
	instanceType.SetAllocationStats(*allocationStats)

	return *instanceType
}

// instanceTypeDisplayName returns the registered display name for an instance
// type ID, falling back to defaultInstanceTypeName when none was set. It takes
// mu itself so callers can invoke it without holding the lock.
func (s *server) instanceTypeDisplayName(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name := strings.TrimSpace(s.instanceTypeName[id]); name != "" {
		return name
	}
	return defaultInstanceTypeName
}

func (s *server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := s.listInstances(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, instances)
}

func (s *server) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.PathValue("instanceID"))
	if instanceID == "" {
		http.NotFound(w, r)
		return
	}

	inst, err := s.getInstance(r.Context(), instanceID)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("instance %s not found", instanceID))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, inst)
}

func (s *server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var req nicosdk.InstanceCreateRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(req.Interfaces) == 0 {
		writeError(w, http.StatusBadRequest, "interfaces must contain at least one entry")
		return
	}

	_, err := s.findInstanceByName(r.Context(), req.Name)
	switch {
	case err == nil:
		writeError(w, http.StatusConflict, fmt.Sprintf("instance %q already exists", req.Name))
		return
	case !errors.Is(err, errNotFound):
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	inst, err := s.createInstance(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, inst)
}

func (s *server) handleUpdateInstance(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.PathValue("instanceID"))
	if instanceID == "" {
		http.NotFound(w, r)
		return
	}

	var req nicosdk.InstanceUpdateRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	containerID, err := s.findManagedContainerIDByInstanceID(r.Context(), instanceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if containerID == "" {
		writeError(w, http.StatusNotFound, fmt.Sprintf("instance %s not found", instanceID))
		return
	}

	if req.GetTriggerReboot() {
		stageBootID := "cat /proc/sys/kernel/random/uuid > " + mockNextBootIDPath
		if _, err := runDocker(r.Context(), "exec", containerID, "/bin/sh", "-c", stageBootID); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("stage instance boot ID: %v", err))
			return
		}
		if _, err := runDocker(r.Context(), "restart", "--time", "10", containerID); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("restart instance container: %v", err))
			return
		}
		slog.Info("restarted mock instance container", "instanceID", instanceID, "containerID", containerID)
	}
	if req.HasLabels() {
		// CAPNICo applies observed machine and topology labels after instance
		// creation, once NICo has returned their IDs and names. Docker labels
		// cannot be changed after container creation, so retain these updates in
		// memory for subsequent instance responses.
		s.setInstanceLabels(instanceID, req.GetLabels())
	}

	inst, err := s.loadInstance(r.Context(), containerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

func (s *server) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.PathValue("instanceID"))
	if instanceID == "" {
		http.NotFound(w, r)
		return
	}

	var deleteReq nicosdk.InstanceDeleteRequest
	if r.ContentLength > 0 {
		if err := decodeJSONBody(r, &deleteReq); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}

	containerID, err := s.findManagedContainerIDByInstanceID(r.Context(), instanceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if containerID == "" {
		writeError(w, http.StatusNotFound, fmt.Sprintf("instance %s not found", instanceID))
		return
	}

	if deleteReq.MachineHealthIssue != nil {
		slog.Info("flagging instance for repair before deletion",
			"instanceID", instanceID,
			"summary", deleteReq.MachineHealthIssue.GetSummary(),
			"category", deleteReq.MachineHealthIssue.GetCategory(),
		)
	}

	if err := s.removeNodeContainer(r.Context(), containerID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.deleteInstanceLabels(instanceID)

	w.WriteHeader(http.StatusNoContent)
}

// createInstance persists enough Forge/NICo instance state in Docker labels to
// rebuild API responses later from `docker inspect` instead of a separate mock
// database. After the container is created, the remaining bootstrap steps live
// in startInstanceContainer so failures can cleanly tear down the half-built
// container and allow a retry with the same instance name.
func (s *server) createInstance(ctx context.Context, req nicosdk.InstanceCreateRequest) (*nicosdk.Instance, error) {
	instanceID := uuid.NewString()
	containerName := req.Name + "-" + instanceID[:8]
	machineID := req.GetMachineId()
	if machineID == "" {
		machineID = "machine-" + instanceID[:8]
	}

	labels := map[string]string{}
	// CWE currently passes NKE bookkeeping labels here, such as the node,
	// node-pool, and cluster IDs. Keep them separate from the mock's own label
	// namespace so GET/LIST responses can round-trip the caller-visible labels.
	for key, value := range req.Labels {
		if strings.TrimSpace(key) == "" {
			continue
		}
		labels[key] = value
	}

	labels[labelManaged] = "true"
	labels[labelInstanceID] = instanceID
	labels[labelInstanceName] = req.Name
	labels[labelSiteID] = s.cfg.SiteID
	labels[labelVPCID] = firstNonEmpty(req.VpcId, s.cfg.VPCID)
	labels[labelTenantID] = firstNonEmpty(req.TenantId, s.cfg.TenantID)
	labels[labelInstanceType] = firstNonEmpty(req.GetInstanceTypeId(), s.cfg.InstanceTypeID)
	labels[labelMachineID] = machineID
	if req.HasIpxeScript() {
		labels[labelIpxeScript] = encodeLabelValue(req.GetIpxeScript())
	}
	if req.HasSshKeyGroupIds() {
		labels[labelSSHKeyGroupIDs] = encodeLabelValue(req.SshKeyGroupIds)
	}
	if len(req.Interfaces) > 0 {
		labels[labelInterfaces] = encodeLabelValue(req.Interfaces)
	}
	if req.HasInfinibandInterfaces() {
		labels[labelInfiniBandInterfaces] = encodeLabelValue(req.InfinibandInterfaces)
	}
	if req.HasNvLinkInterfaces() {
		labels[labelNVLinkInterfaces] = encodeLabelValue(req.NvLinkInterfaces)
	}
	if subnetID := req.Interfaces[0].GetSubnetId(); subnetID != "" {
		labels[labelSubnetID] = subnetID
	}
	if vpcPrefixID := req.Interfaces[0].GetVpcPrefixId(); vpcPrefixID != "" {
		labels[labelVPCPrefixID] = vpcPrefixID
	}

	createArgs := []string{
		"create",
		"--name", containerName,
		"--hostname", req.Name,
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

	containerID, err := runDocker(ctx, createArgs...)
	if err != nil {
		return nil, fmt.Errorf("create node container: %w", err)
	}

	inst, err := s.startInstanceContainer(ctx, containerID, instanceID, req)
	if err != nil {
		// Tear down the half-built container so a retry isn't blocked by the
		// stale name. WithoutCancel keeps cleanup running even if the parent
		// request context was canceled mid-flight.
		_ = s.removeNodeContainer(context.WithoutCancel(ctx), containerID)
		return nil, err
	}
	return inst, nil
}

// instanceID is unused: the container already carries it as a label, and the
// instance is rebuilt from `docker inspect` below. Kept in the signature, named
// _, so the call sites stay readable next to the other create/start helpers.
func (s *server) startInstanceContainer(ctx context.Context, containerID, _ string, req nicosdk.InstanceCreateRequest) (*nicosdk.Instance, error) {
	if _, err := runDocker(ctx, "start", containerID); err != nil {
		return nil, fmt.Errorf("start node container: %w", err)
	}
	if err := s.applyCloudConfig(ctx, containerID, req.GetUserData()); err != nil {
		return nil, err
	}
	return s.loadInstance(ctx, containerID)
}

func (s *server) getInstance(ctx context.Context, instanceID string) (*nicosdk.Instance, error) {
	containerID, err := s.findManagedContainerIDByInstanceID(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if containerID == "" {
		return nil, errNotFound
	}
	return s.loadInstance(ctx, containerID)
}

func (s *server) findInstanceByName(ctx context.Context, name string) (*nicosdk.Instance, error) {
	containerIDs, err := s.listManagedContainerIDs(ctx, map[string]string{
		labelInstanceName: name,
	})
	if err != nil {
		return nil, err
	}
	if len(containerIDs) == 0 {
		return nil, errNotFound
	}
	return s.loadInstance(ctx, containerIDs[0])
}

// listInstances enumerates the mock-managed containers and then lets
// loadInstance rebuild each API object from Docker inspect data plus the labels
// written at create time. Those labels are the mock's lightweight persistence
// layer for instance identity and metadata, so no separate instance store is
// needed here.
func (s *server) listInstances(ctx context.Context) ([]*nicosdk.Instance, error) {
	containerIDs, err := s.listManagedContainerIDs(ctx, nil)
	if err != nil {
		return nil, err
	}

	instances := make([]*nicosdk.Instance, 0, len(containerIDs))
	for _, containerID := range containerIDs {
		inst, err := s.loadInstance(ctx, containerID)
		if err != nil {
			slog.Warn("failed to inspect mock instance container", "containerID", containerID, "error", err)
			continue
		}
		instances = append(instances, inst)
	}

	return instances, nil
}

func (s *server) findManagedContainerIDByInstanceID(ctx context.Context, instanceID string) (string, error) {
	containerIDs, err := s.listManagedContainerIDs(ctx, map[string]string{
		labelInstanceID: instanceID,
	})
	if err != nil {
		return "", err
	}
	if len(containerIDs) == 0 {
		return "", nil
	}
	return containerIDs[0], nil
}

// listManagedContainerIDs treats Docker labels as the mock's lookup index for
// translating API concepts like instance ID/name into concrete containers.
func (s *server) listManagedContainerIDs(ctx context.Context, labels map[string]string) ([]string, error) {
	args := []string{"ps", "-aq", "--filter", "label=" + labelManaged + "=true"}
	for key, value := range labels {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		args = append(args, "--filter", "label="+key+"="+value)
	}

	out, err := runDocker(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("list managed containers: %w", err)
	}
	return strings.Fields(out), nil
}

// loadInstance rebuilds the public instance view from Docker inspect data
// instead of maintaining a separate mock database.
func (s *server) loadInstance(ctx context.Context, containerID string) (*nicosdk.Instance, error) {
	inspectResult, err := s.inspectContainer(ctx, containerID)
	if err != nil {
		return nil, err
	}

	labels := inspectResult.Config.Labels
	instanceID := labels[labelInstanceID]
	if instanceID == "" {
		return nil, errNotFound
	}

	inst := nicosdk.NewInstance()
	inst.SetId(instanceID)
	if name := labels[labelInstanceName]; name != "" {
		inst.SetName(name)
	}
	if siteID := firstNonEmpty(labels[labelSiteID], s.cfg.SiteID); siteID != "" {
		inst.SetSiteId(siteID)
	}
	if instanceTypeID := firstNonEmpty(labels[labelInstanceType], s.cfg.InstanceTypeID); instanceTypeID != "" {
		inst.SetInstanceTypeId(instanceTypeID)
	}
	if vpcID := firstNonEmpty(labels[labelVPCID], s.cfg.VPCID); vpcID != "" {
		inst.SetVpcId(vpcID)
	}
	if tenantID := firstNonEmpty(labels[labelTenantID], s.cfg.TenantID); tenantID != "" {
		inst.SetTenantId(tenantID)
	}
	if machineID := labels[labelMachineID]; machineID != "" {
		inst.SetMachineId(machineID)
	}
	if ipxeScript, err := decodeLabelValue[string](labels[labelIpxeScript]); err != nil {
		return nil, fmt.Errorf("decode iPXE script label: %w", err)
	} else if ipxeScript != "" {
		inst.SetIpxeScript(ipxeScript)
	}
	if sshKeyGroupIDs, err := decodeLabelValue[[]string](labels[labelSSHKeyGroupIDs]); err != nil {
		return nil, fmt.Errorf("decode SSH key group IDs label: %w", err)
	} else if len(sshKeyGroupIDs) > 0 {
		inst.SetSshKeyGroupIds(sshKeyGroupIDs)
	}
	instanceLabels := publicInstanceLabels(labels)
	if updatedLabels, ok := s.getInstanceLabels(instanceID); ok {
		instanceLabels = updatedLabels
	}
	if len(instanceLabels) > 0 {
		inst.SetLabels(instanceLabels)
	}

	tpmEKCertificate, err := s.tpmEKCertificate(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("read TPM EK certificate for %s: %w", containerID, err)
	}

	inst.SetStatus(instanceStatusForContainer(inspectResult.State.Running, tpmEKCertificate))
	if tpmEKCertificate != "" {
		inst.SetTpmEkCertificate(tpmEKCertificate)
	}

	if interfaces, err := decodeLabelValue[[]nicosdk.InterfaceCreateRequest](labels[labelInterfaces]); err != nil {
		return nil, fmt.Errorf("decode interfaces label: %w", err)
	} else if len(interfaces) > 0 {
		inst.SetInterfaces(instanceInterfacesFromCreateRequests(interfaces, s.firstContainerIPAddress(inspectResult)))
	} else if ipAddress := s.firstContainerIPAddress(inspectResult); ipAddress != "" || labels[labelSubnetID] != "" || labels[labelVPCPrefixID] != "" {
		inst.SetInterfaces([]nicosdk.Interface{legacyInstanceInterface(labels, ipAddress)})
	}
	if interfaces, err := decodeLabelValue[[]nicosdk.InfiniBandInterfaceCreateRequest](labels[labelInfiniBandInterfaces]); err != nil {
		return nil, fmt.Errorf("decode InfiniBand interfaces label: %w", err)
	} else if len(interfaces) > 0 {
		inst.SetInfinibandInterfaces(instanceInfiniBandInterfacesFromCreateRequests(interfaces))
	}
	if interfaces, err := decodeLabelValue[[]nicosdk.NVLinkInterfaceCreateRequest](labels[labelNVLinkInterfaces]); err != nil {
		return nil, fmt.Errorf("decode NVLink interfaces label: %w", err)
	} else if len(interfaces) > 0 {
		inst.SetNvLinkInterfaces(instanceNVLinkInterfacesFromCreateRequests(interfaces))
	}

	return inst, nil
}

func (s *server) inspectContainer(ctx context.Context, containerID string) (*dockerInspect, error) {
	out, err := runDocker(ctx, "inspect", containerID)
	if err != nil {
		return nil, fmt.Errorf("inspect container %s: %w", containerID, err)
	}

	var inspectResults []dockerInspect
	if err := json.Unmarshal([]byte(out), &inspectResults); err != nil {
		return nil, fmt.Errorf("decode docker inspect output: %w", err)
	}
	if len(inspectResults) == 0 {
		return nil, errNotFound
	}
	return &inspectResults[0], nil
}

func (s *server) firstContainerIPAddress(inspectResult *dockerInspect) string {
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

func (s *server) removeNodeContainer(ctx context.Context, containerID string) error {
	_, _ = runDocker(ctx, "stop", "--time", "10", containerID)
	if _, err := runDocker(ctx, "rm", "--force", containerID); err != nil {
		return fmt.Errorf("remove node container: %w", err)
	}
	return nil
}

func (s *server) setInstanceLabels(instanceID string, labels map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.instanceLabels == nil {
		s.instanceLabels = map[string]map[string]string{}
	}
	s.instanceLabels[instanceID] = cloneStringMap(labels)
}

func (s *server) getInstanceLabels(instanceID string) (map[string]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	labels, ok := s.instanceLabels[instanceID]
	return cloneStringMap(labels), ok
}

func (s *server) deleteInstanceLabels(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.instanceLabels, instanceID)
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

// tpmEKCertificate returns the container's EK certificate in the same
// base64-encoded DER form the higher layers expect from the real NICo/Forge
// APIs. Each read shells into the container and base64-encodes the file at
// `tpmEKCertPath` directly so the mock does not carry extra TPM-specific state.
//
// Empty output is treated specially: it means the container is up but the mock
// TPM state is not initialized yet, so callers should continue to see a
// pending-style status instead of a ready instance.
func (s *server) tpmEKCertificate(ctx context.Context, containerID string) (string, error) {
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

// instanceStatusForContainer intentionally distinguishes "container is running"
// from "instance is ready for TPM-aware consumers". A booted container without
// an EK certificate is still reported as Pending so the local legacy Forge path
// does not treat the instance as ready before the mock TPM state is available.
func instanceStatusForContainer(running bool, tpmEKCertificate string) nicosdk.InstanceStatus {
	switch {
	case !running:
		return nicosdk.InstanceStatus("Error")
	case strings.TrimSpace(tpmEKCertificate) == "":
		return nicosdk.InstanceStatus("Pending")
	default:
		return nicosdk.InstanceStatus("Ready")
	}
}

// applyCloudConfig implements the narrow cloud-config subset that local NICo
// consumers depend on: `write_files` plus string-valued `runcmd` entries.
func (s *server) applyCloudConfig(ctx context.Context, containerID, userData string) (retErr error) {
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
			if err := s.execInContainer(ctx, containerID, "mkdir", "-p", filepath.Dir(file.Path)); err != nil {
				return fmt.Errorf("create directory for %s: %w", file.Path, err)
			}
			if err := s.copyFileToContainer(ctx, containerID, file.Path, file.Content); err != nil {
				return fmt.Errorf("write %s: %w", file.Path, err)
			}
			if perms := strings.TrimSpace(file.Permissions); perms != "" {
				if err := s.execInContainer(ctx, containerID, "chmod", perms, file.Path); err != nil {
					return fmt.Errorf("chmod %s: %w", file.Path, err)
				}
			}
			if owner := strings.TrimSpace(file.Owner); owner != "" {
				if err := s.execInContainer(ctx, containerID, "chown", owner, file.Path); err != nil {
					return fmt.Errorf("chown %s: %w", file.Path, err)
				}
			}
		}

		for _, command := range cfg.RunCmd {
			if strings.TrimSpace(command) == "" {
				continue
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

// writeMockCloudInitStatus atomically updates the status read by the
// cloud-init shim in mock NICo node containers.
func (s *server) writeMockCloudInitStatus(ctx context.Context, containerID, status string) error {
	const writeStatus = `set -eu
status_dir=/var/lib/nico-mock-cloud-init
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

// copyFileToContainer writes content to targetPath via `docker exec` + stdin
// rather than `docker cp`. docker cp operates on the container's storage layer
// and cannot write into tmpfs mounts; the node image mounts /run, /tmp, and
// /var/lib/etcd as tmpfs, and CABPK writes files such as /run/kubeadm/kubeadm.yaml
// under them, so docker cp fails there with "Could not find the file". `docker
// exec` runs inside the container's mount namespace and sees those tmpfs mounts.
func (s *server) copyFileToContainer(ctx context.Context, containerID, targetPath, content string) error {
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerID,
		"sh", "-c", "cat > "+singleQuote(targetPath))
	cmd.Stdin = strings.NewReader(content)
	output, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			trimmed = err.Error()
		}
		return fmt.Errorf("write file into container: %s", trimmed)
	}
	return nil
}

// singleQuote wraps s in single quotes for safe use in a `sh -c` command,
// escaping any embedded single quotes.
func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (s *server) execInContainer(ctx context.Context, containerID string, args ...string) error {
	_, err := s.execInContainerOutput(ctx, containerID, args...)
	return err
}

func (s *server) execInContainerOutput(ctx context.Context, containerID string, args ...string) (string, error) {
	command := append([]string{"exec", containerID}, args...)
	out, err := runDocker(ctx, command...)
	if err != nil {
		return "", fmt.Errorf("exec in container: %w", err)
	}
	return out, nil
}

func runDocker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	output, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(output))
	if err != nil {
		if trimmed == "" {
			trimmed = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", strings.Join(args, " "), trimmed)
	}
	return trimmed, nil
}

func decodeJSONBody(r *http.Request, v any) error {
	// net/http closes the request body itself, so this is belt and braces and
	// there is nothing useful to do if it fails. Discarded explicitly rather
	// than implicitly, so the intent is visible and errcheck stays on.
	defer func() { _ = r.Body.Close() }()

	return json.NewDecoder(r.Body).Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		slog.Error("encode response", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(payload); err != nil {
		slog.Error("write response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func encodeLabelValue[T any](value T) string {
	payload, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("encode mock label value: %v", err))
	}
	return base64.StdEncoding.EncodeToString(payload)
}

func decodeLabelValue[T any](value string) (T, error) {
	var decoded T
	if strings.TrimSpace(value) == "" {
		return decoded, nil
	}

	payload, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return decoded, err
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return decoded, err
	}
	return decoded, nil
}

func instanceInterfacesFromCreateRequests(requests []nicosdk.InterfaceCreateRequest, ipAddress string) []nicosdk.Interface {
	interfaces := make([]nicosdk.Interface, 0, len(requests))
	for _, request := range requests {
		iface := nicosdk.NewInterface()
		if subnetID := request.GetSubnetId(); subnetID != "" {
			iface.SetSubnetId(subnetID)
		}
		if vpcPrefixID := request.GetVpcPrefixId(); vpcPrefixID != "" {
			iface.SetVpcPrefixId(vpcPrefixID)
		}
		if requestedIP := request.GetIpAddress(); requestedIP != "" {
			iface.SetRequestedIpAddress(requestedIP)
		}
		if physical, ok := request.GetIsPhysicalOk(); ok && physical != nil {
			iface.SetIsPhysical(*physical)
		}
		if device := request.GetDevice(); device != "" {
			iface.SetDevice(device)
		}
		if deviceInstance, ok := request.GetDeviceInstanceOk(); ok && deviceInstance != nil {
			iface.SetDeviceInstance(*deviceInstance)
		}
		if virtualFunctionID, ok := request.GetVirtualFunctionIdOk(); ok && virtualFunctionID != nil {
			iface.SetVirtualFunctionId(*virtualFunctionID)
		}
		if ipAddress != "" {
			iface.SetIpAddresses([]string{ipAddress})
		}
		interfaces = append(interfaces, *iface)
	}
	return interfaces
}

func legacyInstanceInterface(labels map[string]string, ipAddress string) nicosdk.Interface {
	iface := nicosdk.NewInterface()
	if subnetID := labels[labelSubnetID]; subnetID != "" {
		iface.SetSubnetId(subnetID)
	}
	if vpcPrefixID := labels[labelVPCPrefixID]; vpcPrefixID != "" {
		iface.SetVpcPrefixId(vpcPrefixID)
	}
	if ipAddress != "" {
		iface.SetIpAddresses([]string{ipAddress})
	}
	return *iface
}

func instanceInfiniBandInterfacesFromCreateRequests(requests []nicosdk.InfiniBandInterfaceCreateRequest) []nicosdk.InfiniBandInterface {
	interfaces := make([]nicosdk.InfiniBandInterface, 0, len(requests))
	for _, request := range requests {
		iface := nicosdk.NewInfiniBandInterface()
		if partitionID := request.GetPartitionId(); partitionID != "" {
			iface.SetPartitionId(partitionID)
		}
		if device := request.GetDevice(); device != "" {
			iface.SetDevice(device)
		}
		if deviceInstance, ok := request.GetDeviceInstanceOk(); ok && deviceInstance != nil {
			iface.SetDeviceInstance(*deviceInstance)
		}
		if physical, ok := request.GetIsPhysicalOk(); ok && physical != nil {
			iface.SetIsPhysical(*physical)
		}
		if virtualFunctionID, ok := request.GetVirtualFunctionIdOk(); ok && virtualFunctionID != nil {
			iface.SetVirtualFunctionId(*virtualFunctionID)
		}
		interfaces = append(interfaces, *iface)
	}
	return interfaces
}

func instanceNVLinkInterfacesFromCreateRequests(requests []nicosdk.NVLinkInterfaceCreateRequest) []nicosdk.NVLinkInterface {
	interfaces := make([]nicosdk.NVLinkInterface, 0, len(requests))
	for _, request := range requests {
		iface := nicosdk.NewNVLinkInterface()
		if partitionID := request.GetNvLinklogicalPartitionId(); partitionID != "" {
			iface.SetNvLinkLogicalPartitionId(partitionID)
		}
		if deviceInstance, ok := request.GetDeviceInstanceOk(); ok && deviceInstance != nil {
			iface.SetDeviceInstance(*deviceInstance)
		}
		interfaces = append(interfaces, *iface)
	}
	return interfaces
}

func publicInstanceLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}

	public := map[string]string{}
	for key, value := range labels {
		switch key {
		case labelManaged, labelInstanceID, labelInstanceName, labelSiteID, labelVPCID,
			labelTenantID, labelInstanceType, labelSubnetID, labelVPCPrefixID, labelMachineID,
			labelIpxeScript, labelSSHKeyGroupIDs, labelInterfaces, labelInfiniBandInterfaces,
			labelNVLinkInterfaces:
			continue
		default:
			public[key] = value
		}
	}

	if len(public) == 0 {
		return nil
	}

	return public
}
