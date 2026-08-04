package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/NVIDIA/nke/node-agent/pkg/agent/proto"
	nkeutils "github.com/NVIDIA/nke/node-agent/pkg/utils"
)

func TestParseLoopbackIPv4VIPs(t *testing.T) {
	output := `1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever
1: lo    inet 192.168.100.0/32 scope host lo\       valid_lft forever preferred_lft forever
`
	vips, err := parseLoopbackIPv4VIPs(output)
	assert.NoError(t, err)
	assert.Equal(t, []string{"192.168.100.0"}, vips)
}

func TestResolveKubeVipAddressPrefersManifest(t *testing.T) {
	st := newGracefulShutdownTestState(t, true)
	server := &NodeAgentServer{RootPath: st.tmp}
	runner := &MockRunner{Runners: []runner{
		loopbackVIPRunner("1: lo inet 192.168.101.0/32 scope host lo\n"),
	}}

	vip, err := server.resolveKubeVipAddress(runner)

	assert.NoError(t, err)
	assert.Equal(t, "192.168.100.0", vip)
}

func TestResolveKubeVipAddressUsesBackupManifest(t *testing.T) {
	st := newGracefulShutdownTestState(t, true)
	server := &NodeAgentServer{RootPath: st.tmp}
	backup := server.kubeVipDrainedManifestPath()
	assert.NoError(t, os.MkdirAll(filepath.Dir(backup), 0o755))
	assert.NoError(t, os.Rename(st.manifest, backup))

	vip, err := server.resolveKubeVipAddress(&MockRunner{})

	assert.NoError(t, err)
	assert.Equal(t, "192.168.100.0", vip)
}

func TestResolveKubeVipAddressFallsBackToLoopbackWhenManifestMissing(t *testing.T) {
	st := newGracefulShutdownTestState(t, false)
	server := &NodeAgentServer{RootPath: st.tmp}
	runner := &MockRunner{Runners: []runner{
		loopbackVIPRunner("1: lo inet 192.168.100.0/32 scope host lo\n"),
	}}

	vip, err := server.resolveKubeVipAddress(runner)

	assert.NoError(t, err)
	assert.Equal(t, "192.168.100.0", vip)
}

func TestResolveKubeVipAddressRejectsUnsupportedManifestInterface(t *testing.T) {
	st := newGracefulShutdownTestState(t, false)
	server := &NodeAgentServer{RootPath: st.tmp}
	assert.NoError(t, os.MkdirAll(filepath.Dir(st.manifest), 0o755))
	assert.NoError(t, os.WriteFile(st.manifest, []byte(kubeVipManifestWith("192.168.100.0", "eth0", "32")), 0o644))

	_, err := server.resolveKubeVipAddress(&MockRunner{})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), `unsupported vip_interface "eth0"`)
}

func TestParseRemoteVipPeers(t *testing.T) {
	output := `0 0 192.168.100.0:6443 172.18.0.6:50696
0 0 192.168.100.0:6443 127.0.0.1:35000
0 0 [::ffff:192.168.100.0]:6443 [::ffff:172.18.0.7]:50698
`
	peers, err := parseRemoteVipPeers(output, "192.168.100.0", map[string]struct{}{})
	assert.NoError(t, err)
	assert.Equal(t, []vipSocketPeer{
		{IP: "172.18.0.6", Port: "50696"},
		{IP: "172.18.0.7", Port: "50698"},
	}, peers)
}

func TestListStaticPodContainerIDsParsesStdoutJSONAndIgnoresStderr(t *testing.T) {
	runner := &MockRunner{
		Output: []byte(`{"containers":[{"id":"0123456789abcdef"}]}`),
		Stderr: []byte(`time="2026-05-27T19:28:26Z" level=warning msg="Config \"/etc/crictl.yaml\" does not exist"`),
	}

	ids, err := listStaticPodContainerIDs(runner, "kube-vip")

	assert.NoError(t, err)
	assert.Equal(t, []string{"0123456789abcdef"}, ids)
}

func TestWaitForStaticPodExitAcceptsGoneAtDeadline(t *testing.T) {
	server := &NodeAgentServer{}
	runner := &MockRunner{Runners: []runner{
		runForCommand("crictl", []string{"ps", "--label", "io.kubernetes.container.name=kube-vip", "-o", "json"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
			return []byte(`{"containers":[]}`), nil
		}),
	}}

	err := server.waitForStaticPodExit(context.Background(), runner, "kube-vip", []string{"0123456789abcdef"}, 0)

	assert.NoError(t, err)
}

func TestWaitForStaticPodExitReportsRunningContainerIDs(t *testing.T) {
	server := &NodeAgentServer{}
	runner := &MockRunner{Runners: []runner{
		runForCommand("crictl", []string{"ps", "--label", "io.kubernetes.container.name=kube-apiserver", "-o", "json"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
			return []byte(`{"containers":[{"id":"0123456789abcdef"}]}`), nil
		}),
	}}

	err := server.waitForStaticPodExit(context.Background(), runner, "kube-apiserver", []string{"0123456789abcdef"}, 0)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "container still running after 0s: 0123456789abcdef")
}

func TestWaitForVipSocketQuietWindowScansAtDeadline(t *testing.T) {
	server := &NodeAgentServer{}
	ssListCalls := 0
	runner := &MockRunner{Runners: []runner{
		runForCommand("ss", []string{"-Htan", "state", "established", "src", "192.168.100.0", "sport", "=", ":6443"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
			ssListCalls++
			return []byte("0 0 192.168.100.0:6443 203.0.113.1:50696\n"), nil
		}),
	}}

	err := server.waitForVipSocketQuietWindow(context.Background(), runner, "192.168.100.0", defaultKubeVipApiserverPort, 0)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "new remote VIP:6443 sockets appeared")
	assert.Equal(t, 1, ssListCalls)
}

func TestK8SControlPlaneNodeGracefulShutdown(t *testing.T) {
	tests := []struct {
		name              string
		createManifest    bool
		nodeStatus        *NodeStatus
		call              func(*NodeAgentServer) (*pb.NodeAgentOperationStatus, error)
		runner            func(*gracefulShutdownTestState) *MockRunner
		wantPhase         pb.NodeAgentOperationPhase
		wantErrorContains []string
		wantRPCCode       codes.Code
		assertState       func(*testing.T, *gracefulShutdownTestState)
	}{
		{
			name:           "start succeeds after closing stale vip sockets",
			createManifest: true,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				return &MockRunner{Runners: []runner{
					loopbackVIPRunner("1: lo    inet 192.168.100.0/32 scope host lo\\       valid_lft forever preferred_lft forever\n"),
					staticPodExitedRunner("kube-vip"),
					runForCommand("ip", []string{"addr", "del", "192.168.100.0/32", "dev", "lo"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
						st.order = append(st.order, "remove-vip")
						return nil, nil
					}),
					func(name string, args []string, opts *nkeutils.CommandOptions) (bool, []byte, error) {
						if name != "ss" || !slices.Equal(args, []string{"-Htan", "state", "established", "src", "192.168.100.0", "sport", "=", ":6443"}) {
							return false, nil, nil
						}
						st.ssListCalls++
						if st.ssListCalls == 1 {
							return true, []byte("0 0 192.168.100.0:6443 172.18.0.6:34920\n0 0 192.168.100.0:6443 172.18.0.6:35026\n"), nil
						}
						return true, nil, nil
					},
					func(name string, args []string, opts *nkeutils.CommandOptions) (bool, []byte, error) {
						if name != "ss" || len(args) == 0 || args[0] != "-Ktan" {
							return false, nil, nil
						}
						st.order = append(st.order, "close-socket")
						st.killed = append(st.killed, strings.Join(args, " "))
						return true, nil, nil
					},
				}}
			},
			call:      startGracefulShutdown,
			wantPhase: pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
			assertState: func(t *testing.T, st *gracefulShutdownTestState) {
				assert.Len(t, st.killed, 2)
				assert.Equal(t, []string{"close-socket", "close-socket", "remove-vip"}, st.order)
				assert.NoFileExists(t, st.manifest)
				assert.FileExists(t, filepath.Join(st.tmp, "etc", "kubernetes", "manifests-backup", "kube-vip.yaml"))
				assertPathMode(t, filepath.Join(st.tmp, "etc", "kubernetes", "manifests-backup"), staticPodBackupDirMode)
				assert.FileExists(t, st.apiServerManifest)
				assert.NoFileExists(t, st.apiServerBackupManifest)
			},
		},
		{
			name:           "start stops apiserver after draining vip when requested",
			createManifest: true,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				return &MockRunner{Runners: []runner{
					loopbackVIPRunner("1: lo    inet 192.168.100.0/32 scope host lo\\       valid_lft forever preferred_lft forever\n"),
					staticPodExitedRunner("kube-vip"),
					runForCommand("ip", []string{"addr", "del", "192.168.100.0/32", "dev", "lo"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
						st.order = append(st.order, "remove-vip")
						return nil, nil
					}),
					runForCommand("ss", []string{"-Htan", "state", "established", "src", "192.168.100.0", "sport", "=", ":6443"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
						return nil, nil
					}),
					staticPodRunningThenExitedRunner("kube-apiserver", &st.apiServerListCalls),
				}}
			},
			call:      startGracefulShutdownWithKubeAPIServerStop,
			wantPhase: pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
			assertState: func(t *testing.T, st *gracefulShutdownTestState) {
				assert.NoFileExists(t, st.manifest)
				assert.FileExists(t, filepath.Join(st.tmp, "etc", "kubernetes", "manifests-backup", "kube-vip.yaml"))
				assertPathMode(t, filepath.Join(st.tmp, "etc", "kubernetes", "manifests-backup"), staticPodBackupDirMode)
				assert.NoFileExists(t, st.apiServerManifest)
				assert.FileExists(t, st.apiServerBackupManifest)
				assertAPIServerManifestUnchanged(t, st)
				assert.Equal(t, 2, st.apiServerListCalls)
			},
		},
		{
			name:           "start stops apiserver when kube-vip active manifest already moved",
			createManifest: false,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				writeKubeVipBackupManifest(t, st)
				return &MockRunner{Runners: []runner{
					staticPodExitedRunner("kube-vip"),
					runForCommand("ss", []string{"-Htan", "state", "established", "src", "192.168.100.0", "sport", "=", ":6443"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
						return nil, nil
					}),
					runForCommand("ip", []string{"addr", "del", "192.168.100.0/32", "dev", "lo"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
						return []byte("not found"), errors.New("ip addr missing")
					}),
					loopbackVIPRunner(""),
					staticPodRunningThenExitedRunner("kube-apiserver", &st.apiServerListCalls),
				}}
			},
			call:      startGracefulShutdownWithKubeAPIServerStop,
			wantPhase: pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
			assertState: func(t *testing.T, st *gracefulShutdownTestState) {
				assert.NoFileExists(t, st.apiServerManifest)
				assert.FileExists(t, st.apiServerBackupManifest)
				assertAPIServerManifestUnchanged(t, st)
				assert.Equal(t, 2, st.apiServerListCalls)
			},
		},
		{
			name:           "start succeeds as no-op when kube-vip backup manifest and loopback vip are absent",
			createManifest: false,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				return &MockRunner{Runners: []runner{
					loopbackVIPRunner(""),
				}}
			},
			call:      startGracefulShutdown,
			wantPhase: pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
			assertState: func(t *testing.T, st *gracefulShutdownTestState) {
				assert.FileExists(t, st.apiServerManifest)
				assert.NoFileExists(t, st.apiServerBackupManifest)
			},
		},
		{
			name:           "start stops apiserver when kube-vip is absent",
			createManifest: false,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				return &MockRunner{Runners: []runner{
					loopbackVIPRunner(""),
					staticPodRunningThenExitedRunner("kube-apiserver", &st.apiServerListCalls),
				}}
			},
			call:      startGracefulShutdownWithKubeAPIServerStop,
			wantPhase: pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
			assertState: func(t *testing.T, st *gracefulShutdownTestState) {
				assert.NoFileExists(t, st.apiServerManifest)
				assert.FileExists(t, st.apiServerBackupManifest)
				assertAPIServerManifestUnchanged(t, st)
				assert.Equal(t, 2, st.apiServerListCalls)
			},
		},
		{
			name:           "start removes vip before reporting socket close failure",
			createManifest: true,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				return &MockRunner{Runners: []runner{
					loopbackVIPRunner("1: lo    inet 192.168.100.0/32 scope host lo\\       valid_lft forever preferred_lft forever\n"),
					staticPodExitedRunner("kube-vip"),
					runForCommand("ss", []string{"-Htan", "state", "established", "src", "192.168.100.0", "sport", "=", ":6443"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
						return []byte("0 0 192.168.100.0:6443 172.18.0.6:34920\n"), nil
					}),
					func(name string, args []string, opts *nkeutils.CommandOptions) (bool, []byte, error) {
						if name != "ss" || len(args) == 0 || args[0] != "-Ktan" {
							return false, nil, nil
						}
						return true, []byte("boom"), errors.New("ss failed")
					},
					runForCommand("ip", []string{"addr", "del", "192.168.100.0/32", "dev", "lo"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
						st.removedVIP = true
						return nil, nil
					}),
				}}
			},
			call:              startGracefulShutdown,
			wantPhase:         pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
			wantErrorContains: []string{"close stale remote VIP sockets", "ss failed"},
			assertState: func(t *testing.T, st *gracefulShutdownTestState) {
				assert.True(t, st.removedVIP)
			},
		},
		{
			name:           "start fails when a new vip socket appears during quiet window",
			createManifest: true,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				return &MockRunner{Runners: []runner{
					loopbackVIPRunner("1: lo    inet 192.168.100.0/32 scope host lo\\       valid_lft forever preferred_lft forever\n"),
					staticPodExitedRunner("kube-vip"),
					runForCommand("ip", []string{"addr", "del", "192.168.100.0/32", "dev", "lo"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
						return nil, nil
					}),
					func(name string, args []string, opts *nkeutils.CommandOptions) (bool, []byte, error) {
						if name != "ss" || !slices.Equal(args, []string{"-Htan", "state", "established", "src", "192.168.100.0", "sport", "=", ":6443"}) {
							return false, nil, nil
						}
						st.ssListCalls++
						if st.ssListCalls == 1 {
							return true, nil, nil
						}
						return true, []byte("0 0 192.168.100.0:6443 172.18.0.6:50696\n"), nil
					},
				}}
			},
			call:              startGracefulShutdown,
			wantPhase:         pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
			wantErrorContains: []string{"new remote VIP:6443 sockets appeared", "192.168.100.0:6443 -> 172.18.0.6:50696"},
		},
		{
			name: "start requires a single loopback vip",
			runner: func(*gracefulShutdownTestState) *MockRunner {
				return &MockRunner{Runners: []runner{
					loopbackVIPRunner("1: lo inet 192.168.100.0/32 scope host lo\n1: lo inet 192.168.101.0/32 scope host lo\n"),
				}}
			},
			call:              startGracefulShutdown,
			wantPhase:         pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
			wantErrorContains: []string{"multiple non-loopback /32 addresses"},
		},
		{
			name: "start returns crictl error",
			runner: func(*gracefulShutdownTestState) *MockRunner {
				return &MockRunner{Runners: []runner{
					loopbackVIPRunner("1: lo inet 192.168.100.0/32 scope host lo\n"),
					runForCommand("crictl", []string{"ps", "--label", "io.kubernetes.container.name=kube-vip", "-o", "json"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
						return []byte("boom"), errors.New("crictl failed")
					}),
				}}
			},
			call:              startGracefulShutdown,
			wantPhase:         pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
			wantErrorContains: []string{"crictl failed"},
		},
		{
			name:       "get rejects inactive running operation",
			nodeStatus: inactiveGracefulShutdownStatus(),
			call: func(server *NodeAgentServer) (*pb.NodeAgentOperationStatus, error) {
				resp, err := server.GetK8SControlPlaneNodeGracefulShutdownStatus(context.Background(), &pb.GetK8SControlPlaneNodeGracefulShutdownStatusRequest{
					OperationId: " op-1 ",
				})
				if resp == nil {
					return nil, err
				}
				return resp.GetOperation(), err
			},
			wantRPCCode:       codes.FailedPrecondition,
			wantErrorContains: []string{"no worker is active"},
		},
		{
			name:              "start rejects inactive running operation",
			nodeStatus:        inactiveGracefulShutdownStatus(),
			call:              startGracefulShutdown,
			wantRPCCode:       codes.FailedPrecondition,
			wantErrorContains: []string{"no worker is active"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newGracefulShutdownTestState(t, tc.createManifest)
			server := &NodeAgentServer{
				NodeStatus:                    tc.nodeStatus,
				RootPath:                      st.tmp,
				drainKubeVipPollInterval:      time.Millisecond,
				drainKubeVipSocketQuietWindow: time.Millisecond,
			}
			if tc.runner != nil {
				server.Runner = tc.runner(st)
			}

			op, err := tc.call(server)
			if tc.wantRPCCode != codes.OK {
				assert.Error(t, err)
				assert.Equal(t, tc.wantRPCCode, status.Code(err))
				for _, want := range tc.wantErrorContains {
					assert.Contains(t, err.Error(), want)
				}
				return
			}

			if !assert.NoError(t, err) || !assert.NotNil(t, op) {
				return
			}
			assert.Equal(t, "op-1", op.GetOperationId())

			sts := waitForGracefulShutdownStatus(t, server, "op-1")
			if !assert.NotNil(t, sts) {
				return
			}
			assert.Equal(t, tc.wantPhase, sts.GetPhase())
			for _, want := range tc.wantErrorContains {
				assert.Contains(t, sts.GetErrorMessage(), want)
			}
			if tc.assertState != nil {
				tc.assertState(t, st)
			}
		})
	}
}

func TestKubeVipRestore(t *testing.T) {
	tests := []struct {
		name              string
		createManifest    bool
		call              func(*NodeAgentServer) (*pb.NodeAgentOperationStatus, error)
		runner            func(*gracefulShutdownTestState) *MockRunner
		wantPhase         pb.NodeAgentOperationPhase
		wantErrorContains []string
		wantRPCCode       codes.Code
		assertState       func(*testing.T, *gracefulShutdownTestState)
	}{
		{
			name:           "restore succeeds from backup manifest",
			createManifest: false,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				writeKubeVipBackupManifest(t, st)
				return &MockRunner{Runners: []runner{
					staticPodExitedThenRunningRunner("kube-vip", &st.kubeVipListCalls),
					loopbackVIPRunner("1: lo    inet 192.168.100.0/32 scope host lo\\       valid_lft forever preferred_lft forever\n"),
				}}
			},
			call:      startKubeVipRestore,
			wantPhase: pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
			assertState: func(t *testing.T, st *gracefulShutdownTestState) {
				assert.FileExists(t, st.manifest)
				assert.NoFileExists(t, filepath.Join(st.tmp, "etc", "kubernetes", "manifests-backup", "kube-vip.yaml"))
				assert.FileExists(t, st.apiServerManifest)
				assert.NoFileExists(t, st.apiServerBackupManifest)
				assert.Equal(t, 2, st.kubeVipListCalls)
			},
		},
		{
			name:           "restore waits for new kube-vip container after manifest restore",
			createManifest: false,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				writeKubeVipBackupManifest(t, st)
				return &MockRunner{Runners: []runner{
					staticPodStaleThenNewRunner("kube-vip", &st.kubeVipListCalls),
					loopbackVIPRunner("1: lo    inet 192.168.100.0/32 scope host lo\\       valid_lft forever preferred_lft forever\n"),
				}}
			},
			call:      startKubeVipRestore,
			wantPhase: pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
			assertState: func(t *testing.T, st *gracefulShutdownTestState) {
				assert.Equal(t, 3, st.kubeVipListCalls)
			},
		},
		{
			name:           "restore is idempotent when active manifest is already present",
			createManifest: true,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				return &MockRunner{Runners: []runner{
					staticPodRunningRunner("kube-vip"),
					loopbackVIPRunner("1: lo    inet 192.168.100.0/32 scope host lo\\       valid_lft forever preferred_lft forever\n"),
				}}
			},
			call:      startKubeVipRestore,
			wantPhase: pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
			assertState: func(t *testing.T, st *gracefulShutdownTestState) {
				assert.FileExists(t, st.manifest)
				assert.FileExists(t, st.apiServerManifest)
			},
		},
		{
			name:           "restore continues when active and backup manifests match",
			createManifest: true,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				writeKubeVipBackupManifest(t, st)
				return &MockRunner{Runners: []runner{
					staticPodRunningRunner("kube-vip"),
					loopbackVIPRunner("1: lo    inet 192.168.100.0/32 scope host lo\\       valid_lft forever preferred_lft forever\n"),
				}}
			},
			call:      startKubeVipRestore,
			wantPhase: pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
			assertState: func(t *testing.T, st *gracefulShutdownTestState) {
				assert.FileExists(t, st.manifest)
				assert.FileExists(t, filepath.Join(st.tmp, "etc", "kubernetes", "manifests-backup", "kube-vip.yaml"))
			},
		},
		{
			name:           "restore fails when active and backup manifests differ",
			createManifest: true,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				writeKubeVipBackupManifestWith(t, st, kubeVipManifestWith("192.168.101.0", "lo", "32"))
				return &MockRunner{Runners: []runner{
					staticPodRunningRunner("kube-vip"),
				}}
			},
			call:              startKubeVipRestore,
			wantPhase:         pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
			wantErrorContains: []string{"restore kube-vip manifest", "differs from backup"},
		},
		{
			name:           "restore fails when kube-vip manifest is missing from active and backup dirs",
			createManifest: false,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				return &MockRunner{Runners: []runner{
					staticPodExitedRunner("kube-vip"),
					loopbackVIPRunner("1: lo    inet 192.168.100.0/32 scope host lo\\       valid_lft forever preferred_lft forever\n"),
				}}
			},
			call:              startKubeVipRestore,
			wantPhase:         pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
			wantErrorContains: []string{"restore kube-vip manifest", "static pod manifest is neither active"},
		},
		{
			name:           "restore fails when kube-vip never starts",
			createManifest: false,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				writeKubeVipBackupManifest(t, st)
				return &MockRunner{Runners: []runner{
					staticPodExitedRunner("kube-vip"),
				}}
			},
			call:              startKubeVipRestore,
			wantPhase:         pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
			wantErrorContains: []string{"wait for kube-vip start", "container not running after 1ms"},
			assertState: func(t *testing.T, st *gracefulShutdownTestState) {
				assert.FileExists(t, st.manifest)
			},
		},
		{
			name:           "restore fails when vip does not return to loopback",
			createManifest: false,
			runner: func(st *gracefulShutdownTestState) *MockRunner {
				writeKubeVipBackupManifest(t, st)
				return &MockRunner{Runners: []runner{
					staticPodExitedThenRunningRunner("kube-vip", &st.kubeVipListCalls),
					loopbackVIPRunner(""),
				}}
			},
			call:              startKubeVipRestore,
			wantPhase:         pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
			wantErrorContains: []string{"wait for kube-vip address on lo", "kube-vip address 192.168.100.0 not present on lo after 1ms"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newGracefulShutdownTestState(t, tc.createManifest)
			server := &NodeAgentServer{
				RootPath:                      st.tmp,
				Persister:                     &JSONFilePersister{Path: st.stateFile},
				drainKubeVipPollInterval:      time.Millisecond,
				drainKubeVipSocketQuietWindow: time.Millisecond,
				staticPodStartTimeout:         time.Millisecond,
			}
			if tc.runner != nil {
				server.Runner = tc.runner(st)
			}

			op, err := tc.call(server)
			if tc.wantRPCCode != codes.OK {
				assert.Error(t, err)
				assert.Equal(t, tc.wantRPCCode, status.Code(err))
				for _, want := range tc.wantErrorContains {
					assert.Contains(t, err.Error(), want)
				}
				return
			}

			if !assert.NoError(t, err) || !assert.NotNil(t, op) {
				return
			}
			assert.Equal(t, "op-1", op.GetOperationId())

			sts := waitForKubeVipRestoreStatus(t, server, "op-1")
			if !assert.NotNil(t, sts) {
				return
			}
			assert.Equal(t, tc.wantPhase, sts.GetPhase())
			for _, want := range tc.wantErrorContains {
				assert.Contains(t, sts.GetErrorMessage(), want)
			}
			if tc.assertState != nil {
				tc.assertState(t, st)
			}
		})
	}
}

type gracefulShutdownTestState struct {
	tmp                     string
	manifest                string
	apiServerManifest       string
	apiServerBackupManifest string
	apiServerManifestData   string
	apiServerListCalls      int
	kubeVipListCalls        int
	stateFile               string
	ssListCalls             int
	killed                  []string
	order                   []string
	removedVIP              bool
}

func newGracefulShutdownTestState(t *testing.T, createManifest bool) *gracefulShutdownTestState {
	t.Helper()

	tmp := t.TempDir()
	manifest := filepath.Join(tmp, "etc", "kubernetes", "manifests", "kube-vip.yaml")
	apiServerManifest := filepath.Join(tmp, "etc", "kubernetes", "manifests", "kube-apiserver.yaml")
	apiServerBackupManifest := filepath.Join(tmp, "etc", "kubernetes", "manifests-backup", "kube-apiserver.yaml")
	stateFile := filepath.Join(tmp, "state.json")
	apiServerManifestData := kubeAPIServerManifest()
	if createManifest {
		if !assert.NoError(t, os.MkdirAll(filepath.Dir(manifest), 0o755)) {
			return &gracefulShutdownTestState{tmp: tmp, manifest: manifest, apiServerManifest: apiServerManifest, apiServerBackupManifest: apiServerBackupManifest, stateFile: stateFile}
		}
		assert.NoError(t, os.WriteFile(manifest, []byte(kubeVipManifestWith("192.168.100.0", "lo", "32")), 0o644))
	}
	assert.NoError(t, os.MkdirAll(filepath.Dir(apiServerManifest), 0o755))
	assert.NoError(t, os.WriteFile(apiServerManifest, []byte(apiServerManifestData), 0o644))
	return &gracefulShutdownTestState{
		tmp:                     tmp,
		manifest:                manifest,
		apiServerManifest:       apiServerManifest,
		apiServerBackupManifest: apiServerBackupManifest,
		stateFile:               stateFile,
		apiServerManifestData:   apiServerManifestData,
	}
}

func kubeVipManifestWith(address, iface, cidr string) string {
	return `apiVersion: v1
kind: Pod
metadata:
  name: kube-vip
  namespace: kube-system
spec:
  containers:
  - args:
    - manager
    env:
    - name: vip_interface
      value: ` + iface + `
    - name: vip_cidr
      value: "` + cidr + `"
    - name: address
      value: "` + address + `"
    name: kube-vip
  hostNetwork: true
`
}

func kubeAPIServerManifest() string {
	return `apiVersion: v1
kind: Pod
metadata:
  name: kube-apiserver
  namespace: kube-system
spec:
  containers:
  - command:
    - kube-apiserver
    - --advertise-address=192.0.2.10
    name: kube-apiserver
  hostNetwork: true
`
}

func assertAPIServerManifestUnchanged(t *testing.T, st *gracefulShutdownTestState) {
	t.Helper()

	data, err := os.ReadFile(st.apiServerBackupManifest)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, st.apiServerManifestData, string(data))
}

func assertPathMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, mode, info.Mode().Perm())
}

func writeKubeVipBackupManifest(t *testing.T, st *gracefulShutdownTestState) {
	t.Helper()
	writeKubeVipBackupManifestWith(t, st, kubeVipManifestWith("192.168.100.0", "lo", "32"))
}

func writeKubeVipBackupManifestWith(t *testing.T, st *gracefulShutdownTestState, manifest string) {
	t.Helper()
	backupManifest := filepath.Join(st.tmp, "etc", "kubernetes", "manifests-backup", "kube-vip.yaml")
	assert.NoError(t, os.MkdirAll(filepath.Dir(backupManifest), staticPodBackupDirMode))
	assert.NoError(t, os.WriteFile(backupManifest, []byte(manifest), 0o644))
}

func startGracefulShutdown(server *NodeAgentServer) (*pb.NodeAgentOperationStatus, error) {
	resp, err := server.StartK8SControlPlaneNodeGracefulShutdown(context.Background(), &pb.StartK8SControlPlaneNodeGracefulShutdownRequest{
		OperationId: "op-1",
	})
	if resp == nil {
		return nil, err
	}
	return resp.GetOperation(), err
}

func startGracefulShutdownWithKubeAPIServerStop(server *NodeAgentServer) (*pb.NodeAgentOperationStatus, error) {
	resp, err := server.StartK8SControlPlaneNodeGracefulShutdown(context.Background(), &pb.StartK8SControlPlaneNodeGracefulShutdownRequest{
		OperationId:       "op-1",
		StopKubeApiserver: true,
	})
	if resp == nil {
		return nil, err
	}
	return resp.GetOperation(), err
}

func startKubeVipRestore(server *NodeAgentServer) (*pb.NodeAgentOperationStatus, error) {
	resp, err := server.StartKubeVipRestore(context.Background(), &pb.StartKubeVipRestoreRequest{
		OperationId: "op-1",
	})
	if resp == nil {
		return nil, err
	}
	return resp.GetOperation(), err
}

func inactiveGracefulShutdownStatus() *NodeStatus {
	return &NodeStatus{
		NodeAgentOperations: map[string]NodeAgentOperation{
			"op-1": {
				ID:    "op-1",
				Type:  operationTypeK8sControlPlaneNodeGracefulShutdown,
				Phase: NodeAgentOperationPhaseRunning,
				Step:  "waiting_for_kube_vip_exit",
			},
		},
	}
}

func loopbackVIPRunner(output string) runner {
	return runForCommand("ip", []string{"-o", "-4", "addr", "show", "dev", "lo", "scope", "host"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
		return []byte(output), nil
	})
}

func staticPodRunningRunner(containerName string) runner {
	return runForCommand("crictl", []string{"ps", "--label", "io.kubernetes.container.name=" + containerName, "-o", "json"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
		return []byte(`{"containers":[{"id":"0123456789abcdef"}]}`), nil
	})
}

func staticPodExitedRunner(containerName string) runner {
	return runForCommand("crictl", []string{"ps", "--label", "io.kubernetes.container.name=" + containerName, "-o", "json"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
		return []byte(`{"containers":[]}`), nil
	})
}

func staticPodExitedThenRunningRunner(containerName string, calls *int) runner {
	return runForCommand("crictl", []string{"ps", "--label", "io.kubernetes.container.name=" + containerName, "-o", "json"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
		*calls += 1
		if *calls == 1 {
			return []byte(`{"containers":[]}`), nil
		}
		return []byte(`{"containers":[{"id":"0123456789abcdef"}]}`), nil
	})
}

func staticPodStaleThenNewRunner(containerName string, calls *int) runner {
	return runForCommand("crictl", []string{"ps", "--label", "io.kubernetes.container.name=" + containerName, "-o", "json"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
		*calls += 1
		if *calls < 3 {
			return []byte(`{"containers":[{"id":"old-container"}]}`), nil
		}
		return []byte(`{"containers":[{"id":"new-container"}]}`), nil
	})
}

func staticPodRunningThenExitedRunner(containerName string, calls *int) runner {
	return runForCommand("crictl", []string{"ps", "--label", "io.kubernetes.container.name=" + containerName, "-o", "json"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
		*calls += 1
		if *calls == 1 {
			return []byte(`{"containers":[{"id":"0123456789abcdef"}]}`), nil
		}
		return []byte(`{"containers":[]}`), nil
	})
}

func waitForKubeVipRestoreStatus(t *testing.T, server *NodeAgentServer, operationID string) *pb.NodeAgentOperationStatus {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		status, err := server.Persister.LoadFromStateFile()
		if !assert.NoError(t, err) || !assert.NotNil(t, status) {
			return nil
		}
		op, ok := status.NodeAgentOperations[operationID]
		if !ok {
			if time.Now().After(deadline) {
				t.Fatalf("operation %s was not persisted before timeout", operationID)
			}
			t.Logf("operation %s not yet persisted", operationID)
			time.Sleep(time.Millisecond)
			continue
		}
		opStatus := nodeAgentOperationToProto(op)
		if opStatus.GetPhase() != pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING {
			return opStatus
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s did not finish before timeout; last status: %v", operationID, opStatus)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForGracefulShutdownStatus(t *testing.T, server *NodeAgentServer, operationID string) *pb.NodeAgentOperationStatus {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := server.GetK8SControlPlaneNodeGracefulShutdownStatus(context.Background(), &pb.GetK8SControlPlaneNodeGracefulShutdownStatusRequest{
			OperationId: operationID,
		})
		if !assert.NoError(t, err) || !assert.NotNil(t, resp) || !assert.NotNil(t, resp.GetOperation()) {
			return nil
		}
		op := resp.GetOperation()
		if op.GetPhase() != pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s did not finish before timeout; last status: %v", operationID, op)
		}
		time.Sleep(time.Millisecond)
	}
}
