//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	ControllerGofailWorkerBinaryEnv = "NKE_CONTROLLER_GOFAIL_E2E_WORKER_BINARY"
	// legacyWorkerBinaryEnv is a deprecated alias; #1399 removes it.
	legacyWorkerBinaryEnv           = "CWE_GOFAIL_E2E_WORKER_BINARY"
	ControllerGofailFailpointName   = "CWERunbookWorkerCrash"
	ControllerGofailWorkerCrashLine = "cwe-gofail-runbook-worker-crash"

	// ControllerGofailExecutionErrorFailpoint fails the named runbook immediately
	// instead of hanging it, for scenarios about retry budgets rather than
	// heartbeat loss.
	ControllerGofailExecutionErrorFailpoint = "CWERunbookExecutionError"
	controllerWorkerReadyTimeout            = 20 * time.Second
	controllerGofailRequestTimeout          = 5 * time.Second
	controllerWorkerReadyLog                = "Starting Temporal worker"
)

var (
	ControllerGofailNodeID      = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	ControllerGofailClusterID   = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	controllerGofailSiteID      = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	controllerGofailNodePoolID  = uuid.MustParse("44444444-4444-4444-4444-444444444444")
	controllerGofailNodeSetID   = uuid.MustParse("55555555-5555-5555-5555-555555555555")
	controllerGofailInstanceID  = uuid.MustParse("66666666-6666-6666-6666-666666666666")
	controllerGofailControlNode = "cwe-gofail-cp-0"
)

// ControllerWorkerBinaryConfig describes the production worker and its e2e dependencies.
type ControllerWorkerBinaryConfig struct {
	BinaryPath         string
	DBConnectionString string
	KubeconfigPath     string
	NodeAgent          config.NodeAgentConfig
	TemporalHostPort   string
	Namespace          string
	TaskQueue          string
	GofailHTTPAddr     string
	Env                []string
}

// ControllerWorkerBinary returns the production worker binary built by make e2e-gofail.
func ControllerWorkerBinary(t testing.TB) string {
	t.Helper()

	binaryPath := getenvWithLegacy(ControllerGofailWorkerBinaryEnv, legacyWorkerBinaryEnv)
	require.NotEmpty(t, binaryPath, "%s must point to the worker built by make e2e-gofail", ControllerGofailWorkerBinaryEnv)
	_, err := os.Stat(binaryPath)
	require.NoError(t, err, "controller worker binary does not exist")
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

// ManagementNamespace returns the management-plane namespace the worker started
// by StartControllerWorkerBinary reads and writes CAPI objects in. It comes from
// the same local controller config the worker is handed, so a test that seeds a
// Machine puts it where the worker looks for it.
func ManagementNamespace(t testing.TB) string {
	t.Helper()

	settings, err := controllerconfig.Load(filepath.Join(RepoRoot(t), "hack", "local-controller-config.yaml"))
	require.NoError(t, err)
	if settings.ManagementNamespace == "" {
		return constants.LegacyManagementNamespace
	}
	return settings.ManagementNamespace
}

// StartControllerWorkerBinary starts the production controller command against e2e-owned dependencies.
func StartControllerWorkerBinary(t testing.TB, cfg ControllerWorkerBinaryConfig) *Process {
	t.Helper()

	require.NotEmpty(t, cfg.DBConnectionString)
	require.NotEmpty(t, cfg.KubeconfigPath)
	require.NotEmpty(t, cfg.NodeAgent.Port)
	// Config profiles and runbooks come from the sanitized fixtures tree.
	profilesDir := filepath.Join(RepoRoot(t), "test", "fixtures", "profiles")
	_, err := os.Stat(filepath.Join(profilesDir, "configProfiles"))
	require.NoError(t, err, "expected fixture config profiles under %s", profilesDir)

	settings, err := controllerconfig.Load(filepath.Join(RepoRoot(t), "hack", "local-controller-config.yaml"))
	require.NoError(t, err)
	settings.Temporal.HostPort = cfg.TemporalHostPort
	settings.Temporal.Namespace = cfg.Namespace
	settings.Temporal.TaskQueue = cfg.TaskQueue
	settings.ProfilesDir = profilesDir
	settings.RunbooksDir = ""
	settings.Spire.SpiffeEnabled = false
	settings.Teleport.Enabled = false
	encoded, err := yaml.Marshal(settings)
	require.NoError(t, err)
	settingsPath := filepath.Join(t.TempDir(), "controller-config.yaml")
	require.NoError(t, os.WriteFile(settingsPath, encoded, 0o600))

	env := []string{
		"CONTROLLER_CONFIG_PATH=" + settingsPath,
		"DB_CONNECTION_STRING=" + cfg.DBConnectionString,
		"DB_MAX_OPEN_CONNECTIONS=4",
		"DB_QUERY_LOG_LEVEL=debug",
		"KUBECONFIG=" + cfg.KubeconfigPath,
		"NODE_AGENT_PORT=" + cfg.NodeAgent.Port,
		"NODE_HEALTH_CHECK_DISABLED=true",
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
		ReadyContains: controllerWorkerReadyLog,
		ReadyTimeout:  controllerWorkerReadyTimeout,
	})
}

// WriteKubeconfig returns client configuration for the normal worker bootstrap.
// StartAPIServer explains why a placeholder address no longer works.
func WriteKubeconfig(t testing.TB) string {
	t.Helper()

	return StartAPIServer(t)
}

// SetGofail enables a failpoint through gofail's HTTP control plane.
func SetGofail(t testing.TB, addr, name, term string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), controllerGofailRequestTimeout)
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

// SeedControllerGofailDB inserts the production workflow's minimum database state.
func SeedControllerGofailDB(t testing.TB, dsn string) {
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
	require.NoError(t, seedControllerGofailDB(context.Background(), db.NewDB(bunDB)))
}

// seedControllerGofailDB inserts the seed rows in one transaction. The worker is
// already running, and its node-pool controller deletes any non-current node
// set that has no nodes, so a set committed on its own can be gone before the
// node insert lands. Same reason as seedTeardownScenario.
func seedControllerGofailDB(ctx context.Context, database db.DB) error {
	return database.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return seedControllerGofailDBTx(ctx, tx)
	})
}

func seedControllerGofailDBTx(ctx context.Context, database db.Tx) error {
	cluster := db.Cluster{
		ID:                ControllerGofailClusterID,
		Name:              "cwe-gofail-cluster",
		KubernetesVersion: "1.30.0",
		Conditions: db.ClusterConditions{
			Provisioned: db.NewCondition(true, "seeded"),
		},
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{
		OrgID:  "cwe-gofail-org",
		SiteID: controllerGofailSiteID,
		VpcID:  uuid.New(),
	})
	if _, err := database.CreateCluster(ctx, &cluster); err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}

	nodePool := db.NodePool{
		ID:                   controllerGofailNodePoolID,
		ClusterID:            ControllerGofailClusterID,
		Name:                 "cwe-gofail-cp",
		NodeNamePrefix:       "cwe-gofail-cp",
		ControlPlane:         true,
		DesiredCount:         1,
		SystemReservedMemory: "16Gi",
		NodeLabels:           map[string]string{},
		Conditions: db.NodePoolConditions{
			Provisioned: db.NewCondition(true, "seeded"),
		},
	}
	nodePool.SetNico(db.NicoNodePoolProvider{
		InstanceTypeID:        controllerGofailInstanceID,
		Release:               "test",
		ConfigProfile:         "test",
		SubnetIDs:             []uuid.UUID{},
		VpcPrefixIDs:          []uuid.UUID{},
		SSHKeyGroupIDs:        []uuid.UUID{},
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	})
	if _, err := database.CreateNodePool(ctx, &nodePool); err != nil {
		return fmt.Errorf("create node pool: %w", err)
	}

	nodeSet := db.NodeSet{
		ID:                   controllerGofailNodeSetID,
		NodePoolID:           controllerGofailNodePoolID,
		DesiredCount:         1,
		KubernetesVersion:    "1.30.0",
		SystemReservedMemory: "16Gi",
	}
	nodeSet.SetNico(db.NicoNodePoolProvider{
		Release:               "test",
		ConfigProfile:         "test",
		SubnetIDs:             []uuid.UUID{},
		VpcPrefixIDs:          []uuid.UUID{},
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	})
	if _, err := database.CreateNodeSet(ctx, &nodeSet); err != nil {
		return fmt.Errorf("create node set: %w", err)
	}

	node := db.Node{
		ID:                     ControllerGofailNodeID,
		ClusterID:              ControllerGofailClusterID,
		NodePoolID:             controllerGofailNodePoolID,
		NodeSetID:              controllerGofailNodeSetID,
		ControlPlane:           true,
		Name:                   controllerGofailControlNode,
		IP:                     "127.0.0.1",
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
		Conditions: db.NodeConditions{
			Joined:          db.NewCondition(true, "seeded"),
			KubernetesReady: db.NewCondition(true, "seeded"),
			NodeAgentReady:  db.NewCondition(true, "seeded"),
		},
	}
	node.SetNico(db.NicoNodeProvider{
		InstanceTypeID: controllerGofailInstanceID,
	})
	if _, err := database.CreateNode(ctx, &node); err != nil {
		return fmt.Errorf("create node: %w", err)
	}

	return nil
}
