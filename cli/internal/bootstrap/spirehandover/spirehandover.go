// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package spirehandover moves the SPIRE server identity of a site's nodes from
// the seed cluster to the site cluster.
//
// The site SPIRE server takes over the seed server's datastore and signing
// keys, so it serves the same trust bundle and the nodes keep their trust
// anchor. The nodes are then repointed at the site server through the seed
// controller.
package spirehandover

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

	api "github.com/dsx-ai-factory/nvke/controller/api"
)

// DefaultNamespace is the namespace the SPIRE server runs in on both clusters.
const DefaultNamespace = "spire-server"

// Step names, in the order Run performs them.
const (
	StepPreflight     = "preflight"
	StepCopyObjects   = "copy-objects"
	StepMoveState     = "move-state"
	StepStartAndPrune = "start-and-prune"
	StepVerify        = "verify"
	StepRestartAgents = "restart-site-agents"
	StepRepointNodes  = "repoint-nodes"
)

const (
	defaultPollInterval = 10 * time.Second
	defaultPollTimeout  = 30 * time.Minute
	// waitTimeout bounds each wait for a pod or StatefulSet to settle.
	waitTimeout = 5 * time.Minute
)

// Controller is the part of the seed NVKE controller API the handover uses.
// *clients.ControllerClient implements it.
type Controller interface {
	GetCluster(ctx context.Context, id openapi_types.UUID) (*api.Cluster, error)
	UpdateClusterSpireServer(ctx context.Context, id openapi_types.UUID, server api.SpireServer) error
	GetClusterSpireServer(ctx context.Context, id openapi_types.UUID) (*api.ClusterSpireServerStatus, error)
}

// CommandRunner runs kubectl. It returns combined stdout and stderr, also on error.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (output string, err error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// Config is the input to Run.
type Config struct {
	// SeedKubeconfig is the kubeconfig of the seed cluster.
	SeedKubeconfig string
	// SiteKubeconfig is the kubeconfig of the site cluster.
	SiteKubeconfig string
	// Controller is the client of the NVKE controller on the seed.
	Controller Controller
	// ClusterID is the seed controller's ID of the site cluster.
	ClusterID openapi_types.UUID
	// SiteSpireAddress and SiteSpirePort are where the nodes reach the site SPIRE server.
	SiteSpireAddress string
	SiteSpirePort    int
	// Namespace of the SPIRE server on both clusters. Empty means DefaultNamespace.
	Namespace string
	// PollInterval is how often each wait checks again. Zero means 10 seconds.
	PollInterval time.Duration
	// PollTimeout bounds the wait for every node to hold an SVID from the site server. Zero means 30 minutes.
	PollTimeout time.Duration
	// Runner runs kubectl. Nil runs it with os/exec.
	Runner CommandRunner
	// Logger receives progress. Nil means slog.Default().
	Logger *slog.Logger
}

// cluster is one side of the handover.
type cluster struct {
	name       string
	kubeconfig string
	kube       kubernetes.Interface
	dyn        dynamic.Interface
}

type handover struct {
	seed, site *cluster
	cfg        Config
	runner     CommandRunner
	log        *slog.Logger
	// seedAgentPrefixes are the SPIFFE ID prefixes of the seed cluster's own
	// in-cluster agents, read from the seed server config at preflight.
	seedAgentPrefixes []string
	// siteConf is the site server config, read at preflight.
	siteConf *serverConfig
}

// Run hands the SPIRE identity of the cluster's nodes from the seed SPIRE
// server to the site SPIRE server, in the steps named by the Step constants.
// Every step reads the clusters before acting, so Run can be repeated after a
// failure at any point. The seed SPIRE server is left scaled to zero.
func Run(ctx context.Context, cfg Config) error {
	if cfg.SeedKubeconfig == "" || cfg.SiteKubeconfig == "" {
		return errors.New("spire handover: seed and site kubeconfigs are required")
	}
	if cfg.Controller == nil {
		return errors.New("spire handover: a seed controller client is required")
	}
	if cfg.SiteSpireAddress == "" || cfg.SiteSpirePort < 1 || cfg.SiteSpirePort > 65535 {
		return fmt.Errorf("spire handover: invalid site SPIRE server %q:%d", cfg.SiteSpireAddress, cfg.SiteSpirePort)
	}
	seed, err := newCluster("seed", cfg.SeedKubeconfig)
	if err != nil {
		return err
	}
	site, err := newCluster("site", cfg.SiteKubeconfig)
	if err != nil {
		return err
	}
	return newHandover(cfg, seed, site).run(ctx)
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

func newHandover(cfg Config, seed, site *cluster) *handover {
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = defaultPollTimeout
	}
	h := &handover{seed: seed, site: site, cfg: cfg, runner: cfg.Runner, log: cfg.Logger}
	if h.runner == nil {
		h.runner = execRunner{}
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	return h
}

func (h *handover) run(ctx context.Context) error {
	h.log.Info("SPIRE handover", "step", StepPreflight)
	authorities, err := h.preflight(ctx)
	if err != nil {
		return fmt.Errorf("spire handover %s: %w", StepPreflight, err)
	}
	marker := fingerprint(authorities)

	h.log.Info("SPIRE handover", "step", StepCopyObjects)
	if err := h.copyObjects(ctx); err != nil {
		return fmt.Errorf("spire handover %s: %w", StepCopyObjects, err)
	}

	moved, err := h.stateMoved(ctx, marker)
	if err != nil {
		return fmt.Errorf("spire handover %s: %w", StepMoveState, err)
	}
	if moved {
		h.log.Info("SPIRE handover: the site server already holds the seed's state", "step", StepMoveState)
	} else {
		h.log.Info("SPIRE handover", "step", StepMoveState)
		if err := h.moveState(ctx); err != nil {
			return fmt.Errorf("spire handover %s: %w", StepMoveState, err)
		}
	}

	h.log.Info("SPIRE handover", "step", StepStartAndPrune)
	if err := h.startAndPrune(ctx); err != nil {
		return fmt.Errorf("spire handover %s: %w", StepStartAndPrune, err)
	}

	h.log.Info("SPIRE handover", "step", StepVerify)
	if err := h.verify(ctx, authorities, marker); err != nil {
		return fmt.Errorf("spire handover %s: %w", StepVerify, err)
	}

	h.log.Info("SPIRE handover", "step", StepRestartAgents)
	if err := h.restartSiteAgents(ctx, authorities, marker); err != nil {
		return fmt.Errorf("spire handover %s: %w", StepRestartAgents, err)
	}

	h.log.Info("SPIRE handover", "step", StepRepointNodes, "address", h.cfg.SiteSpireAddress, "port", h.cfg.SiteSpirePort)
	if err := h.repointNodes(ctx); err != nil {
		return fmt.Errorf("spire handover %s: %w", StepRepointNodes, err)
	}
	h.log.Info("SPIRE handover complete")
	return nil
}

// kubectl runs kubectl against c in the SPIRE namespace.
func (h *handover) kubectl(ctx context.Context, c *cluster, args ...string) (string, error) {
	out, err := h.runner.Run(ctx, "kubectl", append([]string{"--kubeconfig", c.kubeconfig, "-n", h.cfg.Namespace}, args...)...)
	if err != nil {
		return out, fmt.Errorf("%s: kubectl %v: %w\n%s", c.name, args, err, out)
	}
	return out, nil
}
