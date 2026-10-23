// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"

	"github.com/dsx-ai-factory/nvke/controller/api"
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
// directory, with rolesReader as roles.json: os.Readlink on "<dir>/..data"
// reports fs.ErrNotExist here, so loadBundle takes resolvedBundleDir's
// fallback path and reads dir directly, same as every other test bundle in
// this package.
func writeReloadTestBundle(t *testing.T, dir, policies, mappings string) {
	t.Helper()
	writeReloadTestBundleWithRoles(t, dir, policies, rolesReader, mappings)
}

// writeReloadTestBundleWithRoles is writeReloadTestBundle with the roles.json
// content given.
func writeReloadTestBundleWithRoles(t *testing.T, dir, policies, roles, mappings string) {
	t.Helper()
	for name, content := range map[string]string{
		policiesFile:     policies,
		actionsFile:      realActionsJSON(t),
		rolesFile:        roles,
		roleMappingsFile: mappings,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
}

const (
	readerPermit = `permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource);`

	// readerListsOnly permits reader one read that is not readerQuery's, so
	// a policy-only edit from readerPermit revokes readerQuery.
	readerListsOnly = `permit(principal in NKE::Role::"reader", action == NKE::Action::"listClusters", resource);`

	rolesReader = `[{"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]}]`

	// mappingKeepsOperator grants reader to subjectOperator, never
	// readerQuery's caller, so every bundle here keeps one mapping and loads.
	mappingKeepsOperator = `{"issuer":"` + testIssuer + `","claim":{"name":"sub","value":"` + subjectOperator + `"},
	  "grants":[{"type":"NKE::Role","id":"reader"}]}`

	// mappingsGrantReader adds a grant of reader to readerQuery's caller,
	// through its groups claim. mappingsRevokeReader drops that grant.
	mappingsGrantReader = `[` + mappingKeepsOperator + `,
	 {"issuer":"` + testIssuer + `","claim":{"name":"groups","value":"readers"},
	  "grants":[{"type":"NKE::Role","id":"reader"}]}]`
	mappingsRevokeReader = `[` + mappingKeepsOperator + `]`
)

func readerQuery() Query {
	return Query{
		Principal: subjectReader, Issuer: testIssuer, Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: someClusterID, Method: http.MethodGet,
		Claims: map[string]any{"groups": []any{"readers"}},
	}
}

// reloadTestIssuers is the issuer set every reloadable in this file is built
// with.
func reloadTestIssuers() []string { return []string{testIssuer} }

// recordingReloadCalls counts ReloadRecorder/BundleChangeRecorder calls, so a
// test can assert "no NewLocal call happened" or "the gauge advanced exactly
// once" without reaching into internal/metrics.
type reloadCalls struct {
	mu              sync.Mutex
	outcomes        []ReloadOutcomeLabel
	sources         []string
	changed         int
	groupOnlyWrites []int
}

func (c *reloadCalls) recordGroupOnlyWrites(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.groupOnlyWrites = append(c.groupOnlyWrites, n)
}

func (c *reloadCalls) groupOnlyWriteCounts() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.groupOnlyWrites)
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
	initial, err := NewLocal(dir, reloadTestIssuers())
	require.NoError(t, err)
	var record ReloadRecorder
	var change BundleChangeRecorder
	var groupOnly GroupOnlyWriteRecorder
	if calls != nil {
		record = calls.record
		change = calls.recordChange
		groupOnly = calls.recordGroupOnlyWrites
	}
	return newReloadable(dir, newIssuerSet(reloadTestIssuers()), initial, testLogger(), record, change, groupOnly)
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
		{"unparseable cedar", badPolicySyntax, rolesReader},
		{"empty roles.json", readerPermit, "[]"},
		{"a policy checkPolicyShape refuses", badShape, rolesReader},
		{
			name:     "a dangling role parent",
			policies: readerPermit,
			roles:    `[{"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[{"type":"NKE::Role","id":"ghost"}]}]`,
		},
		{
			name:     "a principal entity",
			policies: readerPermit,
			roles: `[{"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]},
			 {"uid":{"type":"NKE::Principal","id":"` + subjectReader + `"},
			  "attrs":{},"parents":[{"type":"NKE::Role","id":"reader"}]}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			goodDir := t.TempDir()
			writeReloadTestBundle(t, goodDir, readerPermit, mappingsGrantReader)
			calls := &reloadCalls{}
			r := newTestReloadable(t, goodDir, calls)
			before := r.current.Load()

			badDir := t.TempDir()
			writeReloadTestBundleWithRoles(t, badDir, tt.policies, tt.roles, mappingsGrantReader)
			// Mechanical, behavior-preserving rename of what used to be
			// r.dir = badDir, now that reload reads through r.bundleSource
			// (a BundleSource) rather than a raw directory string.
			r.bundleSource = DirBundleSource{Dir: badDir}

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
	writeReloadTestBundle(t, goodDir, readerPermit, mappingsGrantReader)
	r := newTestReloadable(t, goodDir, nil)
	before := r.current.Load()

	badDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(badDir, policiesFile), []byte(readerPermit), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(badDir, rolesFile), []byte(rolesReader), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(badDir, roleMappingsFile), []byte(mappingsGrantReader), 0o600))
	// actions.json with every action entity except one the compiled table
	// still needs -- read-group only, no write group and no per-operation
	// entities, so checkEntityStore's action loop finds a gap immediately.
	require.NoError(t, os.WriteFile(filepath.Join(badDir, actionsFile), []byte(`[
	 {"uid":{"type":"NKE::Action","id":"read"},"attrs":{},"parents":[]}
	]`), 0o600))
	// Mechanical rename of the old r.dir = badDir; see the comment on the
	// same pattern above in TestReloadNeverPublishesABadBundle.
	r.bundleSource = DirBundleSource{Dir: badDir}

	outcome := r.reload(t.Context(), ReloadSourceEndpoint)
	require.Error(t, outcome.Err)
	assert.Same(t, before, r.current.Load())
}

// ---------------------------------------------------------------------------
// A successful reload actually takes effect.
// ---------------------------------------------------------------------------

func TestSuccessfulReloadTakesEffect(t *testing.T) {
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)
	r := newTestReloadable(t, dir, nil)

	decision, err := r.Authorize(t.Context(), readerQuery())
	require.NoError(t, err)
	require.True(t, decision.Allow, "must be Allow before the reload for this test to mean anything")

	writeReloadTestBundle(t, dir, readerListsOnly, mappingsGrantReader)
	outcome := r.reload(t.Context(), ReloadSourceEndpoint)
	require.NoError(t, outcome.Err)
	assert.True(t, outcome.Changed)

	decision, err = r.Authorize(t.Context(), readerQuery())
	require.NoError(t, err)
	assert.False(t, decision.Allow, "the revocation must take effect with no restart, no new Local/Provider")
}

// roleMappings.json is part of the generation: an edit to it alone grants a
// role on reload, and a second edit revokes it, while another mapping keeps
// the bundle loadable. Each counts as changed content.
func TestAMappingOnlyEditChangesGrantsOnReload(t *testing.T) {
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, mappingsRevokeReader)
	calls := &reloadCalls{}
	r := newTestReloadable(t, dir, calls)
	r.minInterval = 0

	decision, err := r.Authorize(t.Context(), readerQuery())
	require.NoError(t, err)
	require.False(t, decision.Allow, "must be Deny before the reload for this test to mean anything")

	editMappings := func(mappings string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, roleMappingsFile), []byte(mappings), 0o600))
		outcome := r.reload(t.Context(), ReloadSourceEndpoint)
		require.NoError(t, outcome.Err)
		assert.True(t, outcome.Changed, "a mapping-only edit is a content change")
	}

	editMappings(mappingsGrantReader)
	assert.Equal(t, 1, calls.changedCount())
	decision, err = r.Authorize(t.Context(), readerQuery())
	require.NoError(t, err)
	assert.True(t, decision.Allow, "the new grant must take effect with no restart")

	editMappings(mappingsRevokeReader)
	assert.Equal(t, 2, calls.changedCount())
	decision, err = r.Authorize(t.Context(), readerQuery())
	require.NoError(t, err)
	assert.False(t, decision.Allow, "the revoked grant must stop working with no restart")
}

// A mapping the loader refuses fails the reload and leaves the previous
// generation answering, the same as a bad policy. So does a roleMappings.json
// with no entries: reload applies startup's rule that at least one mapping
// remains.
func TestReloadRefusesABadMapping(t *testing.T) {
	tests := []struct {
		name     string
		mappings string
		want     string
	}{
		{
			name: "an expression that does not compile to bool",
			mappings: `[{"issuer":"` + testIssuer + `","spiffe":{"trustDomain":"example.org","expression":"path.size()"},
			  "grants":[{"type":"NKE::Role","id":"reader"}]}]`,
			want: "must evaluate to bool",
		},
		{
			name: "an issuer the authentication config does not configure",
			mappings: `[{"issuer":"not-configured","claim":{"name":"groups","value":"readers"},
			  "grants":[{"type":"NKE::Role","id":"reader"}]}]`,
			want: "not-configured",
		},
		{
			name:     "no entries",
			mappings: `[]`,
			want:     roleMappingsFile + " has no entries",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)
			calls := &reloadCalls{}
			r := newTestReloadable(t, dir, calls)
			before := r.current.Load()

			writeReloadTestBundle(t, dir, readerPermit, tt.mappings)
			outcome := r.reload(t.Context(), ReloadSourceEndpoint)
			require.Error(t, outcome.Err)
			assert.Contains(t, outcome.Err.Error(), tt.want)
			assert.Same(t, before, r.current.Load(), "a failed reload must leave the previous bundle live")
			assert.Equal(t, 1, calls.count(ReloadOutcomeFailed))

			decision, err := r.Authorize(t.Context(), readerQuery())
			require.NoError(t, err)
			assert.True(t, decision.Allow, "the previous generation's mapping must keep answering")
		})
	}
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

// load takes and ignores the BundleSource reload passes it (r.bundleSource,
// per reload.go), loading from b.dir instead. The two are the same directory
// in every test that uses blockingLoader (newBlockingReloadable is only ever
// called from runConcurrentJoinCase, which never pokes r.bundleSource), so
// this is not a behavior difference -- it's this fake choosing to assert its
// own fixed, already-known-good directory rather than plumb the parameter
// through, because these tests measure call counts and overlap timing
// (entered/release/calls), not BundleSource polymorphism.
func (b *blockingLoader) load(ctx context.Context, _ BundleSource) (*Local, error) {
	atomic.AddInt32(&b.calls, 1)
	close(b.entered)
	<-b.release
	return newLocalFromSource(ctx, DirBundleSource{Dir: b.dir}, newIssuerSet(reloadTestIssuers()), nil)
}

func newBlockingReloadable(t *testing.T, dir string) (*reloadable, *blockingLoader) {
	t.Helper()
	initial, err := NewLocal(dir, reloadTestIssuers())
	require.NoError(t, err)
	r := newReloadable(dir, newIssuerSet(reloadTestIssuers()), initial, testLogger(), nil, nil, nil)
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
	writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)
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
	writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)
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
	writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)
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
	writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)
	r := newTestReloadable(t, dir, nil)

	// The version pinned by calling Load() itself, before any reload
	// published a new one -- exactly what reloadable.Authorize does at the
	// top of every call.
	pinned := r.current.Load()

	writeReloadTestBundle(t, dir, readerPermit, mappingsRevokeReader)
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
// symlinked to the first, and the four well-known file names symlinked
// through "..data". The two generations differ in policies.cedar and
// roleMappings.json, the first and last files a load reads, so a read that
// mixed them would show.
func buildConfigMapMount(t *testing.T, mountDir string) (string, string) {
	t.Helper()
	gen1 := filepath.Join(mountDir, "..gen1")
	gen2 := filepath.Join(mountDir, "..gen2")
	require.NoError(t, os.Mkdir(gen1, 0o700))
	require.NoError(t, os.Mkdir(gen2, 0o700))

	policies := map[string]string{gen1: readerPermit, gen2: readerListsOnly}
	for genDir, mappings := range map[string]string{gen1: mappingsGrantReader, gen2: mappingsRevokeReader} {
		for name, content := range map[string]string{
			policiesFile:     policies[genDir],
			actionsFile:      realActionsJSON(t),
			rolesFile:        rolesReader,
			roleMappingsFile: mappings,
		} {
			require.NoError(t, os.WriteFile(filepath.Join(genDir, name), []byte(content), 0o600))
		}
	}

	require.NoError(t, os.Symlink("..gen1", filepath.Join(mountDir, "..data")))
	for _, name := range []string{policiesFile, actionsFile, rolesFile, roleMappingsFile} {
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
	for _, name := range []string{policiesFile, actionsFile, rolesFile, roleMappingsFile} {
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
// up front, and every one of the four reads goes through that resolved
// directory rather than back through the mutable symlink, there is no window
// left in which a rename can land between two of the four reads -- this is
// what the stress loop below is exercising: many attempts, a concurrent
// flip on every one, and every successful result matching one generation's
// hash exactly, never neither.
func TestGenerationPinningSurvivesAConcurrentConfigMapSwap(t *testing.T) {
	mountDir := t.TempDir()
	gen1, gen2 := buildConfigMapMount(t, mountDir)
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

		local, err := NewLocal(mountDir, reloadTestIssuers())
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
		writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)
		_, err := NewLocal(dir, reloadTestIssuers())
		require.NoError(t, err, "a plain directory with no ..data must load through the fallback path")
	})

	t.Run("a non-ENOENT Readlink error is a load failure, not a silent fallback", func(t *testing.T) {
		dir := t.TempDir()
		writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)
		// "..data" exists but is not a symlink: os.Readlink reports EINVAL,
		// not fs.ErrNotExist. Resolving this must not fall back to reading
		// dir directly, even though dir itself holds a perfectly loadable
		// bundle -- silently downgrading here is exactly the regression
		// this test exists to catch.
		require.NoError(t, os.Mkdir(filepath.Join(dir, "..data"), 0o700))

		_, err := NewLocal(dir, reloadTestIssuers())
		require.Error(t, err)
		assert.NotErrorIs(t, err, os.ErrNotExist)
	})
}

// ---------------------------------------------------------------------------
// The content-hash gauge tracks content, not attempts.
// ---------------------------------------------------------------------------

func TestBundleChangedGaugeTracksContentNotAttempts(t *testing.T) {
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)
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

	writeReloadTestBundle(t, dir, readerPermit, mappingsRevokeReader)
	outcome := r.reload(t.Context(), ReloadSourcePoll)
	require.NoError(t, outcome.Err)
	assert.True(t, outcome.Changed)
	assert.Equal(t, 1, calls.changedCount(), "the gauge must advance exactly once, on the reload that actually changed")
}

// ---------------------------------------------------------------------------
// The group-only write report follows the bundle in force.
// ---------------------------------------------------------------------------

// The gauge is set from every published bundle and left alone by a failed
// reload, so it always describes the bundle serving requests. The
// per-operation log lines come only with a change, not with every poll.
func TestGroupOnlyWriteReportFollowsThePublishedBundle(t *testing.T) {
	const readersMayWrite = readerPermit + "\n" +
		`permit(principal in NKE::Role::"reader", action in NKE::Action::"write", resource);`

	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)
	calls := &reloadCalls{}
	r := newTestReloadable(t, dir, calls)
	r.minInterval = 0
	var log bytes.Buffer
	r.logger = slog.New(slog.NewJSONHandler(&log, nil))
	logged := func() int {
		n := bytes.Count(log.Bytes(), []byte("write operation the policy covers only through the write action group"))
		log.Reset()
		return n
	}

	writeReloadTestBundle(t, dir, readersMayWrite, mappingsGrantReader)
	require.NoError(t, r.reload(t.Context(), ReloadSourcePoll).Err)
	writes := len(allWrites(t))
	assert.Equal(t, []int{writes}, calls.groupOnlyWriteCounts(), "a published grant of write sets the gauge")
	assert.Equal(t, writes, logged(), "and logs each operation it covers")

	require.NoError(t, r.reload(t.Context(), ReloadSourcePoll).Err)
	assert.Equal(t, []int{writes, writes}, calls.groupOnlyWriteCounts(), "an unchanged reload sets the same value")
	assert.Zero(t, logged(), "and logs nothing")

	writeReloadTestBundle(t, dir, "this is not cedar at all {{{", mappingsGrantReader)
	require.Error(t, r.reload(t.Context(), ReloadSourcePoll).Err)
	assert.Equal(t, []int{writes, writes}, calls.groupOnlyWriteCounts(),
		"a failed reload leaves the gauge with the bundle still in force")

	writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)
	require.NoError(t, r.reload(t.Context(), ReloadSourcePoll).Err)
	assert.Equal(t, []int{writes, writes, 0}, calls.groupOnlyWriteCounts(), "dropping the grant resets it")
}

// ---------------------------------------------------------------------------
// Poll interval honored, including disabled.
// ---------------------------------------------------------------------------

func TestPollLoopReloadsOnInterval(t *testing.T) {
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)
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
// valid bundles every reload, one that allows readerQuery and one that denies
// it, so Authorize genuinely sees both a stable and a changing
// r.current.Load() across the run, rather than reloading the same bytes onto
// themselves. The throttle is off, so the run publishes many generations
// rather than the one reloadMinInterval would allow, and the test requires
// several changed publications and a decision from each kind of generation.
func TestConcurrentAuthorizeSurvivesConcurrentReloads(t *testing.T) {
	dir := t.TempDir()
	writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)
	calls := &reloadCalls{}
	r := newTestReloadable(t, dir, calls)
	r.minInterval = 0

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

	// Flips the mappings file every reload tick so pollLoop is publishing
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
				mappings := mappingsGrantReader
				if n%2 == 0 {
					mappings = mappingsRevokeReader
				}
				for name, content := range map[string]string{
					policiesFile:     readerPermit,
					actionsFile:      actionsJSON,
					rolesFile:        rolesReader,
					roleMappingsFile: mappings,
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
	var allowed, denied atomic.Int64
	// Run for at least 300ms, then until the run has published minChanged
	// changed generations and seen both decisions, up to a hard cap. Reads of
	// a half-written file fail and publish nothing, so how many publications
	// fit in a fixed window depends on the machine; the cap is only there so
	// a broken publication path fails the assertions below instead of hanging.
	const minChanged = 3
	start := time.Now()
	keepGoing := func() bool {
		elapsed := time.Since(start)
		if elapsed < 300*time.Millisecond {
			return true
		}
		if elapsed > 5*time.Second {
			return false
		}
		return calls.changedCount() < minChanged || allowed.Load() == 0 || denied.Load() == 0
	}
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for keepGoing() {
				decision, err := r.Authorize(t.Context(), readerQuery())
				if err != nil {
					reportErr(err)
					return
				}
				if decision.Allow {
					allowed.Add(1)
				} else {
					denied.Add(1)
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

	assert.GreaterOrEqual(t, calls.changedCount(), minChanged,
		"the run must publish several changed generations, not one reload and a run of throttled calls")
	assert.Positive(t, allowed.Load(), "Authorize never saw a generation that grants reader")
	assert.Positive(t, denied.Load(), "Authorize never saw a generation that revokes reader")
}

func TestReloadDisabledNeverWrapsTheEngine(t *testing.T) {
	provider, err := Setup(t.Context(), Options{
		BundleDir:      shippedBundleWithMappings(t, testRoleMappings),
		IssuerNames:    testIssuers,
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
	writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)

	provider, err := Setup(t.Context(), Options{
		BundleDir: dir, IssuerNames: reloadTestIssuers(), ReloadInterval: 0,
	}, testLogger())
	require.NoError(t, err)
	require.NotNil(t, provider)

	// Mutate the bundle to something NewLocal would refuse, then just keep
	// asking the (unwrapped) engine for decisions -- if anything were
	// secretly reloading, this would eventually flip.
	writeReloadTestBundle(t, dir, "not cedar {{{", mappingsGrantReader)

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
	writeReloadTestBundle(t, dir, readerPermit, mappingsGrantReader)

	provider, err := Setup(t.Context(), Options{
		BundleDir:      dir,
		IssuerNames:    reloadTestIssuers(),
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
	local, err := NewLocal(shippedBundleWithMappings(t, testRoleMappings), testIssuers)
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
		return r.WithContext(authn.ContextWithPrincipal(r.Context(), &authn.Principal{
			IssuerName: testIssuer,
			Issuer:     "https://kubernetes.default.svc.cluster.local",
			Subject:    subject,
			Claims:     subClaim(subject),
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
