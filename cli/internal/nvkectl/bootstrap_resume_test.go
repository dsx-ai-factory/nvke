// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/controllerdeploy"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitecluster"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	api "github.com/dsx-ai-factory/nvke/controller/api"
)

// builtCluster is the cluster the controller holds after an earlier run of the
// site spec at path created it.
func builtCluster(t *testing.T, path string) *api.Cluster {
	t.Helper()
	inv, err := sitespec.Load(path)
	require.NoError(t, err)
	req, err := sitecluster.ClusterRequest(inv)
	require.NoError(t, err)
	built := &api.Cluster{
		Name:                          req.Name,
		ControlPlaneKubernetesVersion: req.ControlPlaneKubernetesVersion,
		CniPlugin:                     string(req.CniPlugin),
		EtcdEncrypted:                 req.EtcdEncrypted,
		ControlPlaneVIP:               &req.ControlPlaneVIP,
		Nico:                          req.Nico,
		ControlPlaneConfig:            &api.ControlPlaneConfig{DesiredCount: req.ControlPlaneConfig.DesiredCount},
	}
	if req.CniVersion != nil {
		built.CniVersion = *req.CniVersion
	}
	if req.Network != nil {
		built.Network = api.ClusterNetwork{PodCIDR: req.Network.PodCIDR, ServiceCIDR: req.Network.ServiceCIDR}
	}
	return built
}

// pivotStartedRunner answers the controller's replicas as zero, as after the
// pivot stopped it, and records every helm call.
type pivotStartedRunner struct {
	next controllerdeploy.CommandRunner
	mu   sync.Mutex
	helm []string
}

func (r *pivotStartedRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	line := strings.Join(args, " ")
	if name == "helm" {
		r.mu.Lock()
		r.helm = append(r.helm, line)
		r.mu.Unlock()
	}
	if strings.Contains(line, "jsonpath={.spec.replicas}") {
		return "0", nil
	}
	return r.next.Run(ctx, name, args...)
}

// answerDial accepts every connection, as a VIP with a control plane behind it does.
func answerDial(context.Context, string, string) (net.Conn, error) {
	client, server := net.Pipe()
	_ = server.Close()
	return client, nil
}

// siteKubeconfigAt writes a site cluster kubeconfig whose server is vip.
func siteKubeconfigAt(t *testing.T, vip string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nvke-site.kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(`apiVersion: v1
kind: Config
clusters:
- name: nvke-site
  cluster:
    server: https://`+vip+`:6443
contexts:
- name: nvke-site
  context: {cluster: nvke-site, user: admin}
current-context: nvke-site
users:
- name: admin
  user: {token: example-token}
`), 0o600))
	return path
}

func TestRunBootstrapSiteResume(t *testing.T) {
	t.Run("is refused without nico", func(t *testing.T) {
		setBootstrapEnv(t)
		err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
			siteSpec: writeSiteSpec(t, validSiteSpec), resume: true,
			bootstrapRunner: &recordingRunner{next: leftoverKindRunner{}}, cweRunner: newFakeCWERunner(t),
			lookPath: runtimeOnPath, dialContext: refuseDial,
		})
		require.ErrorContains(t, err, "--resume continues a bootstrap with infra.provider nico only")
	})

	t.Run("with no bootstrap cluster it starts a new bootstrap", func(t *testing.T) {
		setBootstrapEnv(t)
		startFakeCWEAPIWithNodes(t, true, 3, 3)
		siteKubeconfig := filepath.Join(t.TempDir(), "nvke-site.kubeconfig")
		runner := &recordingRunner{next: fakeBootstrapRunner{}}
		err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
			siteSpec: nicoSiteSpecFile(t), resume: true,
			bootstrapRunner: runner, cweRunner: siteCallsFail{next: newFakeCWERunner(t), site: siteKubeconfig},
			kubeconfigOut: siteKubeconfig, encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
			lookPath: runtimeOnPath, dialContext: refuseDial, untaintControlPlane: skipUntaint,
			dumpSpireEntries: func(context.Context, string, string) (int, error) { return 1, nil },
		})
		require.ErrorContains(t, err, "still manages the site cluster until the pivot completes")
		require.True(t, runner.sawArgs("create", "cluster"), "a new bootstrap cluster is created")
	})

	t.Run("continues the cluster an earlier run created", func(t *testing.T) {
		setBootstrapEnv(t)
		fake := startFakeCWEAPIWithNodes(t, true, 3, 3)
		spec := nicoSiteSpecFile(t)
		fake.listed = builtCluster(t, spec)
		siteKubeconfig := filepath.Join(t.TempDir(), "nvke-site.kubeconfig")
		runner := &recordingRunner{next: leftoverKindRunner{}}
		err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
			siteSpec: spec, resume: true,
			bootstrapRunner: runner, cweRunner: siteCallsFail{next: newFakeCWERunner(t), site: siteKubeconfig},
			kubeconfigOut: siteKubeconfig, encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
			// The VIP answers, as the site an earlier run built does.
			lookPath: runtimeOnPath, dialContext: answerDial, untaintControlPlane: skipUntaint,
			dumpSpireEntries: func(context.Context, string, string) (int, error) { return 1, nil },
		})
		require.ErrorContains(t, err, "still manages the site cluster until the pivot completes")
		require.ErrorContains(t, err, "--resume")
		require.Zero(t, fake.posts.Load(), "the cluster is not created again")
		require.False(t, runner.sawArgs("create", "cluster"), "the earlier bootstrap cluster is reused")
		require.False(t, runner.sawArgs("delete", "cluster"), "the bootstrap cluster is kept on failure")
	})

	t.Run("refuses a changed field the cluster cannot change", func(t *testing.T) {
		setBootstrapEnv(t)
		fake := startFakeCWEAPIWithNodes(t, true, 3, 3)
		spec := nicoSiteSpecFile(t)
		fake.listed = builtCluster(t, spec)
		other := "10.0.0.200"
		fake.listed.ControlPlaneVIP = &other
		runner := &recordingRunner{next: leftoverKindRunner{}}
		err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
			siteSpec: spec, resume: true, bootstrapRunner: runner, cweRunner: newFakeCWERunner(t),
			kubeconfigOut: filepath.Join(t.TempDir(), "nvke-site.kubeconfig"), encryptionKeyOut: filepath.Join(t.TempDir(), "key.json"),
			lookPath: runtimeOnPath, untaintControlPlane: skipUntaint,
		})
		require.ErrorContains(t, err, "cluster.controlPlaneVIP is 10.0.0.100 in the site spec but 10.0.0.200 in the cluster")
		require.Zero(t, fake.posts.Load())
		require.False(t, runner.sawArgs("delete", "cluster"), "the bootstrap cluster is kept on failure")
	})

	t.Run("goes straight to the pivot once it has started", func(t *testing.T) {
		setBootstrapEnv(t)
		cwe := &pivotStartedRunner{next: newFakeCWERunner(t)}
		runner := &recordingRunner{next: leftoverKindRunner{}}
		err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
			siteSpec: nicoSiteSpecFile(t), resume: true, bootstrapRunner: runner, cweRunner: cwe,
			kubeconfigOut: siteKubeconfigAt(t, "10.0.0.100"), lookPath: runtimeOnPath,
		})
		// The fake bootstrap cluster has no kubeconfig for the pivot to load.
		require.ErrorContains(t, err, "the pivot to the site cluster did not complete")
		require.Empty(t, cwe.helm, "nothing is installed again once the pivot has started")
		require.False(t, runner.sawArgs("delete", "cluster"), "the bootstrap cluster is kept on failure")
	})

	t.Run("refuses a new VIP once the pivot has started", func(t *testing.T) {
		setBootstrapEnv(t)
		err := runBootstrapSite(context.Background(), io.Discard, &bootstrapSiteOptions{
			siteSpec: nicoSiteSpecFile(t), resume: true, bootstrapRunner: &recordingRunner{next: leftoverKindRunner{}},
			cweRunner: &pivotStartedRunner{next: newFakeCWERunner(t)}, kubeconfigOut: siteKubeconfigAt(t, "10.0.0.200"),
			lookPath: runtimeOnPath,
		})
		require.ErrorContains(t, err, "built with control-plane VIP 10.0.0.200, but the site spec says 10.0.0.100")
	})
}
