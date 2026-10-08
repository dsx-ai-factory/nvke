// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

// Contract tests for the nkectl command boundary.
//
// Everything else in this package tests Go functions. These tests build the
// real binary and run it as a process, because the properties they protect only
// exist at that boundary: the exit code the shell sees, which of stdout and
// stderr a byte lands on, and what a signal does to a command in flight. A unit
// test calling RunE observes none of those — it cannot tell os.Exit(1) from
// os.Exit(130), and a stray fmt.Println that bypasses cmd.OutOrStdout() is
// invisible to it.
//
// What these tests assert is the compatibility surface stated in
// cli/docs/compatibility.md. Changing one without the other is the bug this
// pair exists to catch.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// nkectlBin is the binary under test, built once by TestMain.
var nkectlBin string //nolint:gochecknoglobals // TestMain builds it once for every test in this file

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "nkectl-contract")
	if err != nil {
		fmt.Fprintf(os.Stderr, "contract tests: creating temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	nkectlBin = filepath.Join(dir, "nkectl")
	// No -ldflags: the version string is not what is under test here, and
	// leaving it at its "dev" default keeps this independent of the Makefile.
	build := exec.Command("go", "build", "-o", nkectlBin, ".")
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "contract tests: building nkectl: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// result is one invocation of the binary, as a caller sees it.
type result struct {
	exitCode int
	stdout   string
	stderr   string
}

// run invokes nkectl with args and an empty, already-closed stdin, so every
// case also stands as a non-interactive invocation: a command that waits for a
// human hangs here rather than passing.
//
// configHome becomes $XDG_CONFIG_HOME, which is where nkectl looks for its
// config file. Tests pass a temporary directory so that none of them read or
// write the developer's real ~/.config/nkectl.
func run(t *testing.T, configHome string, args ...string) result {
	t.Helper()

	cmd := exec.Command(nkectlBin, args...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+configHome)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("nkectl %s: %v", strings.Join(args, " "), err)
	}
	return result{
		exitCode: cmd.ProcessState.ExitCode(),
		stdout:   stdout.String(),
		stderr:   stderr.String(),
	}
}

// TestExitCodeAndOutputChannel pins the two halves of the contract that are
// invisible to an in-process test: the exit code, and which stream carries the
// message.
//
// The rule is one line long and this table is what keeps it true: results go to
// stdout, diagnostics go to stderr, and the two never mix. A caller doing
// `nkectl get clusters -o json | jq` depends on exactly that.
func TestExitCodeAndOutputChannel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
		// wantExit is the exit status the shell should see.
		wantExit int
		// wantStdout and wantStderr are substrings. An empty string means the
		// stream must carry nothing at all, which is the stronger assertion and
		// the one that catches a diagnostic printed on the wrong channel.
		wantStdout string
		wantStderr string
	}{
		{
			name:       "no arguments prints help and succeeds",
			args:       nil,
			wantExit:   0,
			wantStdout: "NKE control-plane CLI",
			wantStderr: "",
		},
		{
			name:       "help goes to stdout",
			args:       []string{"--help"},
			wantExit:   0,
			wantStdout: "Usage:",
			wantStderr: "",
		},
		{
			name:       "subcommand help goes to stdout",
			args:       []string{"get", "clusters", "--help"},
			wantExit:   0,
			wantStdout: "List tenant clusters",
			wantStderr: "",
		},
		{
			name:       "version succeeds",
			args:       []string{"version"},
			wantExit:   0,
			wantStdout: "nkectl ",
			wantStderr: "",
		},
		{
			name:       "unknown command fails with nothing on stdout",
			args:       []string{"nosuchcommand"},
			wantExit:   1,
			wantStdout: "",
			wantStderr: `unknown command "nosuchcommand"`,
		},
		{
			name:       "unknown flag fails with nothing on stdout",
			args:       []string{"--nosuchflag"},
			wantExit:   1,
			wantStdout: "",
			wantStderr: "unknown flag: --nosuchflag",
		},
		{
			name:       "unknown output format fails with nothing on stdout",
			args:       []string{"version", "--output", "bogus"},
			wantExit:   1,
			wantStdout: "",
			wantStderr: `unknown output format "bogus"`,
		},
		{
			name:       "too many arguments fails",
			args:       []string{"version", "extra"},
			wantExit:   1,
			wantStdout: "",
			wantStderr: "unknown command",
		},
		{
			name:       "a missing required flag fails",
			args:       []string{"create", "cluster"},
			wantExit:   1,
			wantStdout: "",
			wantStderr: `required flag(s) "filename" not set`,
		},
		{
			name:       "an unreadable spec file fails without touching stdout",
			args:       []string{"create", "cluster", "-f", "/nonexistent/spec.yaml"},
			wantExit:   1,
			wantStdout: "",
			wantStderr: "no such file or directory",
		},
		{
			name:       "an empty spec on stdin fails rather than blocking",
			args:       []string{"create", "cluster", "-f", "-"},
			wantExit:   1,
			wantStdout: "",
			wantStderr: "empty document",
		},
		{
			name:       "an API command with no configured site fails",
			args:       []string{"get", "clusters"},
			wantExit:   1,
			wantStdout: "",
			wantStderr: "no site configured",
		},
		{
			name:       "an unknown --site fails",
			args:       []string{"get", "clusters", "--site", "nosuchsite"},
			wantExit:   1,
			wantStdout: "",
			wantStderr: `site "nosuchsite" not found in config`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := run(t, t.TempDir(), tc.args...)

			if got.exitCode != tc.wantExit {
				t.Errorf("exit code = %d, want %d\nstdout: %q\nstderr: %q",
					got.exitCode, tc.wantExit, got.stdout, got.stderr)
			}
			assertStream(t, "stdout", got.stdout, tc.wantStdout)
			assertStream(t, "stderr", got.stderr, tc.wantStderr)
		})
	}
}

// assertStream checks one stream against the table's expectation: empty means
// the stream must be empty, anything else is a required substring.
func assertStream(t *testing.T, name, got, want string) {
	t.Helper()
	if want == "" {
		if got != "" {
			t.Errorf("%s = %q, want nothing on this stream", name, got)
		}
		return
	}
	if !strings.Contains(got, want) {
		t.Errorf("%s = %q, want it to contain %q", name, got, want)
	}
}

// TestMachineReadableOutputIsParseable is the regression coverage for the
// output formats a script consumes. Asserting the bytes parse, and that the
// keys a caller selects on are present, is what a refactor of the printing path
// has to keep true. The assertions are on field names, not on the whole
// document, because adding a field is a compatible change and removing or
// renaming one is not.
func TestMachineReadableOutputIsParseable(t *testing.T) {
	t.Parallel()

	// The version keys, which a release check reads.
	wantVersionKeys := []string{
		"buildVersion", "gitCommit", "gitTreeState", "buildDate",
		"goVersion", "compiler", "platform",
	}

	t.Run("version json", func(t *testing.T) {
		t.Parallel()
		got := run(t, t.TempDir(), "version", "-o", "json")
		requireSuccess(t, got)

		var v map[string]any
		if err := json.Unmarshal([]byte(got.stdout), &v); err != nil {
			t.Fatalf("parsing stdout as JSON: %v\nstdout: %q", err, got.stdout)
		}
		requireKeys(t, v, wantVersionKeys)
	})

	t.Run("version yaml", func(t *testing.T) {
		t.Parallel()
		got := run(t, t.TempDir(), "version", "-o", "yaml")
		requireSuccess(t, got)

		var v map[string]any
		if err := yaml.Unmarshal([]byte(got.stdout), &v); err != nil {
			t.Fatalf("parsing stdout as YAML: %v\nstdout: %q", err, got.stdout)
		}
		requireKeys(t, v, wantVersionKeys)
	})

	t.Run("version short is the bare version and nothing else", func(t *testing.T) {
		t.Parallel()
		got := run(t, t.TempDir(), "version", "-o", "short")
		requireSuccess(t, got)
		// Exact, not a substring: "-o short" exists so that a script can read
		// the version without parsing, and a decoration added to it breaks that.
		if got.stdout != "dev\n" {
			t.Errorf("stdout = %q, want %q", got.stdout, "dev\n")
		}
	})

	t.Run("bootstrap manifest is yaml", func(t *testing.T) {
		t.Parallel()
		got := run(t, t.TempDir(), "get", "bootstrap-manifest")
		requireSuccess(t, got)

		var m map[string]any
		if err := yaml.Unmarshal([]byte(got.stdout), &m); err != nil {
			t.Fatalf("parsing stdout as YAML: %v\nstdout: %q", err, got.stdout)
		}
		if len(m) == 0 {
			t.Error("bootstrap manifest parsed to an empty document")
		}
	})

	t.Run("config view is yaml", func(t *testing.T) {
		t.Parallel()
		got := run(t, t.TempDir(), "config", "view")
		requireSuccess(t, got)

		var cfg map[string]any
		if err := yaml.Unmarshal([]byte(got.stdout), &cfg); err != nil {
			t.Fatalf("parsing stdout as YAML: %v\nstdout: %q", err, got.stdout)
		}
		requireKeys(t, cfg, []string{"sites", "current-site"})
	})
}

// TestGetClustersOutputFormats covers the flagship read journey against a stub
// CWE, which is the only way to reach the printing path from outside the
// process. Both structured formats are exercised, because a caller selecting
// `.[0].name` out of either is depending on the shape asserted here.
func TestGetClustersOutputFormats(t *testing.T) {
	t.Parallel()

	const (
		clusterName = "contract-test"
		clusterID   = "11111111-2222-3333-4444-555555555555"
	)
	srv := stubCWE(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"clusters":[{
			"id": %q,
			"name": %q,
			"controlPlaneKubernetesVersion": "1.31.0",
			"cniPlugin": "calico",
			"cniVersion": "3.28.0",
			"etcdEncrypted": true,
			"createdAt": "2026-01-01T00:00:00Z"
		}]}`, clusterID, clusterName)
	})
	configHome := writeSiteConfig(t, srv.URL)

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		got := run(t, configHome, "get", "clusters", "-o", "json")
		requireSuccess(t, got)

		var clusters []map[string]any
		if err := json.Unmarshal([]byte(got.stdout), &clusters); err != nil {
			t.Fatalf("parsing stdout as JSON: %v\nstdout: %q", err, got.stdout)
		}
		if len(clusters) != 1 {
			t.Fatalf("got %d clusters, want 1: %q", len(clusters), got.stdout)
		}
		requireKeys(t, clusters[0], []string{"id", "name", "controlPlaneKubernetesVersion", "cniPlugin"})
		if clusters[0]["name"] != clusterName {
			t.Errorf("name = %v, want %q", clusters[0]["name"], clusterName)
		}
	})

	t.Run("yaml", func(t *testing.T) {
		t.Parallel()
		got := run(t, configHome, "get", "clusters", "-o", "yaml")
		requireSuccess(t, got)

		var clusters []map[string]any
		if err := yaml.Unmarshal([]byte(got.stdout), &clusters); err != nil {
			t.Fatalf("parsing stdout as YAML: %v\nstdout: %q", err, got.stdout)
		}
		if len(clusters) != 1 || clusters[0]["name"] != clusterName {
			t.Errorf("stdout = %q, want one cluster named %q", got.stdout, clusterName)
		}
	})

	t.Run("table", func(t *testing.T) {
		t.Parallel()
		got := run(t, configHome, "get", "clusters")
		requireSuccess(t, got)
		for _, want := range []string{"NAME", "KUBERNETES VERSION", clusterName} {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("stdout = %q, want it to contain %q", got.stdout, want)
			}
		}
	})

	t.Run("a named cluster that does not exist fails", func(t *testing.T) {
		t.Parallel()
		// The stub answers every list with the same cluster, so ask for one it
		// will not match: the name filter is applied server-side, and the stub
		// ignores it, so this exercises the not-found path only when the stub
		// returns nothing. Use a second stub that does.
		empty := stubCWE(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"clusters":[]}`)
		})
		got := run(t, writeSiteConfig(t, empty.URL), "get", "clusters", "missing")
		if got.exitCode != 1 {
			t.Errorf("exit code = %d, want 1\nstderr: %q", got.exitCode, got.stderr)
		}
		assertStream(t, "stderr", got.stderr, `cluster "missing" not found`)
		if strings.Contains(got.stdout, "Error") {
			t.Errorf("stdout = %q, want no error text on stdout", got.stdout)
		}
	})

	// The one failing command that writes to stdout: a multi-name get prints
	// the matches, kubectl-style, then exits 1 for the names that matched
	// nothing. docs/compatibility.md states the exception.
	t.Run("several names print the matches and fail for the rest", func(t *testing.T) {
		t.Parallel()
		partial := stubCWE(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("name") != clusterName {
				fmt.Fprint(w, `{"clusters":[]}`)
				return
			}
			fmt.Fprintf(w, `{"clusters":[{"id": %q, "name": %q}]}`, clusterID, clusterName)
		})
		got := run(t, writeSiteConfig(t, partial.URL), "get", "clusters", clusterName, "missing", "-o", "json")
		if got.exitCode != 1 {
			t.Errorf("exit code = %d, want 1\nstderr: %q", got.exitCode, got.stderr)
		}
		assertStream(t, "stderr", got.stderr, `cluster "missing" not found`)

		var clusters []map[string]any
		if err := json.Unmarshal([]byte(got.stdout), &clusters); err != nil {
			t.Fatalf("parsing stdout as JSON: %v\nstdout: %q", err, got.stdout)
		}
		if len(clusters) != 1 || clusters[0]["name"] != clusterName {
			t.Errorf("stdout = %q, want only the cluster that matched", got.stdout)
		}
	})
}

// TestAPIErrorsReachStderrWithANonZeroExit proves a failing site controller
// surfaces as a failure, rather than as an empty success a script would read as
// "no clusters".
func TestAPIErrorsReachStderrWithANonZeroExit(t *testing.T) {
	t.Parallel()

	srv := stubCWE(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"database unavailable"}`)
	})

	got := run(t, writeSiteConfig(t, srv.URL), "get", "clusters", "-o", "json")
	if got.exitCode != 1 {
		t.Errorf("exit code = %d, want 1", got.exitCode)
	}
	assertStream(t, "stdout", got.stdout, "")
	assertStream(t, "stderr", got.stderr, "database unavailable")
}

// TestSignalCancelsAnInFlightCommand covers the cancellation half of the
// contract: a command interrupted while it is waiting on the network stops, and
// reports 128 plus the signal number rather than the 1 it would use for a
// genuine failure. Without the signal-aware root context this test hangs until
// the stub is torn down.
func TestSignalCancelsAnInFlightCommand(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		sig      syscall.Signal
		wantExit int
	}{
		{"SIGINT", syscall.SIGINT, 130},
		{"SIGTERM", syscall.SIGTERM, 143},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reached := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			srv := stubCWE(t, func(_ http.ResponseWriter, r *http.Request) {
				once.Do(func() { close(reached) })
				// Hold the request open until the command is interrupted, or until
				// the test finishes, whichever comes first.
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})

			cmd := exec.Command(nkectlBin, "get", "clusters")
			cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+writeSiteConfig(t, srv.URL))
			var stdout, stderr strings.Builder
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("starting nkectl: %v", err)
			}
			t.Cleanup(func() { close(release) })

			select {
			case <-reached:
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("nkectl never reached the stub site controller")
			}

			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatalf("sending %s: %v", tc.name, err)
			}

			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("nkectl did not exit within 30s of %s", tc.name)
			}

			if got := cmd.ProcessState.ExitCode(); got != tc.wantExit {
				t.Errorf("exit code = %d, want %d\nstdout: %q\nstderr: %q",
					got, tc.wantExit, stdout.String(), stderr.String())
			}
			if stdout.String() != "" {
				t.Errorf("stdout = %q, want nothing: a cancelled command produces no result", stdout.String())
			}
		})
	}
}

// TestASecondSignalKillsACommandThatIgnoresTheFirst guards the hazard the
// signal-aware context introduces. Installing a handler takes SIGINT away from
// the kernel's default disposition, so a command blocked somewhere that does
// not watch the context - here, a read that is already under way - would stop
// answering to Ctrl-C altogether. main restores the default as soon as the
// first signal lands, which is what this asserts.
//
// The spec comes from a FIFO rather than from a pipe on stdin so that the test
// knows when to interrupt without guessing. Opening a FIFO for writing blocks
// until a reader opens the other end, so the open below returns only once
// nkectl has reached its spec read, which is long past the point where main
// installed the handler. A sleep here would instead race process startup, and
// lose on a cold or loaded machine.
func TestASecondSignalKillsACommandThatIgnoresTheFirst(t *testing.T) {
	t.Parallel()

	fifo := filepath.Join(t.TempDir(), "spec.yaml")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("creating fifo: %v", err)
	}

	cmd := exec.Command(nkectlBin, "create", "cluster", "-f", fifo)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+t.TempDir())
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting nkectl: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Blocks until nkectl opens the FIFO to read the spec. Held open for the
	// rest of the test and never written to, so the read never completes and
	// never sees EOF.
	writer, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("opening the fifo for writing: %v", err)
	}
	defer writer.Close()

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("sending the first SIGINT: %v", err)
	}

	select {
	case <-done:
		t.Fatal("nkectl exited on the first SIGINT while blocked reading its spec; " +
			"the first signal should cancel the context and only the second should kill it")
	case <-time.After(time.Second):
	}

	// Repeat rather than send exactly one more: the default disposition is
	// restored by a goroutine, and this test should not depend on winning a
	// race with it.
	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			if cmd.ProcessState.ExitCode() != -1 {
				t.Errorf("exit code = %d, want the process to have been killed by a signal",
					cmd.ProcessState.ExitCode())
			}
			return
		case <-deadline:
			_ = cmd.Process.Kill()
			t.Fatal("nkectl never died despite repeated SIGINT")
		case <-tick.C:
			_ = cmd.Process.Signal(syscall.SIGINT)
		}
	}
}

// stubCWE starts a site controller stand-in that answers the cluster list and
// nothing else, so an unexpected call fails loudly rather than being absorbed.
func stubCWE(t *testing.T, list http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/clusters" {
			http.Error(w, "unexpected request: "+r.URL.Path, http.StatusNotFound)
			return
		}
		list(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeSiteConfig returns an XDG_CONFIG_HOME holding a config file that points
// nkectl at url with no kubeconfig, which is how the CLI behaves against a site
// whose API authentication is off. The file is written directly rather than
// through "config set-site" so that these tests do not depend on that command;
// the file's shape is itself part of the compatibility surface.
func writeSiteConfig(t *testing.T, url string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "nkectl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating config dir: %v", err)
	}
	cfg := fmt.Sprintf("sites:\n  default:\n    url: %s\n    kubeconfig: \"\"\ncurrent-site: default\n", url)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return home
}

func requireSuccess(t *testing.T, got result) {
	t.Helper()
	if got.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %q", got.exitCode, got.stderr)
	}
	if got.stderr != "" {
		t.Errorf("stderr = %q, want nothing on a successful run", got.stderr)
	}
}

func requireKeys(t *testing.T, doc map[string]any, keys []string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := doc[k]; !ok {
			t.Errorf("key %q is missing; it is part of the documented output contract", k)
		}
	}
}
