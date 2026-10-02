// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"errors"
	"fmt"
	"os"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ConfigOptions selects the management cluster used by every manager client.
type ConfigOptions struct {
	Kubeconfig string
	Context    string
}

// LoadConfig checks explicit kubeconfig/context, KUBECONFIG, in-cluster credentials,
// then the default kubeconfig. Files must be trusted and remain unchanged during loading;
// invalid selections fail without falling back to another cluster.
func LoadConfig(opts ConfigOptions) (*rest.Config, error) {
	cfg, err := loadRESTConfig(opts)
	if err != nil {
		return nil, err
	}
	// Match controller-runtime: rely on server-side API Priority and Fairness.
	if cfg.QPS == 0 {
		cfg.QPS = -1
	}
	return cfg, nil
}

func loadRESTConfig(opts ConfigOptions) (*rest.Config, error) {
	if opts.Kubeconfig == "" && opts.Context == "" && os.Getenv(clientcmd.RecommendedConfigPathEnvVar) == "" {
		cfg, err := rest.InClusterConfig()
		if err == nil {
			return cfg, nil
		}
		if !errors.Is(err, rest.ErrNotInCluster) {
			return nil, fmt.Errorf("load in-cluster credentials: %w", err)
		}
	}

	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	loader.ExplicitPath = opts.Kubeconfig
	if opts.Kubeconfig == "" && os.Getenv(clientcmd.RecommendedConfigPathEnvVar) != "" {
		// client-go skips missing files, which can change the selected cluster.
		for _, path := range loader.Precedence {
			if path == "" {
				continue
			}
			if _, err := os.Stat(path); err != nil {
				return nil, fmt.Errorf("load management kubeconfig %q: %w", path, err)
			}
		}
	}
	raw, err := loader.Load()
	if err != nil {
		return nil, fmt.Errorf("load management kubeconfig: %w", err)
	}
	// Direct loading avoids deferred clientcmd's implicit in-cluster fallback.
	cfg, err := clientcmd.NewNonInteractiveClientConfig(*raw, opts.Context, &clientcmd.ConfigOverrides{}, loader).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("select management kubeconfig context: %w", err)
	}
	return cfg, nil
}
