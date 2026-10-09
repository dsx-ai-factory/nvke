// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build gofail

package framework

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"k8s.io/client-go/tools/clientcmd"
)

// setControllerRuntimeLogger installs the process-wide controller-runtime
// logger. It is global, so it covers everything in the test process that logs
// through controller-runtime, not only envtest.
//
// Without it, the first caller prints "log.SetLogger(...) was never called"
// followed by a stack trace. NewRuntimeManager sets one, but that runs in the
// worker process, not here.
//
// Routed to slog rather than discarded so the diagnostics survive.
// `make e2e-gofail` runs `go test -v`, so they are visible on a failure.
var controllerRuntimeLoggerOnce sync.Once

func setControllerRuntimeLogger() {
	controllerRuntimeLoggerOnce.Do(func() {
		ctrllog.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))
	})
}

// kubebuilderAssetsEnv holds the directory of the kube-apiserver and etcd
// binaries envtest runs. `make e2e-gofail` sets it from setup-envtest.
const kubebuilderAssetsEnv = "KUBEBUILDER_ASSETS"

// The legacy names are deprecated aliases, read when the new name is unset or
// empty; nke#1399 removes them.
const (
	clusterCRDsEnv = "NKE_CONTROLLER_GOFAIL_E2E_CLUSTER_CRDS"
	nicoCRDsEnv    = "NKE_CONTROLLER_GOFAIL_E2E_CAPNICO_CRDS"

	legacyClusterCRDsEnv = "CWE_GOFAIL_E2E_CLUSTER_CRDS"
	legacyNicoCRDsEnv    = "CWE_GOFAIL_E2E_CAPNICO_CRDS"
)

// StartAPIServer runs a local kube-apiserver and etcd, and returns a kubeconfig
// path for them. Both stop when the test ends.
//
// The tested workflows never call the Kubernetes API, so a placeholder address
// used to be enough. It is not any more: the worker exits at startup without a
// reachable API, because NewRuntimeManager configures a per-namespace cache for
// corev1.Secret and a Cache.ByObject entry makes controller-runtime resolve the
// REST mapping eagerly rather than lazily.
//
// This is envtest, not a cluster. It runs two binaries and no kubelet, no
// scheduler and no controller manager.
func StartAPIServer(t testing.TB) string {
	t.Helper()

	if os.Getenv(kubebuilderAssetsEnv) == "" {
		t.Skipf("%s is unset; run `make e2e-gofail`, which resolves it with setup-envtest", kubebuilderAssetsEnv)
	}
	setControllerRuntimeLogger()

	env := &envtest.Environment{
		CRDDirectoryPaths:     gofailCRDDirectoryPaths(t),
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	require.NoError(t, err, "starting the envtest API server")
	requireGofailAPIResources(t, cfg)
	// The worker resolves every OS image from the release resources, so the
	// tests install them.
	installReleases(t, cfg)
	t.Cleanup(func() {
		require.NoError(t, env.Stop(), "stopping the envtest API server")
	})

	kubeconfig := clientcmdapi.NewConfig()
	kubeconfig.Clusters[gofailContextName] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
	}
	kubeconfig.AuthInfos[gofailContextName] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
	}
	kubeconfig.Contexts[gofailContextName] = &clientcmdapi.Context{
		Cluster:  gofailContextName,
		AuthInfo: gofailContextName,
	}
	kubeconfig.CurrentContext = gofailContextName

	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, clientcmd.WriteToFile(*kubeconfig, path))
	require.NoError(t, os.Chmod(path, 0o600))

	return path
}

const gofailContextName = "gofail-e2e"

func gofailCRDDirectoryPaths(t testing.TB) []string {
	t.Helper()

	paths := []string{
		getenvWithLegacy(clusterCRDsEnv, legacyClusterCRDsEnv),
		getenvWithLegacy(nicoCRDsEnv, legacyNicoCRDsEnv),
	}
	for _, path := range paths {
		require.NotEmpty(t, path, "CRD directory environment variable must be set")
		info, err := os.Stat(path)
		require.NoError(t, err, "expected CRD directory %s to exist", path)
		require.True(t, info.IsDir(), "expected CRD path %s to be a directory", path)
	}

	return append(paths, releaseCRDPaths(t)...)
}

func requireGofailAPIResources(t testing.TB, cfg *rest.Config) {
	t.Helper()

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	require.NoError(t, err, "creating discovery client")

	for _, resource := range []schema.GroupVersionResource{
		{Group: "cluster.x-k8s.io", Version: "v1beta2", Resource: "machines"},
		{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Resource: "nicomachines"},
	} {
		resourceList, err := discoveryClient.ServerResourcesForGroupVersion(resource.GroupVersion().String())
		require.NoError(t, err, "expected envtest API server to discover %s", resource)
		found := false
		for _, apiResource := range resourceList.APIResources {
			if apiResource.Name == resource.Resource {
				found = true
				break
			}
		}
		require.True(t, found, "expected envtest API server to discover %s", resource)
	}
}
