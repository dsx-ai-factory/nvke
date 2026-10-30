// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package pivot moves the management plane of a site cluster from the seed
// cluster that built it onto the site cluster itself.
package pivot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/spirehandover"
)

// The pivot's steps, in the order Run takes them.
const (
	StepCheckCAPI          = "check-capi"
	StepStopSiteController = "stop-site-controller"
	StepSpireHandover      = "spire-handover"
	StepQuiesceSeed        = "quiesce-seed"
	StepMoveCAPI           = "move-capi"
	StepMoveDatabase       = "move-database"
	StepCopySecrets        = "copy-secrets"
	StepActivateSite       = "activate-site"
	StepVerify             = "verify"
)

const (
	defaultPollInterval   = 10 * time.Second
	defaultQuiesceTimeout = 10 * time.Minute
	defaultVerifyTimeout  = 10 * time.Minute
	moveReadyTimeout      = 15 * time.Minute
)

// Config is the input to Run.
type Config struct {
	// SeedKubeconfig is the kubeconfig of the seed cluster.
	SeedKubeconfig string
	// SiteKubeconfig is the kubeconfig of the site cluster.
	SiteKubeconfig string
	// Controller is the client of the NVKE controller on the seed.
	Controller spirehandover.Controller
	// ClusterID and ClusterName identify the site cluster in the seed controller and in Cluster API.
	// A zero ClusterID is read from the seed's database by name.
	ClusterID   openapi_types.UUID
	ClusterName string
	// SiteSpireAddress is where the nodes reach the site SPIRE server: the control-plane VIP.
	SiteSpireAddress string
	// DisableMTLS skips the SPIRE handover and the check of each node's SVID, for
	// a site stood up without SPIFFE.
	DisableMTLS bool
	// PollInterval is how often each wait checks again. Zero means 10 seconds.
	PollInterval time.Duration
	// Runner runs kubectl and clusterctl. Nil runs them with os/exec.
	Runner spirehandover.CommandRunner
	// Logger receives progress. Nil means slog.Default().
	Logger *slog.Logger
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// cluster is one side of the pivot.
type cluster struct {
	name       string
	kubeconfig string
	kube       kubernetes.Interface
	dyn        dynamic.Interface
}

type pivot struct {
	seed, site *cluster
	cfg        Config
	runner     spirehandover.CommandRunner
	log        *slog.Logger
	handover   func(context.Context, spirehandover.Config) error
}

// Run moves the management plane of the site cluster from the seed to the
// site, in the steps named by the Step constants. The site must already run
// the NVKE stack. Run does not delete the seed; it leaves the seed's NVKE
// controller and SPIRE server scaled to zero.
//
// Run continues a pivot an earlier Run left unfinished: it skips each step
// whose result the clusters already show, and once the site's controller
// holds the cluster it only finishes activating and verifying the site.
func Run(ctx context.Context, cfg Config) error {
	if cfg.SeedKubeconfig == "" || cfg.SiteKubeconfig == "" {
		return errors.New("pivot: seed and site kubeconfigs are required")
	}
	if cfg.Controller == nil || cfg.ClusterName == "" || cfg.SiteSpireAddress == "" {
		return errors.New("pivot: a seed controller client, the cluster name and the site SPIRE address are required")
	}
	seed, err := newCluster("seed", cfg.SeedKubeconfig)
	if err != nil {
		return err
	}
	site, err := newCluster("site", cfg.SiteKubeconfig)
	if err != nil {
		return err
	}
	return newPivot(cfg, seed, site).run(ctx)
}

func newCluster(name, kubeconfig string) (*cluster, error) {
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("loading %s kubeconfig %s: %w", name, kubeconfig, err)
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating %s client: %w", name, err)
	}
	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating %s dynamic client: %w", name, err)
	}
	return &cluster{name: name, kubeconfig: kubeconfig, kube: kube, dyn: dyn}, nil
}

func newPivot(cfg Config, seed, site *cluster) *pivot {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	p := &pivot{seed: seed, site: site, cfg: cfg, runner: cfg.Runner, log: cfg.Logger, handover: spirehandover.Run}
	if p.runner == nil {
		p.runner = execRunner{}
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	return p
}

func (p *pivot) run(ctx context.Context) error {
	if p.cfg.ClusterID == (openapi_types.UUID{}) {
		id, err := p.clusterID(ctx)
		if err != nil {
			return fmt.Errorf("pivot: %w", err)
		}
		p.cfg.ClusterID = id
	}
	// done, when set, reports that an earlier run already finished the step.
	steps := []struct {
		name string
		done func(context.Context) (bool, error)
		do   func(context.Context) error
	}{
		{StepCheckCAPI, p.capiMoved, p.checkCAPI},
		{StepStopSiteController, nil, p.stopSiteController},
		// The handover precedes quiesce-seed, and its repoint needs the seed controller.
		{StepSpireHandover, p.seedQuiesced, p.spireHandover},
		{StepQuiesceSeed, nil, p.quiesceSeed},
		{StepMoveCAPI, nil, p.moveCAPI},
		{StepMoveDatabase, p.databaseMoved, p.moveDatabase},
		{StepCopySecrets, nil, p.copySecrets},
		{StepActivateSite, nil, p.activateSite},
		{StepVerify, nil, p.verify},
	}
	active, err := p.siteActive(ctx)
	if err != nil {
		return fmt.Errorf("pivot: %w", err)
	}
	if active {
		// The site controller works on its own database now; the earlier steps
		// would stop it or overwrite that database.
		p.log.Info("the site controller already manages the cluster; finishing the pivot")
		steps = steps[len(steps)-2:]
	}
	for _, s := range steps {
		if s.done != nil {
			done, err := s.done(ctx)
			if err != nil {
				return fmt.Errorf("pivot %s: %w", s.name, err)
			}
			if done {
				p.log.Info("pivot step already done", "step", s.name)
				continue
			}
		}
		p.log.Info("pivot", "step", s.name)
		if err := s.do(ctx); err != nil {
			return fmt.Errorf("pivot %s: %w", s.name, err)
		}
	}
	p.log.Info("pivot complete: the site manages itself")
	return nil
}

// kubectl runs kubectl against c.
func (p *pivot) kubectl(ctx context.Context, c *cluster, args ...string) (string, error) {
	out, err := p.runner.Run(ctx, "kubectl", append([]string{"--kubeconfig", c.kubeconfig}, args...)...)
	if err != nil {
		return out, fmt.Errorf("%s: kubectl %v: %w\n%s", c.name, args, err, out)
	}
	return out, nil
}

// poll calls done every PollInterval until it reports true, returns an error, or timeout passes.
func (p *pivot) poll(ctx context.Context, what string, timeout time.Duration, done func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()
	for {
		ok, err := done(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
		case <-ticker.C:
		}
	}
}
