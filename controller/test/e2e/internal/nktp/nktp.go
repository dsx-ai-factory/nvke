// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nktp checks that an installed NKTP pipeline delivers what it
// collects, by querying its backend through Grafana's datasource proxy rather
// than by reading Pod status: a Collector can be Ready and export nothing.
//
// Baseline is what NKTP delivers on any cluster, mirroring
// nktp/hack/e2e-validate.sh; the other constructors are for what a particular
// installation wires up. Each returns one Check, so a spec's list reads as an
// inventory, and the spec polls Missing and Unwanted over it under Gomega.
package nktp

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

const (
	chartNamespace = "nktp"
	chartRelease   = "nktp"
	backendService = "lgtm"
	grafanaPort    = "grafana"

	// fallbackClusterName is what the Collectors label a signal with when they
	// are not told the cluster's name.
	fallbackClusterName = "unknown"
	attemptTimeout      = 30 * time.Second
)

// Check is one assertion. A presence check waits for a signal to show up. An
// absence check passes trivially on an empty backend, so a spec polls it only
// once the presence checks hold, and then has it keep holding.
type Check struct {
	Name  string
	Probe func(ctx context.Context) error
	// absence marks the checks Unwanted runs; Missing runs the rest.
	absence bool
}

// Validator builds and runs checks against one installation.
type Validator struct {
	cluster    string
	restConfig *rest.Config
	clientset  kubernetes.Interface
	dynamic    dynamic.Interface
	backend    *kube.ServiceForwarder
	grafana    *grafana
	probe      redactionProbe
	log        logr.Logger
}

// New reaches the installation restConfig reaches, whose signals carry cluster,
// and starts the redaction probe, so its line has a spec's whole budget to show
// up. Close stops both.
func New(ctx context.Context, restConfig *rest.Config, cluster string, log logr.Logger) (*Validator, error) {
	if cluster == "" || cluster == fallbackClusterName {
		return nil, fmt.Errorf("nktp: cluster name %q cannot be told from the Collectors' fallback", cluster)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building Kubernetes clientset: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building dynamic client: %w", err)
	}
	backend, err := kube.NewServiceForwarder(restConfig, chartNamespace, backendService, grafanaPort)
	if err != nil {
		return nil, err
	}
	v := &Validator{cluster: cluster, restConfig: restConfig, clientset: clientset, dynamic: dynamicClient, backend: backend, log: log}
	if v.grafana, err = newGrafana(ctx, backend.HTTPClient(), backend.BaseURL()); err != nil {
		backend.Close()
		return nil, err
	}
	if err = v.startRedactionProbe(ctx); err != nil {
		backend.Close()
		return nil, err
	}
	return v, nil
}

// Close stops the redaction probe and the forward to the backend. It runs
// after the checks, often on an expired context.
func (v *Validator) Close(ctx context.Context) {
	v.stopRedactionProbe(ctx)
	v.backend.Close()
}
