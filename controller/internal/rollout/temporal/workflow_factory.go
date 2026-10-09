// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	temporalactivity "go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
	adaptercontroller "github.com/dsx-ai-factory/nke/controller/internal/rollout/adapters/controller"
)

// WorkflowConfig is the input to RegisterRolloutWorkflow. The factory
// builds a workflow body that loads the cluster, calls Execute, and
// returns SimpleResponse. Consumers needing custom request/response
// shape should register manually.
type WorkflowConfig struct {
	// Impl: Compile auto-detects optional capabilities. Impl.Name()
	// is the Temporal workflow type and the operation registry key.
	Impl rollout.Operation

	// Spec resolves the RolloutSpec from the loaded cluster. MUST be
	// pure (no clock, env, globals) — Temporal replay determinism
	// depends on it. Use Static(s) for cluster-independent specs.
	Spec func(rollout.Cluster) rollout.RolloutSpec

	// SuppressDefaultPreflights skips the SpecMutator passed to
	// RegisterRolloutWorkflow. Set true only for workflows that exist
	// to inspect degraded clusters (e.g. rollout_dry_run) or that
	// have already composed the floor into Spec.
	SuppressDefaultPreflights bool

	// ActivityOptions, when non-nil, selects the activity-option context the
	// executor runs every activity under — most importantly ApplyToNode, which
	// drives the agent RPC. The default (nil) uses withRolloutOptions (a
	// 2m ScheduleToClose budget); that is too short for operations whose
	// per-node RPC blocks on apiserver readiness, so such operations MUST supply
	// WithRolloutNodeAgentApplyOptions (an 8m/20m budget with heartbeat). It is
	// captured at registration time and applied inside the deterministic replay
	// region, so it MUST be pure (no clock/env/globals) like Spec.
	ActivityOptions func(workflow.Context) workflow.Context
}

// SpecMutator transforms a RolloutSpec before Execute sees it. MUST
// be pure — called inside the workflow's deterministic replay region.
// Adapters supply one at worker startup to layer host-specific defaults
// onto every spec (e.g. the controller prepends the safety floor). It is captured
// into the workflow's specFn at registration time (see
// RegisterRolloutWorkflow), so there is no mutable process-wide state to
// reassign mid-flight.
type SpecMutator func(cluster rollout.Cluster, spec rollout.RolloutSpec) rollout.RolloutSpec

// Static adapts a constant RolloutSpec to WorkflowConfig.Spec for
// cluster-independent specs.
func Static(s rollout.RolloutSpec) func(rollout.Cluster) rollout.RolloutSpec {
	return func(_ rollout.Cluster) rollout.RolloutSpec { return s }
}

// purityCheckCluster is a stable sentinel passed to Spec twice at
// registration to detect impurity. Never reaches an activity.
type purityCheckCluster struct{}

func (purityCheckCluster) ID() uuid.UUID                     { return uuid.Nil }
func (purityCheckCluster) Name() string                      { return "purity-check-sentinel" }
func (purityCheckCluster) KubernetesVersion() string         { return "" }
func (purityCheckCluster) ControlPlaneVIP() string           { return "" }
func (purityCheckCluster) ControlPlaneNodes() []rollout.Node { return nil }
func (purityCheckCluster) Nodes() []rollout.Node             { return nil }

// assertSpecDeterministic calls spec twice with the sentinel cluster
// and panics if outputs diverge. Catches impure effective spec functions at
// registration rather than as a Temporal replay failure weeks later.
func assertSpecDeterministic(opName string, spec func(rollout.Cluster) rollout.RolloutSpec) {
	a := callSpecForPurityCheck(opName, spec, "first")
	b := callSpecForPurityCheck(opName, spec, "second")
	if !reflect.DeepEqual(a, b) {
		panic(fmt.Sprintf(
			"temporal rollout: effective spec for %q is non-deterministic — two successive calls produced different RolloutSpec values. Common causes: time.Now(), os.Getenv, math/rand, or a captured-by-closure counter inside the Spec function or mutator. Temporal replay determinism depends on both being pure.",
			opName,
		))
	}
}

func callSpecForPurityCheck(
	opName string,
	spec func(rollout.Cluster) rollout.RolloutSpec,
	which string,
) rollout.RolloutSpec {
	var out rollout.RolloutSpec
	func() {
		defer func() {
			if r := recover(); r != nil {
				panic(fmt.Sprintf(
					"temporal rollout: effective spec for %q panicked on the registration-time purity check (%s call). The check passes a minimal sentinel cluster (uuid.Nil, no nodes); make Spec and its mutator tolerate it or document why the panic is intentional. recovered: %v",
					opName, which, r,
				))
			}
		}()
		out = spec(purityCheckCluster{})
	}()
	return out
}

type SimpleRequest struct {
	ClusterID string
}

type SimpleResponse struct {
	ClusterName string
	Result      rollout.Result
}

// WorkflowRegisterer is the workflow-registration subset of
// worker.Worker. Mirrors the activity-side Registerer (activities.go) so
// a TestWorkflowEnvironment — which is not a full worker.Worker — can be
// passed directly, exactly as it already is for activity registration.
type WorkflowRegisterer interface {
	RegisterWorkflowWithOptions(w any, options workflow.RegisterOptions)
}

// RegisterRolloutWorkflow registers cfg's operation into reg and its
// workflow body onto r, and returns the workflow function (so a test
// environment can execute it directly). Call once per operation at
// worker startup, after reg is constructed.
//
// The mutator is captured into specFn here, at registration time — there
// is no process-wide mutator global and no invocation-time lookup, so
// replay determinism is structural. Pass nil for no floor;
// SuppressDefaultPreflights also bypasses it. Panics on nil Impl/Spec,
// empty Name, non-deterministic Spec, or duplicate operation name.
func RegisterRolloutWorkflow(
	r WorkflowRegisterer,
	reg *rollout.Registry,
	mutator SpecMutator,
	cfg WorkflowConfig,
) any {
	if cfg.Impl == nil {
		panic("temporal rollout: WorkflowConfig.Impl is nil")
	}
	if cfg.Spec == nil {
		panic("temporal rollout: WorkflowConfig.Spec is nil (use rollouttemporal.Static for constant specs)")
	}
	op := rollout.Compile(cfg.Impl)
	name := op.Name()
	if name == "" {
		panic("temporal rollout: operation Name() returned empty string")
	}
	specFn := effectiveSpecFn(cfg, mutator)
	assertSpecDeterministic(name, specFn)
	reg.RegisterOperation(op) // panics on duplicate name

	wf := &factoryWorkflow{op: op, specFn: specFn, activityOpts: cfg.ActivityOptions}
	r.RegisterWorkflowWithOptions(wf.Execute, workflow.RegisterOptions{Name: name})
	return wf.Execute
}

// effectiveSpecFn returns the spec function the factory will run: the
// raw Spec when defaults are suppressed or no mutator is supplied,
// otherwise Spec composed with the mutator (captured here, at
// registration time, so the result is replay-safe).
func effectiveSpecFn(cfg WorkflowConfig, mutator SpecMutator) func(rollout.Cluster) rollout.RolloutSpec {
	if cfg.SuppressDefaultPreflights || mutator == nil {
		return cfg.Spec
	}
	return wrapWithMutator(cfg.Spec, mutator)
}

// wrapWithMutator composes mutator after inner, capturing mutator at
// registration time. Both are pure, so the result is replay-safe.
func wrapWithMutator(
	inner func(rollout.Cluster) rollout.RolloutSpec,
	mutator SpecMutator,
) func(rollout.Cluster) rollout.RolloutSpec {
	return func(c rollout.Cluster) rollout.RolloutSpec {
		return mutator(c, inner(c))
	}
}

// withRolloutOptions installs the default rollout activity budget on ctx.
// It is the controller's with<X>Options idiom (see internal/temporal/workflows/
// shared.go). Operations whose per-node RPC blocks on apiserver readiness
// override it via WorkflowConfig.ActivityOptions with
// WithRolloutNodeAgentApplyOptions.
func withRolloutOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		ScheduleToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 2 * time.Second,
			MaximumInterval: 10 * time.Second,
		},
	})
}

// WithRolloutNodeAgentApplyOptions sizes a rollout operation's activities
// (notably ApplyToNode / RollbackNode) for a per-node agent RPC that
// blocks on kube-apiserver readiness — e.g. EnableAPIServerAuthenticationConfig.
// That RPC gates on a mutation-aware readiness wait: ~200s for the forward
// apply, and on failure re-runs the same wait for the restore, ~400s worst
// case before the business Outcome can ride the response back. StartToCloseTimeout
// must exceed that, or a slow-but-healthy apiserver restart times the activity
// out, cancels the agent ctx mid-wait, and is misclassified as an apply
// failure that cascade-rolls-back the whole control plane. Layering: agent
// internal (~400s worst) < StartToClose (8m) < ScheduleToClose (20m). Applied to
// every activity the rollout dispatches; fast activities (preflights, guards)
// finish well inside the larger cap. HeartbeatTimeout bounds crash recovery: the
// long agent activities emit heartbeats, so a lost worker is detected within
// ~2m and the node retried on a fresh worker; the Enable/Disable RPCs are
// idempotent, so that retry is safe.
func WithRolloutNodeAgentApplyOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    8 * time.Minute,
		ScheduleToCloseTimeout: 20 * time.Minute,
		HeartbeatTimeout:       2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 2 * time.Second,
			MaximumInterval: 10 * time.Second,
		},
	})
}

// withLoaderOptions installs the fast-DB activity budget used by the
// workflow-side cluster load: 10s StartToClose, 2m ScheduleToClose, in the
// controller's with<X>Options idiom.
func withLoaderOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    10 * time.Second,
		ScheduleToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 2 * time.Second,
			MaximumInterval: 10 * time.Second,
		},
	})
}

// factoryWorkflow loads the cluster, then calls Execute.
type factoryWorkflow struct {
	op     rollout.CompiledOperation
	specFn func(rollout.Cluster) rollout.RolloutSpec
	// activityOpts, when non-nil, selects the activity-option context every
	// activity this workflow dispatches runs under (see
	// WorkflowConfig.ActivityOptions). nil → withRolloutOptions.
	activityOpts func(workflow.Context) workflow.Context
}

// Execute is the Temporal workflow body. Outcome-class failures
// surface via Result.Status; executor-internal failures (registry
// miss, unsupported strategy) surface as a Go error.
func (w *factoryWorkflow) Execute(ctx workflow.Context, req *SimpleRequest) (*SimpleResponse, error) {
	withOpts := w.activityOpts
	if withOpts == nil {
		withOpts = withRolloutOptions
	}
	ctx = withOpts(ctx)

	if req == nil {
		return nil, temporal.NewNonRetryableApplicationError(
			"nil request", "InvalidArgument", nil,
		)
	}
	clusterID, err := uuid.Parse(req.ClusterID)
	if err != nil {
		return nil, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("parse cluster id %q: %v", req.ClusterID, err),
			"InvalidArgument", nil,
		)
	}

	log := workflow.GetLogger(ctx)

	target, err := LoadClusterForWorkflow(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	log.Info(
		"loaded cluster",
		"clusterID", clusterID.String(),
		"operation", w.op.Name(),
		"clusterName", target.Name(),
		"cpNodes", len(target.ControlPlaneNodes()),
	)

	spec := w.specFn(target)

	result, execErr := Execute(ctx, target, w.op, spec)
	if execErr != nil {
		return nil, fmt.Errorf("executor: %w", execErr)
	}
	if rerr := result.Err(); rerr != nil {
		log.Warn("rollout failed", "err", rerr)
	}

	resp := &SimpleResponse{
		ClusterName: target.Name(),
		Result:      result,
	}
	log.Info(
		"rollout complete",
		"status", string(result.Status),
		"applied", len(result.Applied()),
		"unchanged", len(result.Unchanged()),
		"failed", len(result.Failed()),
	)
	return resp, nil
}

// LoadClusterForWorkflow dispatches ActivityLoadClusterForRollout and rewraps its
// serializable inventory into a rollout.Cluster via the controller adapter. A
// rollout.Cluster is an interface and cannot cross the activity boundary,
// so the activity ships raw db rows and this helper rewraps them. Custom
// workflows use the same helper as factory-generated rollout workflows.
func LoadClusterForWorkflow(ctx workflow.Context, clusterID uuid.UUID) (rollout.Cluster, error) {
	var resp LoadClusterForRolloutResponse
	if err := workflow.ExecuteActivity(
		withLoaderOptions(ctx), ActivityLoadClusterForRollout,
		&LoadClusterForRolloutRequest{ClusterID: clusterID},
	).Get(ctx, &resp); err != nil {
		return nil, fmt.Errorf("load_cluster_for_rollout: %w", err)
	}
	return adaptercontroller.NewCluster(resp.Cluster, resp.Nodes, resp.CPNodes, resp.NodeAgentPort), nil
}

// ActivityLoadClusterForRollout loads a cluster's serializable inventory for
// the workflow-side rewrap into a rollout.Cluster.
const (
	ActivityLoadClusterForRollout = "rollout.load_cluster_for_rollout"
)

type LoadClusterForRolloutRequest struct {
	ClusterID uuid.UUID
}

// LoadClusterForRolloutResponse carries serializable controller inventory: the
// cluster row, all of its nodes, and its control-plane subset. The workflow
// rewraps it via adaptercontroller.NewCluster. NodeAgentPort rides along so the
// workflow — which must stay free of process globals for replay determinism —
// can build each node's gRPC endpoint.
type LoadClusterForRolloutResponse struct {
	Cluster       db.Cluster
	Nodes         []db.Node
	CPNodes       []db.Node
	NodeAgentPort string
}

// clusterDataLoader loads a cluster's serializable inventory. Satisfied by
// adapters/controller.Loader (LoadClusterData).
type clusterDataLoader interface {
	LoadClusterData(ctx context.Context, clusterID uuid.UUID) (db.Cluster, []db.Node, []db.Node, error)
}

// ClusterLoaderActivity hosts the workflow-side cluster load. It is registered
// separately from Activities because it returns serializable db inventory (a
// rollout.Cluster interface cannot cross the activity boundary) rather than a
// resolved Operation+Cluster.
type ClusterLoaderActivity struct {
	loader        clusterDataLoader
	nodeAgentPort string
}

// NewClusterLoaderActivity builds the load activity over a clusterDataLoader
// (the controller's adapters/controller.Loader) and the agent port applied to every
// node's endpoint.
func NewClusterLoaderActivity(loader clusterDataLoader, nodeAgentPort string) *ClusterLoaderActivity {
	return &ClusterLoaderActivity{loader: loader, nodeAgentPort: nodeAgentPort}
}

// Register registers LoadClusterForRollout under its stable activity name.
func (c *ClusterLoaderActivity) Register(r Registerer) {
	r.RegisterActivityWithOptions(c.LoadClusterForRollout, temporalactivity.RegisterOptions{Name: ActivityLoadClusterForRollout})
}

// LoadClusterForRollout loads the cluster row plus its node and control-plane
// inventory. The error stays a plain (retryable) error under the loader
// activity's retry policy.
func (c *ClusterLoaderActivity) LoadClusterForRollout(ctx context.Context, req *LoadClusterForRolloutRequest) (*LoadClusterForRolloutResponse, error) {
	cluster, nodes, cpNodes, err := c.loader.LoadClusterData(ctx, req.ClusterID)
	if err != nil {
		return nil, fmt.Errorf("load cluster %s: %w", req.ClusterID, err)
	}
	return &LoadClusterForRolloutResponse{
		Cluster:       cluster,
		Nodes:         nodes,
		CPNodes:       cpNodes,
		NodeAgentPort: c.nodeAgentPort,
	}, nil
}
