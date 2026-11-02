// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"context"

	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
)

// The phases of "bootstrap site", in the order they run. Each one is timed by a
// span and a "phase finished" log record, and is the phase field of the log
// records made while it runs.
const (
	phasePreflight        = "preflight"
	phaseBootstrapCluster = "bootstrap_cluster"
	phaseEngineDeploy     = "engine_deploy"
	phaseSiteCluster      = "site_cluster"
	phaseFinish           = "finish"

	// operationBootstrapSite is the operation field of every record the command logs.
	operationBootstrapSite = "bootstrap_site"
)

// phaseTracker moves one bootstrap run through its phases. It is used from the
// goroutine that runs the command.
type phaseTracker struct {
	run  *telemetry.Run
	base context.Context // the command's context; each phase starts from it

	current string
	ctx     context.Context
	end     func(error)

	// failed is set when the run entered a phase because an earlier one failed,
	// so the failure is not also reported against that later phase.
	failed bool
}

func newPhaseTracker(ctx context.Context) *phaseTracker {
	return &phaseTracker{run: telemetry.RunFrom(ctx), base: ctx}
}

// enter ends the current phase and starts the one called name, returning the
// context to use for it. previousErr is the error that ended the current phase, or
// nil when it ran to completion. Entering the phase the run is already in does
// nothing.
func (p *phaseTracker) enter(name string, previousErr error) context.Context {
	if p.current == name {
		return p.ctx
	}
	p.leave(previousErr)
	if previousErr != nil {
		p.failed = true
	}
	p.ctx, p.end = p.run.StartPhase(p.base, name)
	p.current = name
	return p.ctx
}

// close ends the current phase. err is the error that ended the run; it is not
// reported against a phase the run only entered to clean up after a failure.
func (p *phaseTracker) close(err error) {
	if p.failed {
		err = nil
	}
	p.leave(err)
}

func (p *phaseTracker) leave(err error) {
	if p.end == nil {
		return
	}
	p.end(err)
	p.current, p.ctx, p.end = "", nil, nil
}
