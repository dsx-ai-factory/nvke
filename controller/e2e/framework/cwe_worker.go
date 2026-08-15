//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package framework

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
)

const (
	CWEGofailWorkerBinaryEnv = "CWE_GOFAIL_E2E_WORKER_BINARY"
	CWEGofailFailpointName   = "CWERunbookWorkerCrash"
	CWEGofailWorkerCrashLine = "cwe-gofail-runbook-worker-crash"

	// CWEGofailExecutionErrorFailpoint fails the named runbook immediately
	// instead of hanging it, for scenarios about retry budgets rather than
	// heartbeat loss.
	CWEGofailExecutionErrorFailpoint = "CWERunbookExecutionError"
	cweWorkerReadyTimeout            = 20 * time.Second
	cweGofailRequestTimeout          = 5 * time.Second
	cweWorkerReadyLog                = "Starting Temporal worker"
)

var (
	CWEGofailNodeID      = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	CWEGofailClusterID   = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	cweGofailSiteID      = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	cweGofailNodePoolID  = uuid.MustParse("44444444-4444-4444-4444-444444444444")
	cweGofailNodeSetID   = uuid.MustParse("55555555-5555-5555-5555-555555555555")
	cweGofailInstanceID  = uuid.MustParse("66666666-6666-6666-6666-666666666666")
	cweGofailControlNode = "cwe-gofail-cp-0"
)

// CWEWorkerBinaryConfig describes the production worker and its e2e dependencies.
type CWEWorkerBinaryConfig struct {
	BinaryPath         string
	DBConnectionString string
	KubeconfigPath     string
	NodeAgentPort      string
	TemporalHostPort   string
	Namespace          string
	TaskQueue          string
	GofailHTTPAddr     string
	Env                []string
}

// CWEWorkerBinary returns the production worker binary built by make e2e-gofail.
func CWEWorkerBinary(t testing.TB) string {
	t.Helper()

	binaryPath := os.Getenv(CWEGofailWorkerBinaryEnv)
	require.NotEmpty(t, binaryPath, "%s must point to the worker built by make e2e-gofail", CWEGofailWorkerBinaryEnv)
	_, err := os.Stat(binaryPath)
	require.NoError(t, err, "CWE worker binary does not exist")
	return binaryPath
}

// RepoRoot returns the module root containing go.mod.
func RepoRoot(t testing.TB) string {
	t.Helper()

	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "could not find go.mod from %s", dir)
		dir = parent
	}
}

// FreeLocalhostAddress returns an available localhost TCP address.
func FreeLocalhostAddress(t testing.TB) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, ln.Close())
	}()
	return ln.Addr().String()
}

// StartCWEWorkerBinary starts the production CWE command against e2e-owned dependencies.
func StartCWEWorkerBinary(t testing.TB, cfg CWEWorkerBinaryConfig) *Process {
	t.Helper()

	require.NotEmpty(t, cfg.DBConnectionString)
	require.NotEmpty(t, cfg.KubeconfigPath)
	require.NotEmpty(t, cfg.NodeAgentPort)
	// Fleet profiles are no longer baked into the worker binary (NKX-13910). Point
	// migrations that load osProfiles/metadata.yaml at the sanitized fixtures tree.
	profilesDir := filepath.Join(RepoRoot(t), "test", "fixtures", "profiles")
	_, err := os.Stat(filepath.Join(profilesDir, "osProfiles", "metadata.yaml"))
	require.NoError(t, err, "expected fixture OS profile metadata under %s", profilesDir)

	env := []string{
		"DB_CONNECTION_STRING=" + cfg.DBConnectionString,
		"DB_MAX_OPEN_CONNECTIONS=4",
		"DB_QUERY_LOG_LEVEL=debug",
		"KUBECONFIG=" + cfg.KubeconfigPath,
		"NODE_AGENT_PORT=" + cfg.NodeAgentPort,
		"TEMPORAL_HOST_PORT=" + cfg.TemporalHostPort,
		"TEMPORAL_NAMESPACE=" + cfg.Namespace,
		"TEMPORAL_TASK_QUEUE=" + cfg.TaskQueue,
		"PROFILES_DIR=" + profilesDir,
		"CONTROLLER_MANAGER_DISABLED=true",
		"NODE_HEALTH_CHECK_DISABLED=true",
		"SPIFFE_ENABLED=false",
		"ENABLE_CAPI=false",
		"ENABLE_FORGE_CAPI=false",
		"TELEPORT_ENABLED=false",
		"FWE_API_KEY=",
		"GOFAIL_FAILPOINTS=",
		"GOFAIL_HTTP=",
		"SERVER_LISTEN_ADDRESS=127.0.0.1:0",
		"LOG_LEVEL=info",
	}
	// Mirror production: supply the DB password via CNPG_SUPERUSER_PASSWORD so config
	// validation accepts the default credentials in DB_CONNECTION_STRING.
	if u, err := url.Parse(cfg.DBConnectionString); err == nil {
		if password, ok := u.User.Password(); ok && password != "" {
			env = append(env, "CNPG_SUPERUSER_PASSWORD="+password)
		}
	}
	if cfg.GofailHTTPAddr != "" {
		env = append(env, "GOFAIL_HTTP="+cfg.GofailHTTPAddr)
	}
	env = append(env, cfg.Env...)

	return StartProcess(t, ProcessConfig{
		Command:       cfg.BinaryPath,
		WorkingDir:    RepoRoot(t),
		Env:           env,
		ReadyContains: cweWorkerReadyLog,
		ReadyTimeout:  cweWorkerReadyTimeout,
	})
}

// WriteKubeconfig writes enough client configuration for the normal worker bootstrap.
// The tested workflow does not call the Kubernetes API.
func WriteKubeconfig(t testing.TB) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kubeconfig")
	contents := []byte(`apiVersion: v1
kind: Config
clusters:
- name: gofail-e2e
  cluster:
    server: http://127.0.0.1:1
contexts:
- name: gofail-e2e
  context:
    cluster: gofail-e2e
    user: gofail-e2e
current-context: gofail-e2e
users:
- name: gofail-e2e
  user: {}
`)
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	return path
}

// SetGofail enables a failpoint through gofail's HTTP control plane.
func SetGofail(t testing.TB, addr, name, term string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), cweGofailRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+addr+"/"+name, strings.NewReader(term))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, resp.Body.Close())
	}()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "gofail response: %s", body)
}

// SeedCWEGofailDB inserts the production workflow's minimum database state.
func SeedCWEGofailDB(t testing.TB, dsn string) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bunDB, err := db.NewBunDB(config.Config{
		DBConnectionString:   dsn,
		DBMaxOpenConnections: 2,
		DBQueryLogLevel:      "info",
	}, logger)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, bunDB.Close())
	}()
	require.NoError(t, seedCWEGofailDB(context.Background(), db.NewDB(bunDB)))
}

func seedCWEGofailDB(ctx context.Context, database db.DB) error {
	if _, err := database.RegisterSite(ctx, &db.Site{
		ID:        cweGofailSiteID,
		VIPRanges: []string{},
	}); err != nil {
		return fmt.Errorf("create site: %w", err)
	}

	if _, err := database.CreateCluster(ctx, &db.Cluster{
		ID:                CWEGofailClusterID,
		Name:              "cwe-gofail-cluster",
		OrgID:             "cwe-gofail-org",
		SiteID:            cweGofailSiteID,
		VpcID:             uuid.New(),
		KubernetesVersion: "1.30.0",
		Conditions: db.ClusterConditions{
			Provisioned: db.NewCondition(true, "seeded"),
		},
	}); err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}

	if _, err := database.CreateNodePool(ctx, &db.NodePool{
		ID:                    cweGofailNodePoolID,
		ClusterID:             CWEGofailClusterID,
		Name:                  "cwe-gofail-cp",
		NodeNamePrefix:        "cwe-gofail-cp",
		ControlPlane:          true,
		DesiredCount:          1,
		InstanceTypeID:        cweGofailInstanceID,
		OSProfile:             "test",
		ConfigProfile:         "test",
		SystemReservedMemory:  "16Gi",
		SubnetIDs:             []uuid.UUID{},
		VpcPrefixIDs:          []uuid.UUID{},
		SSHKeyGroupIDs:        []uuid.UUID{},
		NodeLabels:            []string{},
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
		Conditions: db.NodePoolConditions{
			Provisioned: db.NewCondition(true, "seeded"),
		},
	}); err != nil {
		return fmt.Errorf("create node pool: %w", err)
	}

	if _, err := database.CreateNodeSet(ctx, &db.NodeSet{
		ID:                    cweGofailNodeSetID,
		NodePoolID:            cweGofailNodePoolID,
		DesiredCount:          1,
		OSProfile:             "test",
		ConfigProfile:         "test",
		KubernetesVersion:     "1.30.0",
		SystemReservedMemory:  "16Gi",
		SubnetIDs:             []uuid.UUID{},
		VpcPrefixIDs:          []uuid.UUID{},
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	}); err != nil {
		return fmt.Errorf("create node set: %w", err)
	}

	if _, err := database.CreateNode(ctx, &db.Node{
		ID:              CWEGofailNodeID,
		ClusterID:       CWEGofailClusterID,
		NodePoolID:      cweGofailNodePoolID,
		NodeSetID:       cweGofailNodeSetID,
		ControlPlane:    true,
		InstanceTypeID:  cweGofailInstanceID,
		Name:            cweGofailControlNode,
		ForgeInstanceID: "cwe-gofail-instance",
		ForgeMachineID:  "cwe-gofail-machine",
		IP:              "127.0.0.1",
		Conditions: db.NodeConditions{
			Joined:          db.NewCondition(true, "seeded"),
			KubernetesReady: db.NewCondition(true, "seeded"),
			NodeAgentReady:  db.NewCondition(true, "seeded"),
		},
	}); err != nil {
		return fmt.Errorf("create node: %w", err)
	}

	return nil
}
