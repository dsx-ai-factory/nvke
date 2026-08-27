// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"fmt"
	"log/slog"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntimelog "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

type AddToSchemeFunc func(scheme *runtime.Scheme) error

// NewRuntimeClient creates a new Kubernetes client using the controller-runtime client.
// It uses the clientcmd default loading rules to build the Kubernetes config.
// AddToScheme() functions may optionally be provided to add additional Kubernetes API types to the scheme.
func NewRuntimeClient(addToSchemes ...AddToSchemeFunc) (client.Client, error) {
	scheme, err := NewRuntimeScheme(addToSchemes...)
	if err != nil {
		return nil, err
	}

	config, err := NewRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build Kubernetes config: %w", err)
	}

	return client.New(config, client.Options{Scheme: scheme})
}

func NewRuntimeScheme(addToSchemes ...AddToSchemeFunc) (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	err := clientgoscheme.AddToScheme(scheme)
	if err != nil {
		return nil, fmt.Errorf("failed to add client-go scheme: %w", err)
	}
	if err := clusterv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add cluster-api scheme: %w", err)
	}

	for _, addToScheme := range addToSchemes {
		if err := addToScheme(scheme); err != nil {
			return nil, fmt.Errorf("failed to add scheme: %w", err)
		}
	}

	return scheme, nil
}

func NewRuntimeManager(logger *slog.Logger, configMapNamespace string, addToSchemes ...AddToSchemeFunc) (ctrl.Manager, error) {
	config, err := NewRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build Kubernetes config: %w", err)
	}

	scheme, err := NewRuntimeScheme(addToSchemes...)
	if err != nil {
		return nil, err
	}

	if logger != nil {
		ctrlruntimelog.SetLogger(logr.FromSlogHandler(logger.Handler()))
	} else {
		ctrlruntimelog.SetLogger(logr.Discard())
	}

	cacheOptions := cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Secret{}: {
			Namespaces: map[string]cache.Config{
				tenantAccessKubeconfigNamespace: {},
			},
		},
	}}
	if configMapNamespace != "" {
		cacheOptions.ByObject[&corev1.ConfigMap{}] = cache.ByObject{
			Namespaces: map[string]cache.Config{
				configMapNamespace: {},
			},
		}
	}

	return ctrl.NewManager(config, ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cacheOptions,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
}

// NewClientset creates a new Kubernetes clientset using the clientcmd default loading rules.
func NewClientset() (*kubernetes.Clientset, error) {
	config, err := NewRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build Kubernetes config: %w", err)
	}
	return kubernetes.NewForConfig(config)
}

// NewDynamicClient creates a new Kubernetes dynamic client using the clientcmd default loading rules.
func NewDynamicClient() (*dynamic.DynamicClient, error) {
	config, err := NewRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build Kubernetes config: %w", err)
	}
	return dynamic.NewForConfig(config)
}

// NewRESTConfig creates a new Kubernetes client REST config for a no‑flags binary.
// Uses clientcmd default loading rules:
//   - KUBECONFIG
//   - ~/.kube/config
//   - InClusterConfig if running in a Kubernetes cluster.
func NewRESTConfig() (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err == nil {
		return config, nil
	}

	inClusterConfig, inClusterErr := rest.InClusterConfig()
	if inClusterErr == nil {
		return inClusterConfig, nil
	}

	return nil, fmt.Errorf("failed to load kubeconfig: %w; failed to get in-cluster config: %w", err, inClusterErr)
}
