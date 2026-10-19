// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	"github.com/dsx-ai-factory/nvke/agent/pkg/utils"
)

// spireOSImageConf is os-image/files/spire-agent.conf.tmpl as rendered by
// spire-configure.sh.
const spireOSImageConf = `# Rendered by /usr/lib/nvke/spire-configure.sh. Placeholders are @NAME@ so the
# image never ships a live server address or trust domain.

agent {
  data_dir = "/var/lib/spire-agent"
  log_level = "INFO"
  server_address = "10.0.0.1"
  server_port = "8081"
  socket_path = "/tmp/spire-agent/public/api.sock"
  trust_domain = "example.org"
  trust_bundle_path = "/etc/spire-agent/trust-bundle.pem"
}

plugins {
  NodeAttestor "tpm" {
    plugin_cmd = "/usr/local/lib/spire-agent/tpm_attestor_agent"
    plugin_checksum = "abc123"
    plugin_data {}
  }
  KeyManager "disk" {
    plugin_data {
      directory = "/var/lib/spire-agent/keys"
    }
  }
  WorkloadAttestor "unix" {
    plugin_data {}
  }
}
`

// spireCAPIConf is controller/internal/clients/capi/capi_spire_agent.conf.tmpl
// rendered with the join token attestor.
const spireCAPIConf = `agent {
    data_dir = "/var/lib/spire-agent"
    log_level = "DEBUG"

    server_address = "10.0.0.1"
    server_port = "8081"

    trust_domain = "example.org"
    trust_bundle_path = "/etc/spire-agent/trust-bundle.pem"

    join_token = "example-token"
}

plugins {
    KeyManager "disk" {
        plugin_data {
            directory = "/var/lib/spire-agent"
        }
    }
    NodeAttestor "join_token" {
        plugin_data {}
    }

    WorkloadAttestor "unix" {
        plugin_data {}
    }
}
`

// spireLaunchLayerConf is
// controller/internal/clients/launchlayer/launchlayer_spire_agent.conf.tmpl
// rendered with the dsx_imds attestor.
const spireLaunchLayerConf = `agent {
    data_dir = "/var/lib/spire-agent"
    log_level = "DEBUG"

    server_address = "10.0.0.1"
    server_port = "8081"

    trust_domain = "example.org"
    trust_bundle_path = "/etc/spire-agent/trust-bundle.pem"
}

plugins {
    KeyManager "disk" {
        plugin_data {
            directory = "/var/lib/spire-agent"
        }
    }
    # The DSX node image provides this external plugin and its checksum through
    # the SPIRE agent environment file. The audience identifies this trust
    # domain's SPIRE server and must match the server plugin configuration.
    NodeAttestor "dsx_imds" {
        plugin_cmd = "/usr/local/lib/spire-agent/dsx-imds-node-attestor-agent"
        plugin_checksum = "${DSX_IMDS_PLUGIN_CHECKSUM}"
        plugin_data {
            endpoint = "http://169.254.169.254"
            audience = "spiffe://example.org/spire/server"
        }
    }

    WorkloadAttestor "unix" {
        plugin_data {}
    }
}
`

const spireTestEnv = "SPIRE_SERVER_ADDRESS=10.0.0.1\nSPIRE_SERVER_PORT=8081\nSPIRE_TRUST_DOMAIN=example.org\n"

// spireCommandRunner records commands and answers systemctl show from a queue
// of unit states; the last state repeats.
type spireCommandRunner struct {
	calls      []string
	restartErr error
	showErr    error
	states     []func() string
}

func (r *spireCommandRunner) Run(name string, args []string, _ *utils.CommandOptions) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	return nil, r.restartErr
}

func (r *spireCommandRunner) RunSplitOutput(name string, args []string, _ *utils.CommandOptions) (utils.CommandOutput, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	if r.showErr != nil {
		return utils.CommandOutput{}, r.showErr
	}
	if len(r.states) == 0 {
		return utils.CommandOutput{Stdout: []byte(unitState("inactive", 0))}, nil
	}
	state := r.states[0]
	if len(r.states) > 1 {
		r.states = r.states[1:]
	}
	return utils.CommandOutput{Stdout: []byte(state())}, nil
}

func (*spireCommandRunner) RunDetached(string, []string, *utils.CommandOptions) error {
	return nil
}

func (r *spireCommandRunner) restarts() int {
	n := 0
	for _, call := range r.calls {
		if call == "systemctl restart spire-agent" {
			n++
		}
	}
	return n
}

func unitState(state string, since int64) string {
	timestamp := ""
	if since != 0 {
		timestamp = fmt.Sprintf("@%d", since)
	}
	return fmt.Sprintf("ActiveState=%s\nActiveEnterTimestamp=%s\n", state, timestamp)
}

// activeNow reports an active unit started now, which is after any config write.
func activeNow() string { return unitState("active", time.Now().Unix()+1) }

func failedNow() string { return unitState("failed", time.Now().Unix()) }

func validSVID(context.Context, string) (*x509svid.SVID, error) {
	return &x509svid.SVID{
		ID:           spiffeid.RequireFromString("spiffe://example.org/node/worker-1"),
		Certificates: []*x509.Certificate{{NotAfter: time.Now().Add(time.Hour)}},
	}, nil
}

func newSpireAgentTestServer(t *testing.T, conf string, env *string, runner *spireCommandRunner) *NodeAgentServer {
	t.Helper()
	root := t.TempDir()
	writeSpireTestFile(t, filepath.Join(root, spireAgentConfigPath), conf)
	if env != nil {
		writeSpireTestFile(t, filepath.Join(root, spireAgentEnvPath), *env)
	}
	return &NodeAgentServer{
		Runner:                 runner,
		RootPath:               root,
		SPIFFESocketPath:       "unix:///tmp/spire-agent/public/api.sock",
		kubeadmFileOps:         fileOps{chown: func(string, int, int) error { return nil }},
		spireAgentPollInterval: time.Millisecond,
		spireAgentWaitTimeout:  time.Second,
		fetchX509SVID:          validSVID,
	}
}

func writeSpireTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	// Put the write in the past so a unit started "now" counts as started after it.
	past := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(path, past, past))
}

func assertSpireBackup(t *testing.T, s *NodeAgentServer, path, original string, rewritten bool) {
	t.Helper()
	backup := filepath.Join(s.RootPath, path+".bak")
	if !rewritten {
		assert.NoFileExists(t, backup)
		return
	}
	assert.Equal(t, original, readSpireTestFile(t, s, path+".bak"))
	backupInfo, err := os.Stat(backup)
	require.NoError(t, err)
	info, err := os.Stat(filepath.Join(s.RootPath, path))
	require.NoError(t, err)
	assert.Equal(t, info.Mode().Perm(), backupInfo.Mode().Perm())
}

func readSpireTestFile(t *testing.T, s *NodeAgentServer, path string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(s.RootPath, path))
	require.NoError(t, err)
	return string(content)
}

func TestRewriteSpireAgentServerPreservesTemplateShapes(t *testing.T) {
	tests := []struct {
		name string
		conf string
	}{
		{name: "os image template", conf: spireOSImageConf},
		{name: "capi template", conf: spireCAPIConf},
		{name: "launchlayer template", conf: spireLaunchLayerConf},
		{name: "bare number port", conf: strings.Replace(spireCAPIConf, `server_port = "8081"`, `server_port = 8081`, 1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			address, port, err := parseSpireAgentServer([]byte(tt.conf))
			require.NoError(t, err)
			assert.Equal(t, "10.0.0.1", address)
			assert.Equal(t, uint32(8081), port)

			got := string(rewriteSpireAgentServer([]byte(tt.conf), "spire.example.com", 443))

			want := strings.Replace(tt.conf, `server_address = "10.0.0.1"`, `server_address = "spire.example.com"`, 1)
			want = strings.Replace(want, `server_port = "8081"`, `server_port = "443"`, 1)
			want = strings.Replace(want, `server_port = 8081`, `server_port = "443"`, 1)
			assert.Equal(t, want, got)
		})
	}
}

func TestParseSpireAgentServerRejectsAmbiguousConfig(t *testing.T) {
	tests := []struct {
		name string
		conf string
	}{
		{name: "missing server_address", conf: strings.Replace(spireCAPIConf, `server_address = "10.0.0.1"`, "", 1)},
		{name: "missing server_port", conf: strings.Replace(spireCAPIConf, `server_port = "8081"`, "", 1)},
		{name: "duplicate server_address", conf: spireCAPIConf + "server_address = \"10.0.0.2\"\n"},
		{name: "duplicate server_port", conf: spireCAPIConf + "server_port = 8082\n"},
		{name: "unrendered port placeholder", conf: strings.Replace(spireCAPIConf, `"8081"`, `"@SPIRE_SERVER_PORT@"`, 1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &spireCommandRunner{}
			server := newSpireAgentTestServer(t, tt.conf, nil, runner)

			_, err := server.ReconfigureSpireAgent(context.Background(), &pb.ReconfigureSpireAgentRequest{
				ServerAddress: "10.0.0.2",
				ServerPort:    8081,
			})
			assert.Equal(t, codes.FailedPrecondition, status.Code(err))

			_, err = server.GetSpireAgentStatus(context.Background(), &pb.GetSpireAgentStatusRequest{})
			assert.Equal(t, codes.FailedPrecondition, status.Code(err))
			assert.Empty(t, runner.calls)
		})
	}
}

func TestReconfigureSpireAgentRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		req  *pb.ReconfigureSpireAgentRequest
	}{
		{name: "nil request", req: nil},
		{name: "empty address", req: &pb.ReconfigureSpireAgentRequest{ServerPort: 8081}},
		{name: "zero port", req: &pb.ReconfigureSpireAgentRequest{ServerAddress: "10.0.0.2"}},
		{name: "port above range", req: &pb.ReconfigureSpireAgentRequest{ServerAddress: "10.0.0.2", ServerPort: 65536}},
		{name: "whitespace", req: &pb.ReconfigureSpireAgentRequest{ServerAddress: "spire .example.com", ServerPort: 8081}},
		{name: "quote", req: &pb.ReconfigureSpireAgentRequest{ServerAddress: `spire"x`, ServerPort: 8081}},
		{name: "backslash", req: &pb.ReconfigureSpireAgentRequest{ServerAddress: `spire\x`, ServerPort: 8081}},
		{name: "dollar", req: &pb.ReconfigureSpireAgentRequest{ServerAddress: "$(reboot)", ServerPort: 8081}},
		{name: "backtick", req: &pb.ReconfigureSpireAgentRequest{ServerAddress: "`reboot`", ServerPort: 8081}},
		{name: "newline", req: &pb.ReconfigureSpireAgentRequest{ServerAddress: "spire\nx", ServerPort: 8081}},
		{name: "equals", req: &pb.ReconfigureSpireAgentRequest{ServerAddress: "a=b", ServerPort: 8081}},
		{name: "hash", req: &pb.ReconfigureSpireAgentRequest{ServerAddress: "spire#x", ServerPort: 8081}},
		{name: "host and port", req: &pb.ReconfigureSpireAgentRequest{ServerAddress: "spire:8081", ServerPort: 8081}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &spireCommandRunner{}
			server := newSpireAgentTestServer(t, spireCAPIConf, nil, runner)

			_, err := server.ReconfigureSpireAgent(context.Background(), tt.req)

			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Empty(t, runner.calls)
			assert.Equal(t, spireCAPIConf, readSpireTestFile(t, server, spireAgentConfigPath))
		})
	}
}

func TestReconfigureSpireAgentWithoutSocket(t *testing.T) {
	runner := &spireCommandRunner{}
	server := newSpireAgentTestServer(t, spireCAPIConf, nil, runner)
	server.SPIFFESocketPath = ""

	_, err := server.ReconfigureSpireAgent(context.Background(), &pb.ReconfigureSpireAgentRequest{ServerAddress: "10.0.0.2", ServerPort: 8081})

	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Empty(t, runner.calls)
	assert.Equal(t, spireCAPIConf, readSpireTestFile(t, server, spireAgentConfigPath))
}

func TestReconfigureSpireAgentMissingConfig(t *testing.T) {
	runner := &spireCommandRunner{}
	server := newSpireAgentTestServer(t, spireCAPIConf, nil, runner)
	require.NoError(t, os.Remove(filepath.Join(server.RootPath, spireAgentConfigPath)))

	_, err := server.ReconfigureSpireAgent(context.Background(), &pb.ReconfigureSpireAgentRequest{ServerAddress: "10.0.0.2", ServerPort: 8081})

	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Empty(t, runner.calls)
}

func TestReconfigureSpireAgentOutcomes(t *testing.T) {
	staleEnv := "SPIRE_SERVER_ADDRESS=10.0.0.9\nSPIRE_SERVER_PORT=8081\nSPIRE_TRUST_DOMAIN=example.org\n"
	movedEnv := "SPIRE_SERVER_ADDRESS=spire.example.com\nSPIRE_SERVER_PORT=443\nSPIRE_TRUST_DOMAIN=example.org\n"
	movedConf := strings.Replace(spireOSImageConf, `"10.0.0.1"`, `"spire.example.com"`, 1)
	movedConf = strings.Replace(movedConf, `"8081"`, `"443"`, 1)

	tests := []struct {
		name         string
		address      string
		port         uint32
		env          *string
		states       []func() string
		wantOutcome  pb.ReconfigureSpireAgentResponse_Outcome
		wantRestarts int
		wantConf     string
		wantEnv      *string
		wantErrorMsg string
	}{
		{
			name:         "same server with valid SVID is unchanged",
			address:      "10.0.0.1",
			port:         8081,
			env:          new(spireTestEnv),
			states:       []func() string{activeNow},
			wantOutcome:  pb.ReconfigureSpireAgentResponse_OUTCOME_UNCHANGED,
			wantConf:     spireOSImageConf,
			wantEnv:      new(spireTestEnv),
			wantRestarts: 0,
		},
		{
			name:         "same server with stale env file only rewrites env",
			address:      "10.0.0.1",
			port:         8081,
			env:          new(staleEnv),
			states:       []func() string{activeNow},
			wantOutcome:  pb.ReconfigureSpireAgentResponse_OUTCOME_UNCHANGED,
			wantConf:     spireOSImageConf,
			wantEnv:      new(spireTestEnv),
			wantRestarts: 0,
		},
		{
			name:         "same server with inactive agent restarts",
			address:      "10.0.0.1",
			port:         8081,
			states:       []func() string{failedNow, activeNow},
			wantOutcome:  pb.ReconfigureSpireAgentResponse_OUTCOME_APPLIED,
			wantConf:     spireOSImageConf,
			wantRestarts: 1,
		},
		{
			name:    "new server is applied after the agent exits once",
			address: "spire.example.com",
			port:    443,
			env:     new(spireTestEnv),
			// Pre-restart reads are skipped because the config differs; the
			// agent fails once and activates again before serving an SVID.
			states:       []func() string{failedNow, func() string { return unitState("activating", 0) }, activeNow},
			wantOutcome:  pb.ReconfigureSpireAgentResponse_OUTCOME_APPLIED,
			wantConf:     movedConf,
			wantEnv:      new(movedEnv),
			wantRestarts: 1,
		},
		{
			name:         "new server without env file leaves env absent",
			address:      "spire.example.com",
			port:         443,
			states:       []func() string{activeNow},
			wantOutcome:  pb.ReconfigureSpireAgentResponse_OUTCOME_APPLIED,
			wantConf:     movedConf,
			wantRestarts: 1,
		},
		{
			name:         "agent that never recovers times out as failed",
			address:      "spire.example.com",
			port:         443,
			env:          new(spireTestEnv),
			states:       []func() string{failedNow},
			wantOutcome:  pb.ReconfigureSpireAgentResponse_OUTCOME_FAILED,
			wantConf:     movedConf,
			wantEnv:      new(movedEnv),
			wantRestarts: 1,
			wantErrorMsg: "spire-agent is failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &spireCommandRunner{states: tt.states}
			server := newSpireAgentTestServer(t, spireOSImageConf, tt.env, runner)
			server.spireAgentWaitTimeout = 50 * time.Millisecond

			resp, err := server.ReconfigureSpireAgent(context.Background(), &pb.ReconfigureSpireAgentRequest{
				ServerAddress: tt.address,
				ServerPort:    tt.port,
			})

			require.NoError(t, err)
			assert.Equal(t, tt.wantOutcome, resp.GetOutcome())
			assert.Contains(t, resp.GetErrorMessage(), tt.wantErrorMsg)
			assert.Equal(t, tt.wantRestarts, runner.restarts())
			assert.Equal(t, tt.wantConf, readSpireTestFile(t, server, spireAgentConfigPath))
			info, err := os.Stat(filepath.Join(server.RootPath, spireAgentConfigPath))
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
			if tt.wantEnv == nil {
				assert.NoFileExists(t, filepath.Join(server.RootPath, spireAgentEnvPath))
			} else {
				assert.Equal(t, *tt.wantEnv, readSpireTestFile(t, server, spireAgentEnvPath))
			}
			// A file that was rewritten leaves its previous content, with the same mode, in <path>.bak.
			assertSpireBackup(t, server, spireAgentConfigPath, spireOSImageConf, tt.wantConf != spireOSImageConf)
			if tt.env != nil {
				assertSpireBackup(t, server, spireAgentEnvPath, *tt.env, *tt.wantEnv != *tt.env)
			}
		})
	}
}

func TestReconfigureSpireAgentContextCanceled(t *testing.T) {
	runner := &spireCommandRunner{states: []func() string{failedNow}}
	server := newSpireAgentTestServer(t, spireOSImageConf, nil, runner)
	server.spireAgentPollInterval = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := server.ReconfigureSpireAgent(ctx, &pb.ReconfigureSpireAgentRequest{ServerAddress: "spire.example.com", ServerPort: 443})

	assert.Equal(t, codes.DeadlineExceeded, status.Code(err))
}

func TestReconfigureSpireAgentExpiredBeforeLock(t *testing.T) {
	runner := &spireCommandRunner{states: []func() string{activeNow}}
	server := newSpireAgentTestServer(t, spireOSImageConf, nil, runner)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := server.ReconfigureSpireAgent(ctx, &pb.ReconfigureSpireAgentRequest{ServerAddress: "spire.example.com", ServerPort: 443})

	assert.Equal(t, codes.Canceled, status.Code(err))
	assert.Empty(t, runner.calls)
	assert.Equal(t, spireOSImageConf, readSpireTestFile(t, server, spireAgentConfigPath))
}

func TestReconfigureSpireAgentRestartFailure(t *testing.T) {
	runner := &spireCommandRunner{restartErr: errors.New("exit status 1")}
	server := newSpireAgentTestServer(t, spireOSImageConf, nil, runner)

	resp, err := server.ReconfigureSpireAgent(context.Background(), &pb.ReconfigureSpireAgentRequest{ServerAddress: "spire.example.com", ServerPort: 443})

	require.NoError(t, err)
	assert.Equal(t, pb.ReconfigureSpireAgentResponse_OUTCOME_FAILED, resp.GetOutcome())
	assert.Contains(t, resp.GetErrorMessage(), "restart spire-agent")
}

func TestGetSpireAgentStatus(t *testing.T) {
	expired := func(context.Context, string) (*x509svid.SVID, error) {
		return &x509svid.SVID{
			ID:           spiffeid.RequireFromString("spiffe://example.org/node/worker-1"),
			Certificates: []*x509.Certificate{{NotAfter: time.Now().Add(-time.Minute)}},
		}, nil
	}
	fetchFails := func(context.Context, string) (*x509svid.SVID, error) {
		return nil, errors.New("no identity issued")
	}

	tests := []struct {
		name        string
		state       func() string
		fetch       func(context.Context, string) (*x509svid.SVID, error)
		noSocket    bool
		wantApplied bool
		wantValid   bool
		wantSVID    bool
		wantMessage string
	}{
		{name: "inactive agent", state: func() string { return unitState("inactive", 0) }, fetch: validSVID, wantMessage: "spire-agent is inactive"},
		{
			name:        "agent started before config write",
			state:       func() string { return unitState("active", time.Now().Add(-time.Hour).Unix()) },
			fetch:       validSVID,
			wantMessage: "started before",
		},
		{name: "mtls disabled", state: activeNow, fetch: validSVID, noSocket: true, wantApplied: true, wantMessage: "socket is not configured"},
		{name: "svid fetch fails", state: activeNow, fetch: fetchFails, wantApplied: true, wantMessage: "no identity issued"},
		{name: "expired svid", state: activeNow, fetch: expired, wantApplied: true, wantSVID: true, wantMessage: "expired"},
		{name: "valid svid", state: activeNow, fetch: validSVID, wantApplied: true, wantValid: true, wantSVID: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &spireCommandRunner{states: []func() string{tt.state}}
			server := newSpireAgentTestServer(t, spireCAPIConf, nil, runner)
			server.fetchX509SVID = tt.fetch
			if tt.noSocket {
				server.SPIFFESocketPath = ""
			}

			resp, err := server.GetSpireAgentStatus(context.Background(), &pb.GetSpireAgentStatusRequest{})

			require.NoError(t, err)
			assert.Equal(t, "10.0.0.1", resp.GetServerAddress())
			assert.Equal(t, uint32(8081), resp.GetServerPort())
			assert.Equal(t, tt.wantApplied, resp.GetConfigApplied())
			assert.Equal(t, tt.wantValid, resp.GetHasValidSvid())
			assert.Contains(t, resp.GetMessage(), tt.wantMessage)
			if tt.wantValid {
				assert.Empty(t, resp.GetMessage())
			}
			if tt.wantSVID {
				assert.Equal(t, "spiffe://example.org/node/worker-1", resp.GetSvidSpiffeId())
				assert.NotZero(t, resp.GetSvidExpiresAtUnixSeconds())
			}
			assert.Equal(t, []string{
				"systemctl show spire-agent --property=ActiveState --property=ActiveEnterTimestamp --timestamp=unix",
			}, runner.calls)
		})
	}
}

func TestGetSpireAgentStatusSystemctlError(t *testing.T) {
	runner := &spireCommandRunner{showErr: errors.New("exit status 1")}
	server := newSpireAgentTestServer(t, spireCAPIConf, nil, runner)

	_, err := server.GetSpireAgentStatus(context.Background(), &pb.GetSpireAgentStatusRequest{})

	assert.Equal(t, codes.Internal, status.Code(err))
}
