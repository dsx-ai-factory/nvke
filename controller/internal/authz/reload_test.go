// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/auth"
)

// realActionsJSON is the shipped, generated actions.json -- every custom test
// bundle below reuses it verbatim, the same reason bundleWith
// (local_errors_test.go) does: checkEntityStore's action-vs-operations-table
// check runs against the process's real, compiled operations table, so a
// hand-written actions.json missing an entry (in particular
// reloadAuthzBundle, this design's own new action) would refuse to load for a
// reason unrelated to whatever the test is actually checking.
func realActionsJSON(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), actionsFile))
	require.NoError(t, err)
	return string(raw)
}

// writeReloadTestBundle (re)writes a flat (non-ConfigMap-shaped) bundle
// directory: os.Readlink on "<dir>/..data" reports fs.ErrNotExist here, so
// loadBundle takes resolvedBundleDir's fallback path and reads dir directly,
// same as every other test bundle in this package.
func writeReloadTestBundle(t *testing.T, dir, policies, roles string) {
	t.Helper()
	for name, content := range map[string]string{
		policiesFile: policies,
		actionsFile:  realActionsJSON(t),
		rolesFile:    roles,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
}

const (
	readerPermit = `permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource);`

	rolesGrantReader = `[
	 {"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]},
	 {"uid":{"type":"NKE::Principal","id":"` + subjectReader + `"},
	  "attrs":{},"parents":[{"type":"NKE::Role","id":"reader"}]}
	]`

	rolesRevokeReader = `[
	 {"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]},
	 {"uid":{"type":"NKE::Principal","id":"` + subjectReader + `"},"attrs":{},"parents":[]}
	]`
)

func readerQuery() Query {
	return Query{
		Principal: subjectReader, Issuer: "kube-apiserver", Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: someClusterID, Method: http.MethodGet,
	}
}

// recordingReloadCalls counts ReloadRecorder/BundleChangeRecorder calls, so a
// test can assert "no NewLocal call happened" or "the gauge advanced exactly
// once" without reaching into internal/metrics.
type reloadCalls struct {
	mu       sync.Mutex
	outcomes []ReloadOutcomeLabel
	sources  []string
	changed  int
}

func (c *reloadCalls) record(outcome ReloadOutcomeLabel, source string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outcomes = append(c.outcomes, outcome)
	c.sources = append(c.sources, source)
}

func (c *reloadCalls) recordChange() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.changed++
}

func (c *reloadCalls) count(outcome ReloadOutcomeLabel) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, o := range c.outcomes {
		if o == outcome {
			n++
		}
	}
	return n
}

func (c *reloadCalls) changedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

func newTestReloadable(t *testing.T, dir string, calls *reloadCalls) *reloadable {
	t.Helper()
	initial, err := NewLocal(dir)
	require.NoError(t, err)
	var record ReloadRecorder
	var change BundleChangeRecorder
	if calls != nil {
		record = calls.record
		change = calls.recordChange
	}
	return newReloadable(dir, initial, testLogger(), record, change)
}

// ---------------------------------------------------------------------------
// Fail-safe direction: the load-bearing property of this whole design.
// ---------------------------------------------------------------------------

func TestReloadNeverPublishesABadBundle(t *testing.T) {
	badPolicySyntax := "this is not cedar at all {{{"
	badShape := readerPermit + "\n" + `forbid(principal, action in NKE::Action::"write", resource)
		when { resource has site && resource.site == "prod" };`

	tests := []struct {
		name     string
		policies string
		roles    string
	}{
		{"unparseable cedar", badPolicySyntax, rolesGrantReader},
		{"empty roles.json", readerPermit, "[]"},
		{"a policy checkPolicyShape refuses", badShape, rolesGrantReader},
		{
			name:     "a dangling static parent",
			policies: readerPermit,
			roles: `[
			 {"uid":{"type":"NKE::Principal","id":"` + subjectReader + `"},
			  "attrs":{},"parents":[{"type":"NKE::Role","id":"ghost"}]}
			]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			goodDir := t.TempDir()
			writeReloadTestBundle(t, goodDir, readerPermit, rolesGrantReader)
			calls := &reloadCalls{}
			r := newTestReloadable(t, goodDir, calls)
			before := r.current.Load()

			badDir := t.TempDir()
			writeReloadTestBundle(t, badDir, tt.policies, tt.roles)
			r.dir = badDir

			outcome := r.reload(t.Context(), ReloadSourceEndpoint)
			require.Error(t, outcome.Err)
			assert.False(t, outcome.Changed)

			assert.Same(t, before, r.current.Load(),
				"a failed reload must not touch current, not even with an equal-looking replacement")

			decision, err := r.Authorize(t.Context(), readerQuery())
			require.NoError(t, err)
			assert.True(t, decision.Allow, "the original bundle must keep answering after a failed reload")

			assert.Equal(t, 1, calls.count(ReloadOutcomeFailed))
		})
	}
}

// The fifth NewLocal refusal mode named in the design: an action the compiled
// operations table needs that the reloaded bundle does not declare. Exercised
// separately because it needs a real actions.json missing one entry, not the
// realActionsJSON() helper every other case reuses verbatim.
func TestReloadRefusesABundleMissingACompiledAction(t *testing.T) {
	goodDir := t.TempDir()
	writeReloadTestBundle(t, goodDir, readerPermit, rolesGrantReader)
	r := newTestReloadable(t, goodDir, nil)
	before := r.current.Load()

	badDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(badDir, policiesFile), []byte(readerPermit), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(badDir, rolesFile), []byte(rolesGrantReader), 0o600))
	// actions.json with every action entity except one the compiled table
	// still needs -- read-group only, no write group and no per-operation
	// entities, so checkEntityStore's action loop finds a gap immediately.
	require.NoError(t, os.WriteFile(filepath.Join(badDir, actionsFile), []byte(`[
	 {"uid":{"type":"NKE::Action","id":"read"},"attrs":{},"parents":[]}
	]`), 0o600))
	r.dir = badDir

	outcome := r.reload(t.Context(), ReloadSourceEndpoint)
	require.Error(t, outcome.Err)
	assert.Same(t, before, r.current.Load())
}

// ---------------------------------------------------------------------------
// A successful reload actually takes effect.
// ---------------------------------------------------------------------------

func TestSuccessfulReloadTakesEffect(t *testing.T) {
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, rolesGrantReader)
	r := newTestReloadable(t, dir, nil)

	decision, err := r.Authorize(t.Context(), readerQuery())
	require.NoError(t, err)
	require.True(t, decision.Allow, "must be Allow before the reload for this test to mean anything")

	writeReloadTestBundle(t, dir, readerPermit, rolesRevokeReader)
	outcome := r.reload(t.Context(), ReloadSourceEndpoint)
	require.NoError(t, outcome.Err)
	assert.True(t, outcome.Changed)

	decision, err = r.Authorize(t.Context(), readerQuery())
	require.NoError(t, err)
	assert.False(t, decision.Allow, "the revocation must take effect with no restart, no new Local/Provider")
}

// ---------------------------------------------------------------------------
// Concurrency: the central fix (§3).
// ---------------------------------------------------------------------------

// blockingLoader wraps NewLocal with an injectable synchronization point, so
// a test can land two reload attempts genuinely overlapping in time rather
// than merely calling reload twice back to back.
type blockingLoader struct {
	dir     string
	entered chan struct{} // closed once, the instant the loader starts
	release chan struct{} // closed by the test to let the loader proceed
	calls   int32
}

func (b *blockingLoader) load(dir string) (*Local, error) {
	atomic.AddInt32(&b.calls, 1)
	close(b.entered)
	<-b.release
	return NewLocal(dir)
}

func newBlockingReloadable(t *testing.T, dir string) (*reloadable, *blockingLoader) {
	t.Helper()
	initial, err := NewLocal(dir)
	require.NoError(t, err)
	r := newReloadable(dir, initial, testLogger(), nil, nil)
	bl := &blockingLoader{dir: dir, entered: make(chan struct{}), release: make(chan struct{})}
	r.loader = bl.load
	return r, bl
}

// runConcurrentJoinCase is shared by the three shapes §7 names explicitly:
// on-demand/on-demand, poll/on-demand, poll/poll. Whichever pair of sources is
// passed, the assertions are the same: exactly one NewLocal call, both
// callers see the identical outcome, and neither is marked throttled, because
// both arrived while a load was genuinely in flight.
func runConcurrentJoinCase(t *testing.T, sourceA, sourceB string) {
	t.Helper()
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, rolesGrantReader)
	r, bl := newBlockingReloadable(t, dir)

	var outcomeA, outcomeB ReloadOutcome
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		outcomeA = r.reload(t.Context(), sourceA)
	}()

	select {
	case <-bl.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reload A never entered the loader")
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		outcomeB = r.reload(t.Context(), sourceB)
	}()

	// B has nothing to synchronize on directly (it should be parked inside
	// singleflight.Do, not the loader), so give the scheduler a beat before
	// releasing A -- if B incorrectly started its own load, this is also
	// roughly the window it would show up as a second entered signal, which
	// the call count below catches regardless of timing.
	time.Sleep(50 * time.Millisecond)
	close(bl.release)

	wg.Wait()

	assert.Equal(t, int32(1), atomic.LoadInt32(&bl.calls),
		"a caller arriving while a load is in flight must join it, never start a second NewLocal call")
	assert.Equal(t, outcomeA.local, outcomeB.local)
	assert.Equal(t, outcomeA.Err, outcomeB.Err)
	assert.False(t, outcomeA.Throttled)
	assert.False(t, outcomeB.Throttled, "a caller joining an in-flight load must never be marked throttled")
}

func TestConcurrentReloadsJoinOneInFlightLoad_EndpointEndpoint(t *testing.T) {
	runConcurrentJoinCase(t, ReloadSourceEndpoint, ReloadSourceEndpoint)
}

func TestConcurrentReloadsJoinOneInFlightLoad_PollEndpoint(t *testing.T) {
	runConcurrentJoinCase(t, ReloadSourcePoll, ReloadSourceEndpoint)
}

func TestConcurrentReloadsJoinOneInFlightLoad_PollPoll(t *testing.T) {
	runConcurrentJoinCase(t, ReloadSourcePoll, ReloadSourcePoll)
}

// The sequential case the throttle actually exists to gate: a caller arriving
// after the previous attempt has fully completed, within reloadMinInterval of
// its completion, is throttled and triggers no new NewLocal call -- the
// mirror image of the concurrent-join cases above, which must never be
// throttled.
func TestSequentialCallWithinThrottleWindowIsThrottled(t *testing.T) {
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, rolesGrantReader)
	calls := &reloadCalls{}
	r := newTestReloadable(t, dir, calls)

	first := r.reload(t.Context(), ReloadSourceEndpoint)
	require.NoError(t, first.Err)
	require.False(t, first.Throttled)

	second := r.reload(t.Context(), ReloadSourceEndpoint)
	require.NoError(t, second.Err)
	assert.True(t, second.Throttled, "a caller within reloadMinInterval of the previous attempt's "+
		"completion must be throttled")

	assert.Equal(t, 1, calls.count(ReloadOutcomeSucceeded), "only the first attempt may call NewLocal")
	assert.Equal(t, 1, calls.count(ReloadOutcomeThrottled))
}

// everCompleted, not a zero-valued lastCompleted, is what makes the very
// first reload attempt against a fresh reloadable unthrottleable.
func TestFirstEverReloadIsNeverThrottled(t *testing.T) {
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, rolesGrantReader)
	calls := &reloadCalls{}
	r := newTestReloadable(t, dir, calls)

	outcome := r.reload(t.Context(), ReloadSourcePoll)
	require.NoError(t, outcome.Err)
	assert.False(t, outcome.Throttled, "the first reload attempt ever made must always call NewLocal for real")
	assert.Equal(t, 1, calls.count(ReloadOutcomeSucceeded))
	assert.Equal(t, 0, calls.count(ReloadOutcomeThrottled))
}

// ---------------------------------------------------------------------------
// A single request is evaluated against exactly one, fully-loaded version.
// ---------------------------------------------------------------------------

// TestPinnedLocalUnaffectedBySubsequentReload is a deterministic proxy for
// the wait-free-read property, not a literal concurrent-injection test: it
// captures a *Local via Load(), reloads synchronously to completion, and only
// then calls Authorize on the pinned value. Nothing here runs concurrently.
//
// That proxy is sufficient for this implementation specifically because
// reloadable.Authorize is exactly `return r.current.Load().Authorize(ctx,
// q)`: Load() happens exactly once per call, before any authorization work
// begins, so "a pinned *Local keeps answering with its own frozen content
// regardless of a later Store" and "an in-flight Authorize call is unaffected
// by a concurrent reload" are the same property here. A literal
// concurrent-injection test -- one that starts a reload while an Authorize
// call is genuinely in progress -- would need an injectable delay inside
// Local.Authorize itself, which has no test seam today.
func TestPinnedLocalUnaffectedBySubsequentReload(t *testing.T) {
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, rolesGrantReader)
	r := newTestReloadable(t, dir, nil)

	// The version pinned by calling Load() itself, before any reload
	// published a new one -- exactly what reloadable.Authorize does at the
	// top of every call.
	pinned := r.current.Load()

	writeReloadTestBundle(t, dir, readerPermit, rolesRevokeReader)
	outcome := r.reload(t.Context(), ReloadSourceEndpoint)
	require.NoError(t, outcome.Err)

	decision, err := pinned.Authorize(t.Context(), readerQuery())
	require.NoError(t, err)
	assert.True(t, decision.Allow,
		"a *Local pinned before the reload must keep answering as it did, unaffected by the swap")

	decision, err = r.Authorize(t.Context(), readerQuery())
	require.NoError(t, err)
	assert.False(t, decision.Allow, "a fresh Load() after the reload must see the new version")
}

// ---------------------------------------------------------------------------
// Generation pinning under a simulated ConfigMap swap, narrowed fallback.
// ---------------------------------------------------------------------------

// buildConfigMapMount lays out mountDir the way kubelet's atomic writer does:
// two timestamped generation directories holding real files, "..data"
// symlinked to the first, and the three well-known file names symlinked
// through "..data".
func buildConfigMapMount(t *testing.T, mountDir, gen1Roles, gen2Roles string) (string, string) {
	t.Helper()
	gen1 := filepath.Join(mountDir, "..gen1")
	gen2 := filepath.Join(mountDir, "..gen2")
	require.NoError(t, os.Mkdir(gen1, 0o700))
	require.NoError(t, os.Mkdir(gen2, 0o700))

	for genDir, roles := range map[string]string{gen1: gen1Roles, gen2: gen2Roles} {
		for name, content := range map[string]string{
			policiesFile: readerPermit,
			actionsFile:  realActionsJSON(t),
			rolesFile:    roles,
		} {
			require.NoError(t, os.WriteFile(filepath.Join(genDir, name), []byte(content), 0o600))
		}
	}

	require.NoError(t, os.Symlink("..gen1", filepath.Join(mountDir, "..data")))
	for _, name := range []string{policiesFile, actionsFile, rolesFile} {
		require.NoError(t, os.Symlink(filepath.Join("..data", name), filepath.Join(mountDir, name)))
	}
	return gen1, gen2
}

// repointConfigMapData returns its error rather than asserting directly: it
// runs concurrently with the load under test, in a goroutine that is not the
// one running the test function, where testify's require/assert must not be
// called.
func repointConfigMapData(mountDir, target string) error {
	tmp := filepath.Join(mountDir, "..data_tmp")
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(mountDir, "..data"))
}

func hashOf(t *testing.T, genDir string) [sha256.Size]byte {
	t.Helper()
	h := sha256.New()
	for _, name := range []string{policiesFile, actionsFile, rolesFile} {
		raw, err := os.ReadFile(filepath.Join(genDir, name))
		require.NoError(t, err)
		h.Write(raw)
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// A load never reads a mix of two generations, even under a concurrent
// "..data" repoint. Because resolvedBundleDir resolves "..data" exactly once,
// up front, and every one of the three reads goes through that resolved
// directory rather than back through the mutable symlink, there is no window
// left in which a rename can land between two of the three reads -- this is
// what the stress loop below is exercising: many attempts, a concurrent
// flip on every one, and every successful result matching one generation's
// hash exactly, never neither.
func TestGenerationPinningSurvivesAConcurrentConfigMapSwap(t *testing.T) {
	mountDir := t.TempDir()
	gen1, gen2 := buildConfigMapMount(t, mountDir, rolesGrantReader, rolesRevokeReader)
	hash1, hash2 := hashOf(t, gen1), hashOf(t, gen2)

	for i := range 50 {
		repointErr := make(chan error, 1)
		go func() {
			if i%2 == 0 {
				repointErr <- repointConfigMapData(mountDir, "..gen2")
			} else {
				repointErr <- repointConfigMapData(mountDir, "..gen1")
			}
		}()

		local, err := NewLocal(mountDir)
		require.NoError(t, <-repointErr)
		if err != nil {
			// A transient error racing the repoint (e.g. the old generation
			// symlink briefly unreadable) is acceptable; a torn read is not.
			// Any error here is "the reload fails," §4's ordinary fail-safe
			// path, not the failure this test exists to catch.
			continue
		}
		assert.Truef(t, local.hash == hash1 || local.hash == hash2,
			"iteration %d loaded a hash matching neither generation -- a torn read", i)
	}
}

func TestGenerationPinningFallsBackOnlyOnENOENT(t *testing.T) {
	t.Run("no ..data at all falls back to dir", func(t *testing.T) {
		dir := t.TempDir()
		writeReloadTestBundle(t, dir, readerPermit, rolesGrantReader)
		_, err := NewLocal(dir)
		require.NoError(t, err, "a plain directory with no ..data must load through the fallback path")
	})

	t.Run("a non-ENOENT Readlink error is a load failure, not a silent fallback", func(t *testing.T) {
		dir := t.TempDir()
		writeReloadTestBundle(t, dir, readerPermit, rolesGrantReader)
		// "..data" exists but is not a symlink: os.Readlink reports EINVAL,
		// not fs.ErrNotExist. Resolving this must not fall back to reading
		// dir directly, even though dir itself holds a perfectly loadable
		// bundle -- silently downgrading here is exactly the regression
		// this test exists to catch.
		require.NoError(t, os.Mkdir(filepath.Join(dir, "..data"), 0o700))

		_, err := NewLocal(dir)
		require.Error(t, err)
		assert.NotErrorIs(t, err, os.ErrNotExist)
	})
}

// ---------------------------------------------------------------------------
// The content-hash gauge tracks content, not attempts.
// ---------------------------------------------------------------------------

func TestBundleChangedGaugeTracksContentNotAttempts(t *testing.T) {
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, rolesGrantReader)
	calls := &reloadCalls{}
	r := newTestReloadable(t, dir, calls)

	for range 2 {
		outcome := r.reload(t.Context(), ReloadSourcePoll)
		require.NoError(t, outcome.Err)
		assert.False(t, outcome.Changed, "an unchanged reload must not report changed")
		time.Sleep(reloadMinInterval + 10*time.Millisecond)
	}
	assert.Equal(t, 0, calls.changedCount(), "the gauge must not advance while the content is unchanged")
	assert.Equal(t, 2, calls.count(ReloadOutcomeSucceeded))

	writeReloadTestBundle(t, dir, readerPermit, rolesRevokeReader)
	outcome := r.reload(t.Context(), ReloadSourcePoll)
	require.NoError(t, outcome.Err)
	assert.True(t, outcome.Changed)
	assert.Equal(t, 1, calls.changedCount(), "the gauge must advance exactly once, on the reload that actually changed")
}

// ---------------------------------------------------------------------------
// Poll interval honored, including disabled.
// ---------------------------------------------------------------------------

func TestPollLoopReloadsOnInterval(t *testing.T) {
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, rolesGrantReader)
	calls := &reloadCalls{}
	r := newTestReloadable(t, dir, calls)

	ctx, cancel := context.WithCancel(t.Context())
	go r.pollLoop(ctx, 20*time.Millisecond)
	t.Cleanup(cancel)

	require.Eventually(t, func() bool {
		return calls.count(ReloadOutcomeSucceeded)+calls.count(ReloadOutcomeThrottled) >= 1
	}, 2*time.Second, 10*time.Millisecond, "the poll loop must call reload on its own schedule")
}

// TestConcurrentAuthorizeSurvivesConcurrentReloads is design-authz-hot-
// reload.md §7's stress test: many goroutines calling Authorize continuously
// while pollLoop reloads on a short interval, run under -race. Its value is
// almost entirely in the race detector, not in the assertions below -- a
// data race here would mean a caller could observe a torn or partially
// published *Local, which is the property the rest of this file proves
// piecewise (TestPinnedLocalUnaffectedBySubsequentReload,
// TestConcurrentReloadsJoinOneInFlightLoad_*). Content also flips between two
// valid bundles every reload, so Authorize genuinely sees both a stable and a
// changing r.current.Load() across the run, rather than reloading the same
// bytes onto themselves.
func TestConcurrentAuthorizeSurvivesConcurrentReloads(t *testing.T) {
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, rolesGrantReader)
	r := newTestReloadable(t, dir, nil)

	ctx, cancel := context.WithCancel(t.Context())

	// flipDone confirms the file-flipping goroutine below has actually
	// stopped -- not merely been told to, via cancel() -- before this test
	// returns. Without that join, t.TempDir()'s own cleanup can start
	// removing dir while that goroutine is still mid os.WriteFile, which
	// flakes as "directory not empty" rather than anything about this
	// design's own concurrency.
	flipDone := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-flipDone
	})

	// errCh carries failures from every background goroutine below -- both
	// the file-flipping goroutine and the Authorize workers -- back to the
	// test goroutine, since require/t.Fatal are unsafe to call from a
	// goroutine other than the one running the test.
	errCh := make(chan error, 1)
	reportErr := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}

	// Flips the roles file every reload tick so pollLoop is publishing
	// genuinely different content each time, not a no-op reload of identical
	// bytes. actionsJSON is read once up front, not inside the goroutine
	// below: realActionsJSON calls require, which is unsafe to call from any
	// goroutine but the one running the test.
	actionsJSON := realActionsJSON(t)
	var flips int32
	go func() {
		defer close(flipDone)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n := atomic.AddInt32(&flips, 1)
				roles := rolesGrantReader
				if n%2 == 0 {
					roles = rolesRevokeReader
				}
				for name, content := range map[string]string{
					policiesFile: readerPermit,
					actionsFile:  actionsJSON,
					rolesFile:    roles,
				} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
						reportErr(err)
						return
					}
				}
			}
		}
	}()

	go r.pollLoop(ctx, 3*time.Millisecond)

	const workers = 8
	var wg sync.WaitGroup
	deadline := time.Now().Add(300 * time.Millisecond)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if _, err := r.Authorize(t.Context(), readerQuery()); err != nil {
					reportErr(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	select {
	case err := <-errCh:
		t.Fatalf("concurrent Authorize/reload traffic produced an error: %v", err)
	default:
	}
}

func TestReloadDisabledNeverWrapsTheEngine(t *testing.T) {
	provider, err := Setup(t.Context(), Options{
		BundleDir:      bundleDir(t),
		ReloadInterval: 0,
	}, testLogger())
	require.NoError(t, err)
	require.NotNil(t, provider)

	_, ok := provider.authorizer.(*reloadable)
	assert.False(t, ok, "AUTHZ_RELOAD_INTERVAL<=0 must leave the bare engine in place, no reloadable wrapper")

	_, ok = provider.TriggerReload(t.Context(), ReloadSourceEndpoint)
	assert.False(t, ok, "TriggerReload must report unavailable when reload was never enabled")
}

// A failed-reload metric is never recorded while reload is disabled, since no
// reload is ever attempted -- proving "disabled" is really disabled and not
// merely a very long interval that this particular test outlasts.
func TestReloadDisabledNeverAttemptsAReload(t *testing.T) {
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, rolesGrantReader)

	provider, err := Setup(t.Context(), Options{BundleDir: dir, ReloadInterval: 0}, testLogger())
	require.NoError(t, err)
	require.NotNil(t, provider)

	// Mutate the bundle to something NewLocal would refuse, then just keep
	// asking the (unwrapped) engine for decisions -- if anything were
	// secretly reloading, this would eventually flip.
	writeReloadTestBundle(t, dir, "not cedar {{{", rolesGrantReader)

	middleware, err := provider.Middleware(nil)
	_ = middleware
	require.Error(t, err, "Middleware(nil) is a different failure and not what this test checks")

	decision, err := provider.authorizer.Authorize(t.Context(), readerQuery())
	require.NoError(t, err)
	assert.True(t, decision.Allow, "with reload disabled the original bundle must keep answering indefinitely")
}

// ---------------------------------------------------------------------------
// Bootstrap: reloadable.current is never observably empty.
// ---------------------------------------------------------------------------

func TestBootstrapNeverLeavesCurrentEmpty(t *testing.T) {
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, rolesGrantReader)

	provider, err := Setup(t.Context(), Options{
		BundleDir:      dir,
		ReloadInterval: time.Hour, // long enough that no tick could plausibly fire during this test
	}, testLogger())
	require.NoError(t, err)
	require.NotNil(t, provider)

	require.NoError(t, provider.authorizer.Ready(t.Context()),
		"Ready must answer against the startup bundle immediately, before any poll tick")

	decision, err := provider.authorizer.Authorize(t.Context(), readerQuery())
	require.NoError(t, err)
	assert.True(t, decision.Allow)
}

// ---------------------------------------------------------------------------
// The on-demand route is itself Cedar-authorized, independent of the mode
// gate that server.serverImpl.ReloadAuthzBundle additionally applies.
// ---------------------------------------------------------------------------

func TestReloadRouteIsCedarAuthorized(t *testing.T) {
	local, err := NewLocal(bundleDir(t))
	require.NoError(t, err)

	mux := http.NewServeMux()
	api.HandlerWithOptions(nil, api.StdHTTPServerOptions{BaseRouter: mux, BaseURL: api.BasePath})
	resolver, err := NewResolver(mux)
	require.NoError(t, err)

	middleware, err := NewMiddleware(local, resolver, ModeEnforcing, testLogger(), nil)
	require.NoError(t, err)

	reached := false
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	request := func(subject string) *http.Request {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, api.BasePath+"/authz/reload", http.NoBody)
		return r.WithContext(auth.ContextWithPrincipal(r.Context(), &auth.Principal{
			IssuerName: "site-apiserver",
			Issuer:     "https://kubernetes.default.svc.cluster.local",
			Subject:    subject,
		}))
	}

	t.Run("a caller with no role granting it is refused, and never reaches the handler", func(t *testing.T) {
		reached = false
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request(subjectStranger))
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.False(t, reached, "the ordinary Cedar deny must never call through to the handler")
	})

	t.Run("an operator is permitted through to the handler", func(t *testing.T) {
		reached = false
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request(subjectOperator))
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.True(t, reached, "operators-may-reload-authz-bundle (or operators-may-write) must permit this")
	})
}
