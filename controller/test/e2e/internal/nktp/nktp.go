// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nktp checks that an installed NKTP pipeline delivers what it
// collects, by querying its backend through Grafana's datasource proxy rather
// than by reading Pod status: a Collector can be Ready and export nothing.
//
// Baseline mirrors nktp/hack/e2e-validate.sh. The other constructors add what
// a particular installation wires up.
package nktp

import (
	"context"
	"errors"
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

	// Signals get arriveBudget to show up. The "nothing bad" checks then get
	// holdBudget to hold, so failures from a rollout age out of their window,
	// and must keep holding for holdWindow.
	arriveBudget   = 8 * time.Minute
	holdBudget     = 3 * time.Minute
	holdWindow     = time.Minute
	pollInterval   = 10 * time.Second
	attemptTimeout = 30 * time.Second
)

// Check is one assertion. A hold check passes trivially on an empty backend,
// so it runs once the others have seen the signals arrive, and then has to
// keep holding.
type Check struct {
	name  string
	hold  bool
	probe func(ctx context.Context, t *target) error
}

type target struct {
	clusterName string
	restConfig  *rest.Config
	clientset   kubernetes.Interface
	dynamic     dynamic.Interface
	grafana     *grafana
	redaction   redactionProbe
}

// Validate runs checks against the installation restConfig reaches, whose
// signals carry clusterName. Every check runs; the error names each one that
// did not hold.
func Validate(ctx context.Context, restConfig *rest.Config, clusterName string, checks []Check, log logr.Logger) error {
	if clusterName == "" || clusterName == fallbackClusterName {
		return fmt.Errorf("nktp: cluster name %q cannot be told from the Collectors' fallback", clusterName)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("building Kubernetes clientset: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("building dynamic client: %w", err)
	}
	backend, err := kube.NewServiceForwarder(restConfig, chartNamespace, backendService, grafanaPort)
	if err != nil {
		return err
	}
	defer backend.Close()

	t := &target{
		clusterName: clusterName,
		restConfig:  restConfig,
		clientset:   clientset,
		dynamic:     dynamicClient,
		grafana:     newGrafana(backend.HTTPClient(), backend.BaseURL()),
	}
	// Started first, so its line has the whole arrive budget to show up.
	if err := t.startRedactionProbe(ctx, log); err != nil {
		return err
	}
	defer t.stopRedactionProbe(ctx, log)

	failures := run(ctx, log, t, checks, stage{name: "arrive", budget: arriveBudget})
	failures = append(failures, run(ctx, log, t, checks, stage{name: "hold", hold: true, budget: holdBudget, window: holdWindow})...)
	return errors.Join(failures...)
}
