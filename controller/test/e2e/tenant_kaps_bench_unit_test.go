// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseKapsBenchRunSpecs(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected []string
	}{
		{name: "one run spec", value: "smoke/smoke.yaml", expected: []string{"smoke/smoke.yaml"}},
		{name: "two run specs in order", value: "smoke/smoke.yaml,netperf/hostnet-stream.yaml", expected: []string{"smoke/smoke.yaml", "netperf/hostnet-stream.yaml"}},
		{name: "an empty value", value: ""},
		{name: "an empty entry", value: "smoke/smoke.yaml,"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			specs, err := parseKapsBenchRunSpecs(tc.value)
			if tc.expected == nil {
				t.Log("Testing: an empty entry is refused, so kaps-bench is never run with an empty run spec")
				require.ErrorContains(t, err, "empty")
				require.Nil(t, specs)
				return
			}
			t.Log("Testing: the run specs come back in the order given")
			require.NoError(t, err)
			require.Equal(t, tc.expected, specs)
		})
	}
}

func TestRequireKapsBenchTools(t *testing.T) {
	t.Log("Doing: complete kaps-bench folders, and copies that each lack one piece")
	folder := func() string {
		dir := t.TempDir()
		for file, mode := range map[string]os.FileMode{
			"kaps-bench":                 0o700,
			"bin/helm":                   0o700,
			"run-specs/smoke/smoke.yaml": 0o600,
		} {
			require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, file), nil, mode))
		}
		return dir
	}
	without := func(file string) string {
		dir := folder()
		require.NoError(t, os.Remove(filepath.Join(dir, file)))
		return dir
	}
	notExecutable := func(file string) string {
		dir := folder()
		require.NoError(t, os.Chmod(filepath.Join(dir, file), 0o600))
		return dir
	}
	dir := folder()
	file := filepath.Join(dir, "kaps-bench.yaml")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	wd, err := os.Getwd()
	require.NoError(t, err)
	relative, err := filepath.Rel(wd, dir)
	require.NoError(t, err)
	specs := []string{"smoke/smoke.yaml"}
	tests := []struct {
		name     string
		specs    []string
		dir      string
		expected []string
	}{
		{name: "no run specs needs nothing", dir: ""},
		{name: "everything is here", specs: specs, dir: dir},
		{name: "no folder named", specs: specs, dir: "", expected: []string{"E2E_KAPS_BENCH_DIR"}},
		{name: "a folder that does not exist", specs: specs, dir: filepath.Join(dir, "absent"), expected: []string{"-e2e.kaps-bench-dir"}},
		{name: "a file in place of the folder", specs: specs, dir: file, expected: []string{"is not a folder"}},
		{name: "a relative folder path", specs: specs, dir: relative, expected: []string{"is not an absolute path"}},
		{name: "no kaps-bench in the folder", specs: specs, dir: without("kaps-bench"), expected: []string{"has no kaps-bench to run"}},
		{name: "a kaps-bench without the execute bit", specs: specs, dir: notExecutable("kaps-bench"), expected: []string{"has no kaps-bench to run", "permission denied"}},
		{name: "no helm in the folder's bin", specs: specs, dir: without("bin/helm"), expected: []string{"has no bin/helm to run"}},
		{name: "a helm without the execute bit", specs: specs, dir: notExecutable("bin/helm"), expected: []string{"has no bin/helm to run", "permission denied"}},
		{name: "a run spec the folder lacks", specs: []string{"smoke/smoke.yaml", "smoke/smok.yaml"}, dir: dir, expected: []string{"kaps-bench has no run spec smoke/smok.yaml"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Log("Testing: each missing piece is named in the one error")
			err := requireKapsBenchTools(tc.specs, tc.dir)
			if len(tc.expected) == 0 {
				require.NoError(t, err)
				return
			}
			for _, want := range tc.expected {
				require.ErrorContains(t, err, want)
			}
			if tc.name != "a run spec the folder lacks" {
				t.Log("Testing: one cause gives one error, so a missing folder names no run spec")
				require.NotContains(t, err.Error(), "has no run spec")
			}
		})
	}

	t.Run("no helm on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		t.Log("Testing: kaps-bench brings its own helm, so the runner's PATH needs none")
		require.NoError(t, requireKapsBenchTools(specs, dir))
	})
}

func TestKapsBenchRunArgs(t *testing.T) {
	t.Log("Testing: kaps-bench runs the spec against the tenant and publishes locally")
	require.Equal(t, []string{
		"run", "smoke/smoke-prometheus.yaml",
		"--kubeconfig", "/w/kubeconfig",
		"--cluster-context", "/w/cluster-context.yaml",
		"--output-dir", "/w/output",
		"--local-publisher",
	}, kapsBenchRunArgs("smoke/smoke-prometheus.yaml", "/w/kubeconfig", "/w/cluster-context.yaml", "/w/output"))
}

func TestKapsBenchClusterContext(t *testing.T) {
	t.Log("Testing: the cluster context names the tenant and the UID of its kube-system namespace")
	document, err := kapsBenchClusterContext("az41-ganesh-test", "209b2b00-e17b-4d05-8073-dccbed7ef5f0")
	require.NoError(t, err)
	require.Equal(t, "cluster:\n  name: az41-ganesh-test\n  uuid: 209b2b00-e17b-4d05-8073-dccbed7ef5f0\n", string(document))
}

func TestKeepKapsBenchResults(t *testing.T) {
	tests := []struct {
		name       string
		work       string
		specFailed bool
		expected   string
	}{
		{name: "the run spec passed, so lost results fail it", work: t.TempDir(), expected: "archiving"},
		{name: "the run spec failed, so its failure stays the verdict", work: t.TempDir(), specFailed: true},
		{name: "no setup ran, so there is nothing to archive", work: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "kaps-bench-results.tar.gz")
			t.Log("Testing: an output dir kaps-bench never wrote leaves no archive behind")
			err := keepKapsBenchResults(tc.work, archive, tc.specFailed)
			if tc.expected == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.expected)
			}
			require.NoFileExists(t, archive)
		})
	}
}

func TestKapsBenchSetupRunsOnce(t *testing.T) {
	tests := []struct {
		name     string
		step     func() (string, error)
		expected string
	}{
		{name: "a setup that succeeds", step: func() (string, error) { return "/tmp/kaps-bench-1", nil }},
		{name: "a setup whose cluster context cannot be read", step: func() (string, error) {
			return "", errors.New(`namespaces "kube-system" is forbidden`)
		}, expected: `namespaces "kube-system" is forbidden`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var setup kapsBenchSetup
			calls := 0
			step := func() (string, error) { calls++; return tc.step() }
			t.Log("Testing: no earlier failure and no setup yet means the run spec runs")
			require.Empty(t, setup.skipReason(false))

			first, firstErr := setup.ensure(step)
			second, secondErr := setup.ensure(step)
			t.Log("Testing: two run specs set up once and see the same dir and error")
			require.Equal(t, 1, calls)
			require.Equal(t, first, second)
			require.Equal(t, firstErr, secondErr)
			if tc.expected == "" {
				require.NoError(t, firstErr)
				require.Equal(t, "/tmp/kaps-bench-1", first)
				require.Empty(t, setup.skipReason(true), "a failed run spec does not skip the next one")
				return
			}
			require.ErrorContains(t, firstErr, tc.expected)
			require.Empty(t, first)
			t.Log("Testing: later run specs skip, naming the setup failure")
			require.Contains(t, setup.skipReason(false), "kaps-bench was not set up for the tenant: ")
		})
	}

	t.Run("an earlier tenant failure skips before any setup", func(t *testing.T) {
		var setup kapsBenchSetup
		require.Equal(t, "an earlier check on this tenant failed", setup.skipReason(true))
	})

	t.Run("a setup that panics", func(t *testing.T) {
		var setup kapsBenchSetup
		require.Panics(t, func() { _, _ = setup.ensure(func() (string, error) { panic("relay closed") }) })
		t.Log("Testing: the stored error stays, so no run spec reads a half-written work dir")
		work, err := setup.ensure(func() (string, error) { t.Fatal("set up twice"); return "", nil })
		require.Empty(t, work)
		require.ErrorContains(t, err, "the kaps-bench setup did not finish")
	})
}

func TestKapsBenchSpecTimeoutAndToken(t *testing.T) {
	tests := []struct {
		name     string
		index    int
		expected time.Duration
		run      time.Duration
	}{
		{name: "the first run spec also gives kaps-bench time to install what it needs", index: 0, expected: 45 * time.Minute, run: 39*time.Minute + 50*time.Second},
		{name: "a later run spec", index: 1, expected: 30 * time.Minute, run: 24*time.Minute + 50*time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Log("Testing: the token outlasts the run spec's own limit by five minutes")
			require.Equal(t, tc.expected, kapsBenchSpecTimeout(tc.index))
			require.Equal(t, tc.expected+5*time.Minute, kapsBenchTokenLifetime(tc.index))

			t.Log("Testing: listing the groups, the stop grace and the pipe wait all end inside the limit")
			tools := tenantToolRun{stopGrace: kapsBenchStopGrace}
			stopped := kapsBenchRunTimeout(tc.index) + aicrKillGrace + tools.waitDelay()
			require.Equal(t, tc.run, kapsBenchRunTimeout(tc.index))
			require.LessOrEqual(t, stopped, kapsBenchSpecTimeout(tc.index))
			require.Less(t, stopped, kapsBenchTokenLifetime(tc.index))
		})
	}
}

func TestArchiveKapsBenchResults(t *testing.T) {
	t.Log("Doing: an output dir laid out the way kaps-bench writes one")
	output := t.TempDir()
	run := filepath.Join(output, "01a1c2e3-1f76-726d-8550-5149212bdcb2")
	require.NoError(t, os.MkdirAll(run, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(run, "preflight.json"), []byte(`{"passed":true}`), 0o600))
	archive := filepath.Join(t.TempDir(), "kaps-bench-results.tar.gz")

	t.Log("Testing: the archive holds every file under the output dir by its relative path")
	require.NoError(t, archiveKapsBenchResults(output, archive))
	f, err := os.Open(archive)
	require.NoError(t, err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeReg {
			content, err := io.ReadAll(tr)
			require.NoError(t, err)
			files[hdr.Name] = string(content)
		}
	}
	require.Equal(t, map[string]string{"01a1c2e3-1f76-726d-8550-5149212bdcb2/preflight.json": `{"passed":true}`}, files)

	t.Log("Testing: a failed rewrite keeps the archive already there and leaves no temp file")
	before, readErr := os.ReadFile(archive)
	require.NoError(t, readErr)
	require.ErrorContains(t, archiveKapsBenchResults(filepath.Join(output, "absent"), archive), "archiving")
	after, readErr := os.ReadFile(archive)
	require.NoError(t, readErr)
	require.Equal(t, before, after)
	entries, readErr := os.ReadDir(filepath.Dir(archive))
	require.NoError(t, readErr)
	require.Len(t, entries, 1)
}
