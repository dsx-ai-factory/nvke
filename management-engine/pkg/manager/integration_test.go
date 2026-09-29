//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package manager_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	nkev1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nke/management-engine/pkg/controllers/registry"
	"github.com/dsx-ai-factory/nke/management-engine/pkg/manager"
)

func TestManagerIntegration(t *testing.T) {
	capiCRDs := os.Getenv("CAPI_CRD_DIR")
	require.NotEmpty(t, capiCRDs, "set CAPI_CRD_DIR to the pinned CAPI module's config/crd/bases directory")
	cfg := startTestAPI(t, "../../apis/crds", filepath.Join(capiCRDs, "cluster.x-k8s.io_clusters.yaml"))
	configPath := writeExternalConfig(t, cfg)
	external, err := manager.LoadConfig(manager.ConfigOptions{Kubeconfig: configPath, Context: "management"})
	require.NoError(t, err)
	require.Equal(t, cfg.Host, external.Host)

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	var replicas [2]testReplica
	t.Cleanup(func() {
		cancel()
		for i := range replicas {
			replica := &replicas[i]
			if replica.done == nil {
				continue
			}
			select {
			case <-replica.done:
				if replica.err != nil {
					t.Errorf("replica %d: %v", i, replica.err)
				}
			case <-time.After(35 * time.Second):
				t.Errorf("replica %d did not shut down", i)
			}
		}
	})
	opts := manager.DefaultOptions()
	opts.LeaderElectionNamespace = "engine-test"
	var active atomic.Int32
	var overlap atomic.Bool
	leaders := make(chan int, 4)
	type observation struct {
		replica int
		name    string
	}
	observed := make(chan observation, 8)
	for i := range replicas {
		replica := &replicas[i]
		probeListener := reserveAddress(t)
		replica.metricsListener = reserveAddress(t)
		replica.probeAddress, replica.metricsAddress = probeListener.Addr().String(), replica.metricsListener.Addr().String()
		opts.HealthProbeBindAddress, opts.MetricsBindAddress = replica.probeAddress, replica.metricsAddress
		require.NoError(t, probeListener.Close())
		replica.manager, err = manager.New(external, opts)
		require.NoError(t, err)
		require.NoError(t, registry.Register(replica.manager))
		for _, obj := range []client.Object{&nkev1.NKECluster{}, &clusterv1.Cluster{}} {
			informer, err := replica.manager.GetCache().GetInformer(ctx, obj)
			require.NoError(t, err)
			_, err = informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{AddFunc: func(obj any) {
				resource, ok := obj.(client.Object)
				if ok && resource.GetNamespace() == "org-test" {
					select {
					case observed <- observation{replica: i, name: resource.GetName()}:
					case <-ctx.Done():
					}
				}
			}})
			require.NoError(t, err)
		}
		require.NoError(t, replica.manager.Add(&leaderWork{id: i, active: &active, overlap: &overlap, started: leaders}))
	}
	api, err := client.New(cfg, client.Options{Scheme: replicas[0].manager.GetScheme()})
	require.NoError(t, err)
	for _, ns := range []string{"engine-test", "org-test"} {
		require.NoError(t, api.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}
	for i := range replicas {
		replica := &replicas[i]
		runCtx, stop := context.WithCancel(ctx)
		replica.stop = stop
		// Reserve the metrics port until Start binds it; close/rebind still races.
		require.NoError(t, replica.metricsListener.Close())
		replica.done = make(chan struct{})
		go func() {
			replica.err = replica.manager.Start(runCtx)
			close(replica.done)
		}()
	}
	var first int
	select {
	case first = <-leaders:
	case <-replicas[0].done:
		t.Fatalf("replica 0 stopped before election: %v", replicas[0].err)
	case <-replicas[1].done:
		t.Fatalf("replica 1 stopped before election: %v", replicas[1].err)
	case <-ctx.Done():
		t.Fatal("no leader elected")
	}
	require.EqualValues(t, 1, active.Load())
	// Standby readiness must not depend on holding the Lease.
	for _, i := range []int{1 - first, first} {
		replica := &replicas[i]
		waitHTTP(t, ctx, replica, replica.probeAddress, "/readyz", "ok")
		waitHTTP(t, ctx, replica, replica.probeAddress, "/healthz", "ok")
		waitHTTP(t, ctx, replica, replica.metricsAddress, "/metrics", "go_goroutines")
	}
	nke := &nkev1.NKECluster{
		ObjectMeta: metav1.ObjectMeta{Name: "nke-example", Namespace: "org-test"},
		Spec: nkev1.NKEClusterSpec{
			ClusterNetwork: nkev1.ClusterNetwork{
				Pods:     nkev1.NetworkRanges{CIDRBlocks: []nkev1.NetworkCIDR{"10.244.0.0/16"}},
				Services: nkev1.NetworkRanges{CIDRBlocks: []nkev1.NetworkCIDR{"10.96.0.0/12"}}, ServiceDomain: "cluster.local",
			},
			Release:           nkev1.ClusterReleaseSelection{Channel: nkev1.ReleaseChannelReference{Name: "stable"}},
			ControlPlaneClass: "standard",
		},
	}
	require.NoError(t, api.Create(ctx, nke))
	paused := true
	require.NoError(t, api.Create(ctx, &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "capi-example", Namespace: "org-test"},
		Spec:       clusterv1.ClusterSpec{Paused: &paused},
	}))
	want := map[observation]bool{
		{replica: 0, name: "nke-example"}: true, {replica: 0, name: "capi-example"}: true,
		{replica: 1, name: "nke-example"}: true, {replica: 1, name: "capi-example"}: true,
	}
	seen := map[observation]bool{}
	for len(seen) < len(want) {
		select {
		case event := <-observed:
			seen[event] = true
		case <-ctx.Done():
			t.Fatalf("both replicas did not observe NKE/CAPI objects: got %v", seen)
		}
	}
	require.Equal(t, want, seen)
	key := client.ObjectKeyFromObject(nke)
	var cached, direct nkev1.NKECluster
	require.NoError(t, replicas[0].manager.GetClient().Get(ctx, key, &cached))
	require.Empty(t, cached.ManagedFields, "cached objects omit field ownership metadata")
	require.NoError(t, replicas[0].manager.GetAPIReader().Get(ctx, key, &direct))
	require.NotEmpty(t, direct.ManagedFields, "the API reader preserves field ownership metadata")
	replicas[first].stop()
	select {
	case next := <-leaders:
		require.NotEqual(t, first, next)
	case <-ctx.Done():
		t.Fatal("standby did not acquire leadership")
	}
	require.False(t, overlap.Load(), "two replicas ran leader-only work simultaneously")
	require.EqualValues(t, 1, active.Load())
}

func TestManagerStartsWithoutCRDs(t *testing.T) {
	cfg := startTestAPI(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	probeListener := reserveAddress(t)
	opts := manager.DefaultOptions()
	opts.LeaderElectionNamespace = "engine-test"
	opts.MetricsBindAddress = "0"
	opts.HealthProbeBindAddress = probeListener.Addr().String()
	require.NoError(t, probeListener.Close())
	mgr, err := manager.New(cfg, opts)
	require.NoError(t, err)
	require.NoError(t, registry.Register(mgr))
	api, err := client.New(cfg, client.Options{Scheme: mgr.GetScheme()})
	require.NoError(t, err)
	require.NoError(t, api.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: opts.LeaderElectionNamespace}}))
	replica := &testReplica{done: make(chan struct{})}
	go func() {
		replica.err = mgr.Start(ctx)
		close(replica.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-replica.done:
			require.NoError(t, replica.err)
		case <-time.After(35 * time.Second):
			t.Error("manager did not shut down")
		}
	})
	select {
	case <-mgr.Elected():
	case <-replica.done:
		t.Fatalf("manager stopped before election: %v", replica.err)
	case <-ctx.Done():
		t.Fatal("manager did not acquire leadership")
	}
	waitHTTP(t, ctx, replica, opts.HealthProbeBindAddress, "/readyz", "ok")
	waitHTTP(t, ctx, replica, opts.HealthProbeBindAddress, "/healthz", "ok")
}

func startTestAPI(t *testing.T, crdPaths ...string) *rest.Config {
	t.Helper()
	// Ignore ambient settings that could target an existing cluster.
	t.Setenv("USE_EXISTING_CLUSTER", "false")
	env := &envtest.Environment{
		CRDDirectoryPaths:     crdPaths,
		ErrorIfCRDPathMissing: true,
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		assets, err := envtest.SetupEnvtestDefaultBinaryAssetsDirectory()
		require.NoError(t, err)
		env.DownloadBinaryAssets = true
		env.DownloadBinaryAssetsVersion = "1.37.0"
		env.BinaryAssetsDirectory = assets
	}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })
	return cfg
}

type leaderWork struct {
	id      int
	active  *atomic.Int32
	overlap *atomic.Bool
	started chan<- int
}

func (w *leaderWork) NeedLeaderElection() bool { return true }
func (w *leaderWork) Start(ctx context.Context) error {
	if w.active.Add(1) != 1 {
		w.overlap.Store(true)
	}
	defer w.active.Add(-1)
	w.started <- w.id
	<-ctx.Done()
	return nil
}

func writeExternalConfig(t *testing.T, config *rest.Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"management": {Server: config.Host, CertificateAuthorityData: config.CAData}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"test": {ClientCertificateData: config.CertData, ClientKeyData: config.KeyData}},
		Contexts:       map[string]*clientcmdapi.Context{"management": {Cluster: "management", AuthInfo: "test"}},
		CurrentContext: "management",
	}, path))
	return path
}

func reserveAddress(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

type testReplica struct {
	manager         ctrl.Manager
	probeAddress    string
	metricsAddress  string
	metricsListener net.Listener
	stop            context.CancelFunc
	done            chan struct{}
	err             error // Read only after done is closed.
}

func waitHTTP(t *testing.T, ctx context.Context, replica *testReplica, address, path, content string) {
	t.Helper()
	httpClient := &http.Client{Timeout: 2 * time.Second}
	err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		select {
		case <-replica.done:
			return false, fmt.Errorf("manager stopped: %v", replica.err)
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+path, nil)
		if err != nil {
			return false, err
		}
		response, err := httpClient.Do(req)
		if err != nil {
			return false, nil
		}
		body, err := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		return err == nil && closeErr == nil && response.StatusCode == http.StatusOK && strings.Contains(string(body), content), nil
	})
	require.NoError(t, err, "endpoint %s%s did not become ready", address, path)
}
