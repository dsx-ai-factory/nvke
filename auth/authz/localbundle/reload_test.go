// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package localbundle_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"
	"github.com/dsx-ai-factory/nvke/auth/authz/localbundle"
)

// deleteQuery asks to delete a cluster. The valid bundle denies it, and
// allowDeletes allows it.
var deleteQuery = authz.Query{
	Principal: authn.Principal{IssuerName: "keycloak", Subject: "alice"},
	Action:    "deleteCluster",
	Resource:  authz.Resource{Type: "Cluster", ID: "c1", Scope: authz.ScopeGlobal},
}

// allowDeletes returns the valid bundle plus a policy that lets anyone delete
// a cluster.
func allowDeletes() files {
	f := validBundle()
	f["policies.cedar"] += `@id("anyone-may-delete") permit(principal, action == NVKE::Action::"deleteCluster", resource);`
	return f
}

// outcome returns lb's decision on deleteQuery.
func outcome(t *testing.T, lb *localbundle.LocalBundle) authz.Outcome {
	t.Helper()
	d, err := lb.Authorize(t.Context(), deleteQuery)
	require.NoError(t, err)
	return d.Outcome
}

// Reload swaps in a changed bundle, reports whether any file changed, and
// keeps the last good bundle when the new one fails to load.
func TestReload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeBundle(t, dir, validBundle())
	lb, err := localbundle.New(testOptions(dir))
	require.NoError(t, err)
	require.Equal(t, authz.OutcomeDeny, outcome(t, lb))

	reload := func(edit files) bool {
		t.Helper()
		writeBundle(t, dir, edit)
		changed, err := lb.Reload()
		require.NoError(t, err)
		return changed
	}
	assert.False(t, reload(files{}), "no file changed")
	assert.True(t, reload(allowDeletes()), "policies.cedar changed")
	assert.Equal(t, authz.OutcomeAllow, outcome(t, lb), "the new bundle is live")
	assert.True(t, reload(files{"roles.json": `[` + reader + `]` + "\n"}), "changing only roles.json counts as a change")
	assert.True(t, reload(files{"authorization.yaml": validBundle()["authorization.yaml"] + "\n"}),
		"changing only authorization.yaml counts as a change")

	writeBundle(t, dir, files{"roles.json": `[]`})
	changed, err := lb.Reload()
	require.ErrorContains(t, err, "roles.json declares no roles")
	assert.False(t, changed)
	assert.Equal(t, authz.OutcomeAllow, outcome(t, lb), "the last good bundle stays live")
}

// Moving bytes from the end of one file to the start of the next changes the
// bundle.
func TestReloadSeesBytesMoveBetweenFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	f := validBundle()
	f["policies.cedar"] += "\n"
	writeBundle(t, dir, f)
	lb, err := localbundle.New(testOptions(dir))
	require.NoError(t, err)

	f = validBundle()
	f["roles.json"] = "\n" + f["roles.json"]
	writeBundle(t, dir, f)
	changed, err := lb.Reload()
	require.NoError(t, err)
	assert.True(t, changed)
}

// A ConfigMap update points "..data" at a new directory. Reload reads the new
// one.
func TestReloadFollowsTheConfigMapGeneration(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	publish := func(name string, f files) {
		generation := filepath.Join(dir, name)
		require.NoError(t, os.Mkdir(generation, 0o700))
		writeBundle(t, generation, f)
		link := filepath.Join(dir, "..data_tmp")
		require.NoError(t, os.Symlink(name, link))
		require.NoError(t, os.Rename(link, filepath.Join(dir, "..data")))
	}
	publish("..1", validBundle())
	lb, err := localbundle.New(testOptions(dir))
	require.NoError(t, err)
	require.Equal(t, authz.OutcomeDeny, outcome(t, lb))

	publish("..2", allowDeletes())
	changed, err := lb.Reload()
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, authz.OutcomeAllow, outcome(t, lb))
}

// Authorize runs while reloads replace the bundle. Run with -race.
func TestReloadDuringAuthorize(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeBundle(t, dir, validBundle())
	lb, err := localbundle.New(testOptions(dir))
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			for range 50 {
				var err error
				if i == 0 {
					_, err = lb.Reload()
				} else {
					_, err = lb.Authorize(t.Context(), deleteQuery)
				}
				if err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
}

// A zero or nil LocalBundle has no directory to reload from.
func TestAnUnloadedLocalBundleDoesntReload(t *testing.T) {
	t.Parallel()

	var zero localbundle.LocalBundle
	_, err := zero.Reload()
	require.ErrorContains(t, err, "the bundle directory is not set")

	var unset *localbundle.LocalBundle
	_, err = unset.Reload()
	assert.ErrorContains(t, err, "no bundle is loaded")
}
