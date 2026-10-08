// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
)

const certificateExpirationOutput = `
[check-expiration] Reading configuration from the cluster...
CERTIFICATE                EXPIRES                  RESIDUAL TIME   CERTIFICATE AUTHORITY   EXTERNALLY MANAGED
admin.conf                 Aug 29, 2026 00:00 UTC   1d              ca                      no
apiserver                  Aug 30, 2026 00:00 UTC   2d              ca                      no
controller-manager.conf    Aug 28, 2026 12:00 UTC   12h             ca                      no
front-proxy-client         Aug 20, 2026 00:00 UTC   <invalid>       front-proxy-ca          no
scheduler.conf             Aug 28, 2027 00:00 UTC   365d            ca                      no

CERTIFICATE AUTHORITY      EXPIRES                  RESIDUAL TIME   EXTERNALLY MANAGED
ca                         Aug 28, 2035 00:00 UTC   8y              no
`

type certificateRenewalTestContainer struct {
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels"`
}

// certificateRenewalTestRunner models local CRI state from manifest presence,
// with independent readiness failures after renewal. This lets tests distinguish
// files being restored from components actually becoming healthy. A component
// whose manifest disappears and returns gets a new container ID, and every CRI
// query records how many control-plane manifests are out of place at once.
type certificateRenewalTestRunner struct {
	mu                        sync.Mutex
	rootPath                  string
	checkOutput               string
	etcdMembers               int
	etcdContainers            []certificateRenewalTestContainer
	memberListStderr          string
	memberListErr             error
	etcdHealthErr             error
	renewCalls                int
	failWhenStopped           bool
	failWhenStoppedComponent  string // restricts failWhenStopped to one component; empty means any
	conflictWhenStopped       bool
	stopFailureIssued         bool
	stopped                   bool
	postRenewalHealthProbe    string
	postRenewalHealthFailures int
	postRenewalHealthCalls    int

	readyzFailsAfterStops    int // when positive, readyz fails once this many components have been observed stopped
	componentGeneration      map[string]int
	componentAbsent          map[string]bool
	stopOrder                []string
	maxComponentsStopped     int
	manifestLayoutViolations []string
}

func (r *certificateRenewalTestRunner) Run(name string, args []string, _ *nvkeutils.CommandOptions) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.healthProbeError(args); err != nil {
		return []byte("still recovering"), err
	}
	switch name {
	case "kubeadm":
		switch strings.Join(args, " ") {
		case "certs check-expiration":
			return []byte(r.checkOutput), nil
		case "certs renew all":
			r.renewCalls++
			return []byte("renewed"), nil
		default:
			return nil, fmt.Errorf("unexpected kubeadm args: %s", strings.Join(args, " "))
		}
	case "crictl":
		command := strings.Join(args, " ")
		if !strings.HasPrefix(command, "exec ") || !strings.Contains(command, " etcdctl ") {
			return nil, fmt.Errorf("unexpected etcd container: %s", command)
		}
		if strings.HasSuffix(command, "endpoint health --cluster") {
			if r.etcdHealthErr != nil {
				return []byte("remote endpoint unhealthy"), r.etcdHealthErr
			}
			return []byte("healthy"), nil
		}
		return nil, fmt.Errorf("unexpected crictl args: %s", command)
	case "kubectl":
		return []byte("ok"), nil
	default:
		return nil, fmt.Errorf("unexpected command %s", name)
	}
}

func (r *certificateRenewalTestRunner) RunSplitOutput(
	name string,
	args []string,
	_ *nvkeutils.CommandOptions,
) (nvkeutils.CommandOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name != "crictl" || len(args) < 3 {
		return nvkeutils.CommandOutput{}, fmt.Errorf("unexpected command: %s %v", name, args)
	}
	command := strings.Join(args, " ")
	if strings.HasSuffix(command, "member list --write-out=json") {
		if !strings.HasPrefix(command, "exec ") || !strings.Contains(command, " etcdctl ") {
			return nvkeutils.CommandOutput{}, fmt.Errorf("unexpected etcd container: %s", command)
		}
		if err := r.healthProbeError(args); err != nil {
			return nvkeutils.CommandOutput{Stdout: []byte("still recovering")}, err
		}
		members := make([]string, r.etcdMembers)
		for index := range r.etcdMembers {
			members[index] = `{"isLearner":false}`
		}
		return nvkeutils.CommandOutput{
			Stdout: []byte(fmt.Sprintf(`{"members":[%s]}`, strings.Join(members, ","))),
			Stderr: []byte(r.memberListStderr),
		}, r.memberListErr
	}
	component := strings.TrimPrefix(args[2], "io.kubernetes.container.name=")
	manifest := map[string]string{
		"kube-apiserver":          "kube-apiserver.yaml",
		"kube-controller-manager": "kube-controller-manager.yaml",
		"kube-scheduler":          "kube-scheduler.yaml",
		"etcd":                    "etcd.yaml",
	}[component]
	if manifest == "" {
		return nvkeutils.CommandOutput{}, fmt.Errorf("unexpected component %s", component)
	}
	r.observeManifestLayout()
	_, statErr := os.Stat(filepath.Join(r.rootPath, "etc", "kubernetes", "manifests", manifest))
	running := statErr == nil
	if r.componentGeneration == nil {
		r.componentGeneration = map[string]int{}
		r.componentAbsent = map[string]bool{}
	}
	switch {
	case !running && !r.componentAbsent[component]:
		r.componentAbsent[component] = true
		r.stopOrder = append(r.stopOrder, component)
	case running && r.componentAbsent[component]:
		r.componentAbsent[component] = false
		r.componentGeneration[component]++
	}
	if !running {
		r.stopped = true
	}
	if !running && r.failWhenStopped && !r.stopFailureIssued &&
		(r.failWhenStoppedComponent == "" || r.failWhenStoppedComponent == component) {
		r.stopFailureIssued = true
		if r.conflictWhenStopped {
			path := filepath.Join(r.rootPath, "etc", "kubernetes", "manifests", manifest)
			if err := os.WriteFile(path, []byte("conflicting manifest"), 0o600); err != nil {
				return nvkeutils.CommandOutput{}, err
			}
		}
		return nvkeutils.CommandOutput{Stderr: []byte("runtime unavailable")}, errors.New("runtime unavailable")
	}
	containers := []certificateRenewalTestContainer{}
	if running {
		hostname, err := os.Hostname()
		if err != nil {
			return nvkeutils.CommandOutput{}, err
		}
		containers = append(containers, certificateRenewalTestContainer{
			ID: fmt.Sprintf("%s-%d", component, r.componentGeneration[component]),
			Labels: map[string]string{
				"io.kubernetes.container.name": component,
				"io.kubernetes.pod.namespace":  "kube-system",
				"io.kubernetes.pod.name":       component + "-" + hostname,
			},
		})
		if component == "etcd" && r.etcdContainers != nil {
			containers = r.etcdContainers
		}
	}
	ps := struct {
		Containers []certificateRenewalTestContainer `json:"containers"`
	}{Containers: []certificateRenewalTestContainer{}}
	for _, container := range containers {
		matches := true
		for index := 1; index+1 < len(args); index++ {
			if args[index] == "--label" {
				label, value, _ := strings.Cut(args[index+1], "=")
				if container.Labels[label] != value {
					matches = false
					break
				}
			}
		}
		if matches {
			ps.Containers = append(ps.Containers, container)
		}
	}
	output, err := json.Marshal(ps)
	return nvkeutils.CommandOutput{Stdout: output}, err
}

// observeManifestLayout records, at every CRI query, how many control-plane
// manifests are out of the active directory and whether anything other than a
// single control-plane manifest was staged or kube-vip was disturbed.
func (r *certificateRenewalTestRunner) observeManifestLayout() {
	manifestDir := filepath.Join(r.rootPath, "etc", "kubernetes", "manifests")
	absent := 0
	for _, name := range certificateRenewalRequiredManifests {
		if _, err := os.Stat(filepath.Join(manifestDir, name)); err != nil {
			absent++
		}
	}
	r.maxComponentsStopped = max(r.maxComponentsStopped, absent)
	if _, err := os.Stat(filepath.Join(manifestDir, "kube-vip.yaml")); err != nil {
		r.manifestLayoutViolations = append(r.manifestLayoutViolations, "kube-vip.yaml is not active")
	}
	staged, err := os.ReadDir(filepath.Join(r.rootPath, strings.TrimPrefix(certificateRenewalStagingDir, "/")))
	if err != nil {
		return
	}
	for _, entry := range staged {
		if !slices.Contains(certificateRenewalRequiredManifests, entry.Name()) {
			r.manifestLayoutViolations = append(r.manifestLayoutViolations, "unrelated manifest staged: "+entry.Name())
		}
	}
	if len(staged) > 1 {
		r.manifestLayoutViolations = append(r.manifestLayoutViolations, fmt.Sprintf("%d manifests staged at once", len(staged)))
	}
}

func (r *certificateRenewalTestRunner) healthProbeError(args []string) error {
	if r.readyzFailsAfterStops > 0 && len(r.stopOrder) >= r.readyzFailsAfterStops &&
		strings.HasSuffix(strings.Join(args, " "), "get --raw /readyz?verbose") {
		return errors.New("readyz not ready")
	}
	if r.renewCalls > 0 && r.postRenewalHealthProbe != "" && strings.HasSuffix(strings.Join(args, " "), r.postRenewalHealthProbe) {
		r.postRenewalHealthCalls++
		if r.postRenewalHealthFailures < 0 || r.postRenewalHealthCalls <= r.postRenewalHealthFailures {
			return errors.New("not ready")
		}
	}
	return nil
}

func (r *certificateRenewalTestRunner) RunDetached(string, []string, *nvkeutils.CommandOptions) error {
	return errors.New("unexpected detached command")
}

// TestExpiringKubernetesCertificates protects the leaf-only, strict-threshold
// boundary and rejects missing certificates instead of treating them as healthy.
func TestExpiringKubernetesCertificates(t *testing.T) {
	expiring, err := expiringKubernetesCertificates(certificateExpirationOutput, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"admin.conf", "controller-manager.conf", "front-proxy-client"}, expiring)

	_, err = expiringKubernetesCertificates(`
CERTIFICATE EXPIRES RESIDUAL TIME CERTIFICATE AUTHORITY EXTERNALLY MANAGED
!MISSING! admin.conf`, 2)
	require.ErrorContains(t, err, "missing certificate")
}

// TestExpiringKubernetesCertificatesParsesHumanDurations covers the compound
// units kubeadm prints (apimachinery HumanDuration) against a 30-day threshold.
func TestExpiringKubernetesCertificatesParsesHumanDurations(t *testing.T) {
	for _, tt := range []struct {
		residual string
		expiring bool
		wantErr  string
	}{
		{residual: "45s", expiring: true},
		{residual: "2m30s", expiring: true},
		{residual: "5h10m", expiring: true},
		{residual: "3d4h", expiring: true},
		{residual: "29d", expiring: true},
		{residual: "29d23h", expiring: true},
		{residual: "30d", expiring: false},
		{residual: "400d", expiring: false},
		{residual: "2y1d", expiring: false},
		{residual: "8y", expiring: false},
		{residual: "<invalid>", expiring: true},
		{residual: "2y1", wantErr: "invalid duration"},
		{residual: "d", wantErr: "invalid duration"},
		{residual: "1w", wantErr: "unknown unit"},
		{residual: "9223372036854775807y", wantErr: "out of range"},
	} {
		t.Run(tt.residual, func(t *testing.T) {
			output := "CERTIFICATE EXPIRES RESIDUAL TIME CERTIFICATE AUTHORITY EXTERNALLY MANAGED\n" +
				"admin.conf Aug 28, 2027 00:00 UTC " + tt.residual + " ca no\n"

			expiring, err := expiringKubernetesCertificates(output, 30)

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.expiring {
				assert.Equal(t, []string{"admin.conf"}, expiring)
			} else {
				assert.Empty(t, expiring)
			}
		})
	}
}

func TestKubernetesCertificateRenewalUnchanged(t *testing.T) {
	rootPath := t.TempDir()
	writeCertificateRenewalManifests(t, rootPath)
	runner := &certificateRenewalTestRunner{
		rootPath: rootPath,
		checkOutput: `CERTIFICATE EXPIRES RESIDUAL TIME CERTIFICATE AUTHORITY EXTERNALLY MANAGED
admin.conf Aug 28, 2027 00:00 UTC 365d ca no
CERTIFICATE AUTHORITY EXPIRES RESIDUAL TIME EXTERNALLY MANAGED
ca Aug 28, 2035 00:00 UTC 8y no`,
	}
	server := newCertificateRenewalTestServer(rootPath, runner)

	operation := runCertificateRenewalOperation(t, server, "unchanged", 1)

	assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED, operation.GetOutcome())
	assert.Zero(t, runner.renewCalls)
	assertCertificateRenewalManifestsRestored(t, rootPath)
}

func TestKubernetesCertificateRenewalHappyPath(t *testing.T) {
	rootPath := t.TempDir()
	writeCertificateRenewalManifests(t, rootPath)
	runner := &certificateRenewalTestRunner{rootPath: rootPath, checkOutput: certificateExpirationOutput}
	server := newCertificateRenewalTestServer(rootPath, runner)

	operation := runCertificateRenewalOperation(t, server, "renewed", 7)

	assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, operation.GetOperation().GetPhase())
	assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, operation.GetOutcome())
	assert.Equal(t, 1, runner.renewCalls)
	assert.Empty(t, server.kubernetesCertificateRenewalState())
	assertCertificateRenewalManifestsRestored(t, rootPath)
}

// TestKubernetesCertificateRenewalRestartsComponentsSequentially mirrors the
// runbook: etcd, kube-apiserver, kube-controller-manager, kube-scheduler, one at
// a time, each with a replacement container, while kube-vip is never touched.
func TestKubernetesCertificateRenewalRestartsComponentsSequentially(t *testing.T) {
	rootPath := t.TempDir()
	writeCertificateRenewalManifests(t, rootPath)
	runner := &certificateRenewalTestRunner{rootPath: rootPath, checkOutput: certificateExpirationOutput, etcdMembers: 3}
	server := newCertificateRenewalTestServer(rootPath, runner)

	operation := runCertificateRenewalV2Operation(t, server, "sequential", 7, true, 3)

	assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, operation.GetOutcome())
	assert.Equal(t, []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler"}, runner.stopOrder)
	assert.Equal(t, 1, runner.maxComponentsStopped, "control-plane components must never be stopped together")
	assert.Empty(t, runner.manifestLayoutViolations)
	for _, component := range certificateRenewalControlPlaneComponents {
		assert.Equal(t, 1, runner.componentGeneration[component], "%s must be replaced exactly once", component)
	}
	assertCertificateRenewalManifestsRestored(t, rootPath)
}

func TestKubernetesCertificateRenewalForceBypassesExpirationCheck(t *testing.T) {
	rootPath := t.TempDir()
	writeCertificateRenewalManifests(t, rootPath)
	runner := &certificateRenewalTestRunner{rootPath: rootPath, checkOutput: "malformed if parsed", etcdMembers: 1}
	server := newCertificateRenewalTestServer(rootPath, runner)

	operation := runCertificateRenewalV2Operation(t, server, "forced", 7, true, 1)

	assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, operation.GetOperation().GetPhase())
	assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, operation.GetOutcome())
	assert.Equal(t, 1, runner.renewCalls)
	assertCertificateRenewalManifestsRestored(t, rootPath)
}

// TestKubernetesCertificateRenewalWaitsForPostRestartHealth distinguishes transient
// recovery from persistent failure: restored manifests must not clear restart
// intent until all V2 health probes pass.
func TestKubernetesCertificateRenewalWaitsForPostRestartHealth(t *testing.T) {
	for _, probe := range []string{"member list --write-out=json", "endpoint health --cluster", "get --raw /readyz?verbose"} {
		for _, persistent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/persistent=%t", probe, persistent), func(t *testing.T) {
				rootPath := t.TempDir()
				writeCertificateRenewalManifests(t, rootPath)
				runner := &certificateRenewalTestRunner{
					rootPath: rootPath, etcdMembers: 3,
					postRenewalHealthProbe: probe, postRenewalHealthFailures: 2,
				}
				if persistent {
					runner.postRenewalHealthFailures = -1
				}
				server := newCertificateRenewalTestServer(rootPath, runner)
				started := time.Now()

				operation := runCertificateRenewalV2Operation(t, server, "post-restart-health", 7, true, 3)

				if persistent {
					assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, operation.GetOperation().GetPhase())
					assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED, operation.GetOutcome())
					assert.Contains(t, operation.GetOperation().GetErrorMessage(), "context deadline exceeded")
					assert.Contains(t, operation.GetOperation().GetErrorMessage(), "still recovering")
					assert.Equal(t, certificateRenewalStateRestartPending, server.kubernetesCertificateRenewalState())
					assert.GreaterOrEqual(t, time.Since(started), server.certificateRenewalStartTimeout)
					assert.Greater(t, runner.postRenewalHealthCalls, 1)
				} else {
					assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, operation.GetOperation().GetPhase())
					assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, operation.GetOutcome())
					assert.Empty(t, server.kubernetesCertificateRenewalState())
					assert.Equal(t, 2+len(certificateRenewalControlPlaneComponents), runner.postRenewalHealthCalls,
						"each component restart must pass its own health gate")
				}
				assert.Equal(t, 1, runner.renewCalls)
				assertCertificateRenewalManifestsRestored(t, rootPath)
			})
		}
	}
}

// TestKubernetesCertificateRenewalWaitsForRecoveredHealth covers restart_pending
// recovery while the local API is still warming up, without repeating renewal.
func TestKubernetesCertificateRenewalWaitsForRecoveredHealth(t *testing.T) {
	rootPath := t.TempDir()
	writeCertificateRenewalManifests(t, rootPath)
	runner := &certificateRenewalTestRunner{
		rootPath: rootPath, etcdMembers: 3, renewCalls: 1,
		postRenewalHealthProbe: "get --raw /readyz?verbose", postRenewalHealthFailures: 2,
	}
	server := newCertificateRenewalTestServer(rootPath, runner)
	server.NodeStatus.K8sCertRenewalState = certificateRenewalStateRestartPending

	operation := runCertificateRenewalV2Operation(t, server, "recovered-health", 7, true, 3)

	assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, operation.GetOperation().GetPhase())
	assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, operation.GetOutcome())
	assert.Equal(t, 1, runner.renewCalls, "recovery must not renew certificates again")
	assert.Equal(t, 3+len(certificateRenewalControlPlaneComponents), runner.postRenewalHealthCalls,
		"recovered state and every restarted component must become healthy")
	assert.Empty(t, server.kubernetesCertificateRenewalState())
	assertCertificateRenewalManifestsRestored(t, rootPath)
}

func TestCertificateRenewalHealthWaitHonorsCancellation(t *testing.T) {
	server := newCertificateRenewalTestServer(t.TempDir(), &certificateRenewalTestRunner{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := server.waitForCertificateRenewalControlPlaneHealth(ctx, server.Runner, 3)

	require.ErrorIs(t, err, context.Canceled)
}

// TestKubernetesCertificateRenewalV2RequiresExpectedEtcdMembers prevents an omitted
// snapshot count from silently opting a V2 caller into V1's weaker health checks.
func TestKubernetesCertificateRenewalV2RequiresExpectedEtcdMembers(t *testing.T) {
	server := newCertificateRenewalTestServer(t.TempDir(), &certificateRenewalTestRunner{})

	_, err := server.StartKubernetesCertificateRenewalV2(
		context.Background(),
		&pb.StartKubernetesCertificateRenewalV2Request{
			OperationId: "missing-members", ThresholdDays: 7,
		},
	)

	require.ErrorContains(t, err, "expected_etcd_members must be positive")
}

func TestKubernetesCertificateRenewalStrictHealthHappyPath(t *testing.T) {
	rootPath := t.TempDir()
	writeCertificateRenewalManifests(t, rootPath)
	runner := &certificateRenewalTestRunner{
		rootPath: rootPath,
		checkOutput: `CERTIFICATE EXPIRES RESIDUAL TIME CERTIFICATE AUTHORITY EXTERNALLY MANAGED
admin.conf Aug 28, 2027 00:00 UTC 365d ca no
CERTIFICATE AUTHORITY EXPIRES RESIDUAL TIME EXTERNALLY MANAGED
ca Aug 28, 2035 00:00 UTC 8y no`,
		etcdMembers: 3,
	}
	server := newCertificateRenewalTestServer(rootPath, runner)

	operation := runCertificateRenewalV2Operation(t, server, "strict-health", 7, false, 3)

	assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, operation.GetOperation().GetPhase())
	assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED, operation.GetOutcome())
	assert.Zero(t, runner.renewCalls)
}

// TestKubernetesCertificateRenewalV2SingleVoterRenews accepts one expected etcd
// member and renews; the agent leaves voter-loss tolerance to the controller.
func TestKubernetesCertificateRenewalV2SingleVoterRenews(t *testing.T) {
	rootPath := t.TempDir()
	writeCertificateRenewalManifests(t, rootPath)
	runner := &certificateRenewalTestRunner{rootPath: rootPath, checkOutput: certificateExpirationOutput, etcdMembers: 1}
	server := newCertificateRenewalTestServer(rootPath, runner)

	operation := runCertificateRenewalV2Operation(t, server, "single-voter", 7, false, 1)

	assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, operation.GetOperation().GetPhase())
	assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, operation.GetOutcome())
	assert.Equal(t, 1, runner.renewCalls)
	assertCertificateRenewalManifestsRestored(t, rootPath)
}

// TestKubernetesCertificateRenewalV2EtcdContainerIdentity rejects unrelated or
// ambiguous etcd containers without assuming the Kubernetes node name equals the
// OS hostname. Identity failure must happen before renewal or manifest staging.
func TestKubernetesCertificateRenewalV2EtcdContainerIdentity(t *testing.T) {
	localPodName := "etcd-" + hostnameForTest(t)
	container := func(id, namespace, podName string) certificateRenewalTestContainer {
		return certificateRenewalTestContainer{
			ID: id,
			Labels: map[string]string{
				"io.kubernetes.container.name": "etcd",
				"io.kubernetes.pod.namespace":  namespace,
				"io.kubernetes.pod.name":       podName,
			},
		}
	}
	local := container("new-container", "kube-system", localPodName)
	workload := container("workload-container", "default", localPodName)
	tests := []struct {
		name       string
		containers []certificateRenewalTestContainer
		wantError  string
	}{
		{
			name:       "local pod name differs from OS hostname",
			containers: []certificateRenewalTestContainer{container("new-container", "kube-system", localPodName+"-override")},
		},
		{
			name:       "workload precedes local static pod",
			containers: []certificateRenewalTestContainer{workload, local},
		},
		{
			name:       "workload follows local static pod",
			containers: []certificateRenewalTestContainer{local, workload},
		},
		{
			name:       "unrelated kube-system pod",
			containers: []certificateRenewalTestContainer{container("unrelated", "kube-system", "workload"), local},
		},
		{
			name:       "pod name prefix must be anchored",
			containers: []certificateRenewalTestContainer{container("unrelated", "kube-system", "workload-"+localPodName), local},
		},
		{
			name:       "distinct kubeadm etcd pod names are ambiguous",
			containers: []certificateRenewalTestContainer{container("other-node", "kube-system", localPodName+"-other"), local},
			wantError:  "expected exactly one running local etcd container, found 2",
		},
		{
			name:       "missing pod identity",
			containers: []certificateRenewalTestContainer{container("unlabeled", "", ""), local},
		},
		{
			name:       "missing pod identity cannot replace local static pod",
			containers: []certificateRenewalTestContainer{container("unlabeled", "", "")},
			wantError:  "expected exactly one running local etcd container, found 0",
		},
		{
			name:       "unrelated workload is not a local static pod",
			containers: []certificateRenewalTestContainer{workload},
			wantError:  "expected exactly one running local etcd container, found 0",
		},
		{
			name:       "duplicate local static pod containers are rejected",
			containers: []certificateRenewalTestContainer{local, container("duplicate", "kube-system", localPodName)},
			wantError:  "expected exactly one running local etcd container, found 2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rootPath := t.TempDir()
			writeCertificateRenewalManifests(t, rootPath)
			runner := &certificateRenewalTestRunner{
				rootPath: rootPath,
				checkOutput: `CERTIFICATE EXPIRES RESIDUAL TIME CERTIFICATE AUTHORITY EXTERNALLY MANAGED
admin.conf Aug 28, 2027 00:00 UTC 365d ca no`,
				etcdMembers:    3,
				etcdContainers: tt.containers,
			}
			server := newCertificateRenewalTestServer(rootPath, runner)

			operation := runCertificateRenewalV2Operation(t, server, "etcd-identity", 7, false, 3)

			if tt.wantError != "" {
				assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, operation.GetOperation().GetPhase())
				assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_BEFORE_RESTART, operation.GetOutcome())
				assert.Contains(t, operation.GetOperation().GetErrorMessage(), tt.wantError)
			} else {
				assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, operation.GetOperation().GetPhase())
				assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED, operation.GetOutcome())
			}
			assert.Zero(t, runner.renewCalls)
			assert.False(t, runner.stopped)
			assert.Empty(t, server.kubernetesCertificateRenewalState())
			assertCertificateRenewalManifestsRestored(t, rootPath)
		})
	}
}

// TestCheckCertificateRenewalControlPlaneHealthEtcdMemberOutput keeps crictl
// warnings out of membership JSON while retaining both streams on command failure.
func TestCheckCertificateRenewalControlPlaneHealthEtcdMemberOutput(t *testing.T) {
	for _, tt := range []struct {
		name   string
		stderr string
		err    error
	}{
		{
			name:   "valid membership stdout with stderr warning succeeds",
			stderr: `time="2026-05-27T19:28:26Z" level=warning msg="Config \"/etc/crictl.yaml\" does not exist"`,
		},
		{
			name:   "command failure preserves stdout and stderr diagnostics",
			stderr: "etcd request timed out",
			err:    errors.New("exit status 1"),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rootPath := t.TempDir()
			writeCertificateRenewalManifests(t, rootPath)
			runner := &certificateRenewalTestRunner{
				rootPath: rootPath, etcdMembers: 3, memberListStderr: tt.stderr, memberListErr: tt.err,
			}
			server := newCertificateRenewalTestServer(rootPath, runner)

			err := server.checkCertificateRenewalControlPlaneHealth(runner, 3)

			if tt.err == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tt.err)
				assert.Contains(t, err.Error(), "etcd member list")
				assert.Contains(t, err.Error(), `stdout={"members":[{"isLearner":false},{"isLearner":false},{"isLearner":false}]}`)
				assert.Contains(t, err.Error(), "stderr="+tt.stderr)
			}
		})
	}
}

func TestKubernetesCertificateRenewalRejectsEtcdMembershipMismatch(t *testing.T) {
	rootPath := t.TempDir()
	writeCertificateRenewalManifests(t, rootPath)
	runner := &certificateRenewalTestRunner{
		rootPath: rootPath, checkOutput: certificateExpirationOutput, etcdMembers: 2,
	}
	server := newCertificateRenewalTestServer(rootPath, runner)

	operation := runCertificateRenewalV2Operation(t, server, "membership-mismatch", 7, false, 3)

	assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, operation.GetOperation().GetPhase())
	assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_BEFORE_RESTART, operation.GetOutcome())
	assert.Contains(t, operation.GetOperation().GetErrorMessage(), "expected=3 members=2 voters=2")
	assert.Zero(t, runner.renewCalls)
	assertCertificateRenewalManifestsRestored(t, rootPath)
}

// TestKubernetesCertificateRenewalRejectsUnhealthyRemoteEtcdMember ensures local
// running containers cannot hide an unhealthy peer before another voter restarts.
func TestKubernetesCertificateRenewalRejectsUnhealthyRemoteEtcdMember(t *testing.T) {
	rootPath := t.TempDir()
	writeCertificateRenewalManifests(t, rootPath)
	runner := &certificateRenewalTestRunner{
		rootPath:      rootPath,
		checkOutput:   certificateExpirationOutput,
		etcdMembers:   3,
		etcdHealthErr: errors.New("unhealthy cluster endpoint"),
	}
	server := newCertificateRenewalTestServer(rootPath, runner)

	operation := runCertificateRenewalV2Operation(t, server, "unhealthy-remote-etcd", 7, false, 3)

	assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, operation.GetOperation().GetPhase())
	assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_BEFORE_RESTART, operation.GetOutcome())
	assert.Contains(t, operation.GetOperation().GetErrorMessage(), "remote endpoint unhealthy")
	assert.Zero(t, runner.renewCalls)
	assertCertificateRenewalManifestsRestored(t, rootPath)
}

func TestKubernetesCertificateRenewalFailureRestoresManifests(t *testing.T) {
	tests := []struct {
		name        string
		component   string
		conflict    bool
		wantOutcome pb.KubernetesCertificateRenewalOperation_Outcome
		wantStaged  bool
		wantStopped []string
	}{
		{
			name:        "runtime failure restores the active manifest",
			wantOutcome: pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED,
			wantStopped: []string{"etcd"},
		},
		{
			name:        "mid-sequence runtime failure restores the active manifest",
			component:   "kube-controller-manager",
			wantOutcome: pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED,
			wantStopped: []string{"etcd", "kube-apiserver", "kube-controller-manager"},
		},
		{
			name:        "conflicting active manifest reports incomplete restore",
			conflict:    true,
			wantOutcome: pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_NOT_RESTORED,
			wantStaged:  true,
			wantStopped: []string{"etcd"},
		},
		{
			name:        "mid-sequence conflicting active manifest reports incomplete restore",
			component:   "kube-scheduler",
			conflict:    true,
			wantOutcome: pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_NOT_RESTORED,
			wantStaged:  true,
			wantStopped: []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rootPath := t.TempDir()
			writeCertificateRenewalManifests(t, rootPath)
			runner := &certificateRenewalTestRunner{
				rootPath:            rootPath,
				checkOutput:         certificateExpirationOutput,
				failWhenStopped:     true,
				conflictWhenStopped: tt.conflict,

				failWhenStoppedComponent: tt.component,
			}
			server := newCertificateRenewalTestServer(rootPath, runner)

			operation := runCertificateRenewalOperation(t, server, "failed", 7)

			assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, operation.GetOperation().GetPhase())
			assert.Equal(t, tt.wantOutcome, operation.GetOutcome())
			_, statErr := os.Stat(filepath.Join(rootPath, strings.TrimPrefix(certificateRenewalStagingDir, "/")))
			assert.Equal(t, tt.wantStaged, statErr == nil)
			assert.Equal(t, tt.wantStopped, runner.stopOrder, "components after the failure must not be stopped")
			assert.Equal(t, certificateRenewalStateRestartPending, server.kubernetesCertificateRenewalState())
			if !tt.conflict {
				assertCertificateRenewalManifestsRestored(t, rootPath)
			}
		})
	}
}

// TestKubernetesCertificateRenewalHealthGateBetweenComponents proves a component
// that restarts but leaves the control plane unhealthy stops the sequence before
// the next pod is touched, with its manifest already restored.
func TestKubernetesCertificateRenewalHealthGateBetweenComponents(t *testing.T) {
	rootPath := t.TempDir()
	writeCertificateRenewalManifests(t, rootPath)
	runner := &certificateRenewalTestRunner{
		rootPath: rootPath, checkOutput: certificateExpirationOutput, etcdMembers: 3, readyzFailsAfterStops: 2,
	}
	server := newCertificateRenewalTestServer(rootPath, runner)

	operation := runCertificateRenewalV2Operation(t, server, "health-gate", 7, true, 3)

	assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, operation.GetOperation().GetPhase())
	assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED, operation.GetOutcome())
	assert.Contains(t, operation.GetOperation().GetErrorMessage(), "post-restart check of kube-apiserver")
	assert.Equal(t, []string{"etcd", "kube-apiserver"}, runner.stopOrder)
	assert.Equal(t, 1, runner.maxComponentsStopped)
	assert.Equal(t, certificateRenewalStateRestartPending, server.kubernetesCertificateRenewalState())
	assertCertificateRenewalManifestsRestored(t, rootPath)
}

// TestKubernetesCertificateRenewalFailsBeforeRestartWithoutRunningComponent keeps
// an absent pod from being treated as restartable: nothing is staged.
func TestKubernetesCertificateRenewalFailsBeforeRestartWithoutRunningComponent(t *testing.T) {
	rootPath := t.TempDir()
	writeCertificateRenewalManifests(t, rootPath)
	runner := &certificateRenewalTestRunner{rootPath: rootPath, checkOutput: certificateExpirationOutput}
	server := newCertificateRenewalTestServer(rootPath, runner)
	require.NoError(t, os.Remove(filepath.Join(rootPath, "etc", "kubernetes", "manifests", "etcd.yaml")))

	operation := runCertificateRenewalOperation(t, server, "missing-etcd", 7)

	assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, operation.GetOperation().GetPhase())
	assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_BEFORE_RESTART, operation.GetOutcome())
	assert.Zero(t, runner.renewCalls)
	assert.Empty(t, server.kubernetesCertificateRenewalState())
	_, err := os.Stat(filepath.Join(rootPath, strings.TrimPrefix(certificateRenewalStagingDir, "/")))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestKubernetesCertificateRenewalResumesInterruptedSequence covers an agent exit
// mid-sequence: one manifest is stranded in staging and restart_pending is
// persisted. The retry restores it, skips renewal, and restarts every component.
func TestKubernetesCertificateRenewalResumesInterruptedSequence(t *testing.T) {
	rootPath := t.TempDir()
	writeCertificateRenewalManifests(t, rootPath)
	stagingDir := filepath.Join(rootPath, strings.TrimPrefix(certificateRenewalStagingDir, "/"))
	require.NoError(t, os.MkdirAll(stagingDir, 0o700))
	require.NoError(t, os.Rename(
		filepath.Join(rootPath, "etc", "kubernetes", "manifests", "kube-apiserver.yaml"),
		filepath.Join(stagingDir, "kube-apiserver.yaml"),
	))
	runner := &certificateRenewalTestRunner{rootPath: rootPath, etcdMembers: 3, renewCalls: 1}
	server := newCertificateRenewalTestServer(rootPath, runner)
	server.NodeStatus.K8sCertRenewalState = certificateRenewalStateRestartPending
	server.NodeStatus.NodeAgentOperations = map[string]NodeAgentOperation{
		"resume": {ID: "resume", Type: operationTypeKubernetesCertificateRenewal, Phase: NodeAgentOperationPhaseRunning},
	}

	operation := runCertificateRenewalV2Operation(t, server, "resume", 7, true, 3)

	assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, operation.GetOperation().GetPhase())
	assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, operation.GetOutcome())
	assert.Equal(t, 1, runner.renewCalls, "resume must not renew certificates again")
	assert.Equal(t, []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler"}, runner.stopOrder)
	assert.Equal(t, 1, runner.maxComponentsStopped)
	assert.Empty(t, server.kubernetesCertificateRenewalState())
	assertCertificateRenewalManifestsRestored(t, rootPath)
}

func TestRecoverInterruptedCertificateRenewalRestoresBeforeRestart(t *testing.T) {
	rootPath := t.TempDir()
	stagingDir := filepath.Join(rootPath, strings.TrimPrefix(certificateRenewalStagingDir, "/"))
	require.NoError(t, os.MkdirAll(stagingDir, 0o700))
	for _, name := range append(certificateRenewalRequiredManifests, "kube-vip.yaml") {
		require.NoError(t, os.WriteFile(filepath.Join(stagingDir, name), []byte(name), 0o600))
	}
	server := &NodeAgentServer{
		RootPath:  rootPath,
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Role: ControlPlaneRole,
			NodeAgentOperations: map[string]NodeAgentOperation{
				"stale": {
					ID: "stale", Type: operationTypeKubernetesCertificateRenewal,
					Phase: NodeAgentOperationPhaseRunning,
				},
			},
		},
	}

	require.NoError(t, server.recoverInterruptedCertificateRenewal("stale"))

	assertCertificateRenewalManifestsRestored(t, rootPath)
	_, found := server.NodeStatus.NodeAgentOperations["stale"]
	assert.False(t, found)
}

func newCertificateRenewalTestServer(rootPath string, runner nvkeutils.CommandRunner) *NodeAgentServer {
	return &NodeAgentServer{
		RootPath:                       rootPath,
		Runner:                         runner,
		Persister:                      &MockPersister{},
		NodeStatus:                     &NodeStatus{Role: ControlPlaneRole},
		certificateRenewalPollInterval: time.Millisecond,
		certificateRenewalStopTimeout:  100 * time.Millisecond,
		certificateRenewalStartTimeout: 100 * time.Millisecond,
	}
}

func runCertificateRenewalOperation(
	t *testing.T,
	server *NodeAgentServer,
	operationID string,
	thresholdDays int32,
) *pb.KubernetesCertificateRenewalOperation {
	t.Helper()
	response, err := server.StartKubernetesCertificateRenewal(
		context.Background(),
		&pb.StartKubernetesCertificateRenewalRequest{
			OperationId: operationID, ThresholdDays: thresholdDays,
		},
	)
	require.NoError(t, err)
	require.NotNil(t, response.GetOperation())
	return waitForCertificateRenewalOperation(t, server, operationID)
}

func runCertificateRenewalV2Operation(
	t *testing.T,
	server *NodeAgentServer,
	operationID string,
	thresholdDays int32,
	force bool,
	expectedEtcdMembers int32,
) *pb.KubernetesCertificateRenewalOperation {
	t.Helper()
	response, err := server.StartKubernetesCertificateRenewalV2(
		context.Background(),
		&pb.StartKubernetesCertificateRenewalV2Request{
			OperationId: operationID, ThresholdDays: thresholdDays,
			Force: force, ExpectedEtcdMembers: expectedEtcdMembers,
		},
	)
	require.NoError(t, err)
	require.NotNil(t, response.GetOperation())
	return waitForCertificateRenewalOperation(t, server, operationID)
}

func waitForCertificateRenewalOperation(
	t *testing.T,
	server *NodeAgentServer,
	operationID string,
) *pb.KubernetesCertificateRenewalOperation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		statusResponse, statusErr := server.GetKubernetesCertificateRenewalStatus(
			context.Background(),
			&pb.GetKubernetesCertificateRenewalStatusRequest{OperationId: operationID},
		)
		require.NoError(t, statusErr)
		operation := statusResponse.GetOperation()
		require.NotNil(t, operation)
		if operation.GetOperation().GetPhase() != pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING {
			return operation
		}
		if time.Now().After(deadline) {
			t.Fatalf("certificate renewal operation did not finish: %v", operation)
		}
		time.Sleep(time.Millisecond)
	}
}

func writeCertificateRenewalManifests(t *testing.T, rootPath string) {
	t.Helper()
	dir := filepath.Join(rootPath, strings.TrimPrefix(defaultStaticPodManifestDir, "/"))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for _, name := range append(certificateRenewalRequiredManifests, "kube-vip.yaml") {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600))
	}
}

func assertCertificateRenewalManifestsRestored(t *testing.T, rootPath string) {
	t.Helper()
	dir := filepath.Join(rootPath, strings.TrimPrefix(defaultStaticPodManifestDir, "/"))
	for _, name := range append(certificateRenewalRequiredManifests, "kube-vip.yaml") {
		contents, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Equal(t, name, string(contents))
	}
	_, err := os.Stat(filepath.Join(rootPath, strings.TrimPrefix(certificateRenewalStagingDir, "/")))
	assert.ErrorIs(t, err, os.ErrNotExist)
}
