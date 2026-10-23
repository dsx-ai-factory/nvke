// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	// reloadMinInterval bounds how often a *completed* reload attempt
	// triggers another real NewLocal call — mirroring auth/authn/keys.go's
	// jwksRefreshThrottle — not how often a caller may call reload. A caller
	// that arrives while a load is genuinely in flight always joins that load
	// via reloading.Do in (*reloadable).reload, regardless of this constant;
	// reloadMinInterval only ever gates the one goroutine singleflight
	// selects to run the closure, deciding whether it does a fresh load or
	// reuses the previous, still-fresh completed outcome. It is a fixed
	// constant, not AUTHZ_RELOAD_INTERVAL: the poll interval is how often
	// this design decides to check on its own schedule; this is a floor
	// under how often a completed check can trigger another one
	// (design-authz-hot-reload.md §3).
	reloadMinInterval = 5 * time.Second

	// ReloadOutcomeSucceeded, ReloadOutcomeFailed and ReloadOutcomeThrottled
	// are the nke_authz_reload_total{outcome} label values, a closed set.
	ReloadOutcomeSucceeded ReloadOutcomeLabel = "succeeded"
	ReloadOutcomeFailed    ReloadOutcomeLabel = "failed"
	ReloadOutcomeThrottled ReloadOutcomeLabel = "throttled"

	// ReloadSourcePoll and ReloadSourceEndpoint are the
	// nke_authz_reload_total{source} label values, and the two callers of
	// (*reloadable).reload: the background poll loop and the on-demand route.
	ReloadSourcePoll     = "poll"
	ReloadSourceEndpoint = "endpoint"
)

// ReloadOutcomeLabel is the nke_authz_reload_total{outcome} label value, a
// closed set.
type ReloadOutcomeLabel string

// ReloadRecorder counts one reload attempt outcome. A function rather than a
// *prometheus.CounterVec so this package does not depend on internal/metrics,
// matching the shape Recorder already establishes for the request path.
type ReloadRecorder func(outcome ReloadOutcomeLabel, source string)

// BundleChangeRecorder marks that a successful reload's content actually
// differs from what was live before it -- called only on genuine content
// change, never on every successful reload. See reload's own doc comment.
type BundleChangeRecorder func()

// ReloadOutcome is one reload attempt's result, returned to every caller that
// asked for it (whether it ran the load itself or joined one already
// running) and cached for a caller the throttle answers without a new load.
type ReloadOutcome struct {
	// Source is which trigger asked for this attempt: ReloadSourcePoll or
	// ReloadSourceEndpoint. Recorded on the outcome so a throttled or joined
	// caller can still see what actually triggered the load it is reporting.
	Source string

	// Err is the underlying NewLocal error, nil on success. A throttled
	// outcome carries the previous attempt's Err, since nothing new was
	// attempted.
	Err error

	// Throttled is true when this call landed within reloadMinInterval of the
	// previous attempt's completion and no new load was attempted -- the rest
	// of this struct reports that previous attempt's outcome instead.
	Throttled bool

	// Changed is true only when a successful load's content hash differs from
	// what was live immediately before it. False on failure, false on a
	// throttled call, and false on a successful load that reproduced
	// byte-identical content.
	Changed bool

	// local is the newly-published bundle on success, nil otherwise. Kept
	// unexported: nothing outside this package needs the value itself, only
	// whether/what changed.
	local *Local
}

// reloadable wraps a *Local behind an atomic pointer so Authorize always reads
// a complete, validated version — never a Local under construction, never one
// with some fields from an old load and some from a new one.
//
// reload collapses every concurrent reload attempt — poll or on-demand, mixed
// or not — onto one in-flight load-and-publish sequence, mirroring
// auth/authn/keys.go's jwksKeySource.refreshing. This is what makes Store
// calls impossible to land out of order: only one sequence is ever running,
// so there is never a second, slower sequence left to publish a stale result
// after a faster one already won. A caller that arrives while a sequence is
// already running does not start its own — it waits and receives that
// sequence's outcome, exactly as if it had called reload a moment earlier
// (design-authz-hot-reload.md §3).
type reloadable struct {
	// bundleSource is what loader reads through -- a BundleSource
	// (bundle.go), not a raw directory string, so this holds the same seam
	// NewLocal reads through and a future non-directory source would not
	// require a second field here.
	bundleSource BundleSource

	// loader builds a *Local from bundleSource. In every real deployment it is
	// newLocalFromSource (local.go) closed over the configured issuer set, so
	// a reload checks roleMappings.json's issuers exactly as startup did. Only
	// a test overrides it, to inject a delay or a controllable failure to land
	// a race deterministically (§7's concurrent-join and no-torn-read tests).
	loader func(ctx context.Context, source BundleSource) (*Local, error)

	// minInterval is the throttle floor reload applies, reloadMinInterval in
	// every real deployment. Only a test overrides it, to zero, so a stress
	// test can drive many real publications in a short run.
	minInterval time.Duration

	current atomic.Pointer[Local]

	reloading singleflight.Group

	mu            sync.Mutex
	everCompleted bool          // false only until the very first reload attempt finishes; see reload
	lastCompleted time.Time     // when the last attempt (success or failure) finished — not started
	lastOutcome   ReloadOutcome // that attempt's result, served to a throttled caller

	logger                *slog.Logger
	recordReload          ReloadRecorder
	recordChange          BundleChangeRecorder
	recordGroupOnlyWrites GroupOnlyWriteRecorder
}

var (
	// errUnexpectedSingleflightResultType marks the unreachable branch a
	// checked type assertion still has to handle: reload's own closure never
	// returns anything but a ReloadOutcome.
	errUnexpectedSingleflightResultType = errors.New("reload: singleflight returned an unexpected type")

	_ Authorizer = (*reloadable)(nil)
)

// newReloadable wraps initial -- the bundle Setup already built and validated
// synchronously -- behind a reloadable. initial is Stored into current before
// this returns, so reloadable.current is never observably empty (§3's
// bootstrap-sequencing requirement). Deliberately does not set everCompleted/
// lastCompleted/lastOutcome from initial: those fields govern the throttle
// for calls to reload specifically, and the bootstrap load never went through
// reload/singleflight at all. The poll goroutine's first tick is therefore
// still, correctly, the first-ever call to reload against this reloadable —
// never throttled, exactly as an implementer who read only reload's own
// comment would expect.
//
// Takes dir as a plain string, the same parameter every caller (Setup,
// today's only one, and every test in this package) already has in hand, and
// wraps it in a DirBundleSource itself. What reload's own mechanics read
// through afterward is bundleSource, the BundleSource interface (bundle.go)
// -- not dir -- so a future non-directory BundleSource only changes what a
// caller of a hypothetical newReloadableFromSource would pass, never
// anything below this constructor.
//
// issuers is the same set Setup built initial with. A reload that names an
// issuer outside it fails, and the previous bundle stays live. Every reloaded
// Local logs through initial's logger, as initial does.
//
// recordGroupOnlyWrites receives each published bundle's group-only write
// count, as Setup does for initial. Nil records nothing.
func newReloadable(
	dir string,
	issuers issuerSet,
	initial *Local,
	logger *slog.Logger,
	recordReload ReloadRecorder,
	recordChange BundleChangeRecorder,
	recordGroupOnlyWrites GroupOnlyWriteRecorder,
) *reloadable {
	if logger == nil {
		logger = slog.Default()
	}
	if recordReload == nil {
		recordReload = func(ReloadOutcomeLabel, string) {}
	}
	if recordChange == nil {
		recordChange = func() {}
	}
	r := &reloadable{
		bundleSource: DirBundleSource{Dir: dir},
		loader: func(ctx context.Context, source BundleSource) (*Local, error) {
			return newLocalFromSource(ctx, source, issuers, initial.logger)
		},
		minInterval:           reloadMinInterval,
		logger:                logger,
		recordReload:          recordReload,
		recordChange:          recordChange,
		recordGroupOnlyWrites: recordGroupOnlyWrites,
	}
	r.current.Store(initial)
	return r
}

func (r *reloadable) Name() string { return r.current.Load().Name() }

func (r *reloadable) Authorize(ctx context.Context, q Query) (Decision, error) {
	return r.current.Load().Authorize(ctx, q)
}

func (r *reloadable) Ready(ctx context.Context) error {
	return r.current.Load().Ready(ctx)
}

// reload runs one load-and-publish sequence, or joins one already running.
// ctx is threaded down to loader (r.bundleSource.Load, for DirBundleSource,
// via newLocalFromSource), matching pollLoop's and TriggerReload's call shape
// and auth/authn/keys.go's maybeRefresh(ctx) precedent. DirBundleSource
// itself ignores it -- a bundle read is a handful of small local files, not
// a network call worth canceling mid-flight -- but a future non-directory
// BundleSource (S3, a CRD) may not have that luxury, so the plumbing is
// there for it already rather than threaded through a second time later.
// Every caller — poll or on-demand — calls reloading.Do first, before any
// throttle check, so a caller that arrives while a load is genuinely in
// flight always joins it and receives that load's real, current outcome. Only
// the one goroutine singleflight actually selects to run the closure ever
// evaluates the throttle, and only against the timestamp of the last
// *completed* attempt — never against a caller's own arrival time. This
// mirrors auth/authn/keys.go's maybeRefresh: every caller calls
// refreshing.Do first; the throttle check runs inside that shared closure, so
// a caller arriving mid-fetch waits for the fetch already running, and only a
// caller arriving after the previous fetch fully finished can be throttled.
// source is poll or endpoint, recorded on the outcome.
func (r *reloadable) reload(ctx context.Context, source string) ReloadOutcome {
	v, _, _ := r.reloading.Do("reload", func() (any, error) {
		r.mu.Lock()
		// everCompleted — not just a zero-valued lastCompleted — is what makes
		// the very first reload attempt in this reloadable's life
		// unthrottleable. Without it, a zero-valued lastCompleted plus a
		// zero-valued lastOutcome would be indistinguishable from "the
		// previous reload legitimately completed at the Unix epoch with
		// local == nil, err == nil" — an ambiguous outcome a caller could
		// mistake for success. Gating on everCompleted instead means the
		// first-ever call always falls through to a real load below, never
		// into the throttle branch, so no caller ever observes that zero
		// value.
		lastCompleted := r.lastCompleted
		throttle := r.everCompleted && time.Since(lastCompleted) < r.minInterval
		previous := r.lastOutcome
		r.mu.Unlock()

		if throttle {
			previous.Source = source
			previous.Throttled = true
			previous.Changed = false
			r.logger.Info("authorization bundle reload throttled",
				"source", source, "dir", r.bundleSource,
				"since_last_attempt_ms", time.Since(lastCompleted).Milliseconds(),
			)
			r.recordReload(ReloadOutcomeThrottled, source)
			return previous, previous.Err
		}

		start := time.Now()
		next, err := r.loader(ctx, r.bundleSource)
		outcome := ReloadOutcome{Source: source, Err: err}
		if err != nil {
			r.logger.Error("authorization bundle reload failed",
				"source", source, "dir", r.bundleSource,
				"duration_ms", time.Since(start).Milliseconds(),
				"error", err,
			)
			r.recordReload(ReloadOutcomeFailed, source)
		} else {
			previous := r.current.Load()
			changed := previous == nil || previous.hash != next.hash
			outcome.local = next
			outcome.Changed = changed
			r.current.Store(next) // the only Store in this design; reloading
			// guarantees only one call to this closure is ever running, so
			// this can never race another one of itself.

			r.logger.Info("authorization bundle reloaded",
				"source", source, "dir", r.bundleSource,
				"duration_ms", time.Since(start).Milliseconds(),
				"changed", changed,
			)
			r.recordReload(ReloadOutcomeSucceeded, source)
			// The gauge follows the bundle in force, so it is set on every
			// publish; a failed reload above leaves it with the bundle still
			// serving. The per-operation lines come only with a change:
			// repeating them every poll would bury the change they exist to
			// show.
			var groupLogger *slog.Logger
			if changed {
				r.recordChange()
				groupLogger = r.logger
			}
			reportGroupOnlyWrites(groupLogger, r.recordGroupOnlyWrites, next)
		}

		r.mu.Lock()
		r.everCompleted = true
		r.lastCompleted = time.Now() // set on completion, not on entry — the
		r.lastOutcome = outcome      // throttle floor measures finished work,
		r.mu.Unlock()                // never a caller that merely started one
		return outcome, err
	})
	// Every caller of Do — whether it ran the closure or joined one already
	// running — receives the identical ReloadOutcome value back; singleflight
	// never fabricates a separate result per joiner. v is always that
	// ReloadOutcome: the closure above never returns anything else, so the
	// type assertion below cannot fail in practice, but it is checked anyway
	// rather than trusted blindly.
	outcome, ok := v.(ReloadOutcome)
	if !ok {
		return ReloadOutcome{Source: source, Err: errUnexpectedSingleflightResultType}
	}
	return outcome
}

// pollLoop calls reload on a fixed interval until ctx is canceled. Started by
// Setup as a goroutine, and stopped when the context Setup was called with is
// canceled — Setup already takes a context.Context and already has nothing
// else async to coordinate with (design-authz-hot-reload.md §2).
func (r *reloadable) pollLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reload(ctx, ReloadSourcePoll)
		}
	}
}
