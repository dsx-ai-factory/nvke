// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	bootstrapengine "github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/engine"
	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
)

// phasedRun is what a bootstrap run logged and traced.
type phasedRun struct {
	err     error
	records []map[string]any
	raw     string
	spans   []sdktrace.ReadOnlySpan
	traceID string
	rootID  string
}

// runPhased runs "bootstrap site" inside a telemetry run whose logs and spans
// are captured. The tests that call it replace the default logger, so they do
// not run in parallel.
func runPhased(t *testing.T, opts *bootstrapSiteOptions) phasedRun {
	t.Helper()
	return runPhasedWith(context.Background(), t, opts)
}

// runPhasedWith is runPhased for a run that starts from base, so a test can
// cancel the command's context.
func runPhasedWith(base context.Context, t *testing.T, opts *bootstrapSiteOptions) phasedRun {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	run := telemetry.NewRun("test", telemetry.WithSpanProcessor(rec))
	ctx := run.Start(base, "nvkectl bootstrap site")

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(run.LogHandler(slog.NewJSONHandler(&buf, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })

	err := runBootstrapSite(ctx, io.Discard, opts)
	run.Finish(err)

	got := phasedRun{err: err, raw: buf.String(), spans: rec.Ended(), traceID: run.TraceID(), rootID: run.SpanContext().SpanID().String()}
	dec := json.NewDecoder(strings.NewReader(got.raw))
	for dec.More() {
		var record map[string]any
		require.NoError(t, dec.Decode(&record))
		got.records = append(got.records, record)
	}
	return got
}

// phaseLines returns the "phase finished" records as phase=outcome strings.
func (r phasedRun) phaseLines() []string {
	var lines []string
	for _, record := range r.records {
		if record["msg"] == "phase finished" {
			lines = append(lines, record["phase"].(string)+"="+record["outcome"].(string))
		}
	}
	return lines
}

// phaseSpan returns the span of the phase called name.
func (r phasedRun) phaseSpan(name string) sdktrace.ReadOnlySpan {
	for _, span := range r.spans {
		if span.Name() == "phase."+name {
			return span
		}
	}
	return nil
}

// happyPathOptions is a successful run, as in TestRunBootstrapSiteCreatesTheCluster.
func happyPathOptions(t *testing.T) *bootstrapSiteOptions {
	t.Helper()
	return &bootstrapSiteOptions{
		siteSpec:            writeSiteSpec(t, validSiteSpec),
		bootstrapRunner:     fakeBootstrapRunner{},
		cweRunner:           newFakeCWERunner(t),
		kubeconfigOut:       filepath.Join(t.TempDir(), "nvke-site.kubeconfig"),
		lookPath:            runtimeOnPath,
		dialContext:         refuseDial,
		untaintControlPlane: skipUntaint,
		dumpSpireEntries:    func(context.Context, string, string) (int, error) { return 0, nil },
		encryptionKeyOut:    filepath.Join(t.TempDir(), "key.json"),
	}
}

func TestRunBootstrapSiteTimesEveryPhase(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	got := runPhased(t, happyPathOptions(t))
	require.NoError(t, got.err)

	assert.Equal(t, []string{
		"preflight=succeeded", "bootstrap_cluster=succeeded", "engine_deploy=succeeded", "site_cluster=succeeded", "finish=succeeded",
	}, got.phaseLines())
	for _, record := range got.records {
		if record["msg"] == "phase finished" {
			assert.GreaterOrEqual(t, record["duration_ms"], float64(0), record["phase"])
		}
	}
	for _, name := range []string{phasePreflight, phaseBootstrapCluster, phaseEngineDeploy, phaseSiteCluster, phaseFinish} {
		span := got.phaseSpan(name)
		require.NotNil(t, span, name)
		assert.Equal(t, got.rootID, span.Parent().SpanID().String(), "%s is a child of the run's root span", name)
		assert.Equal(t, codes.Unset, span.Status().Code, name)
	}
}

// TestRunBootstrapSiteLogsOperationClusterAndPhase checks the fields on records
// made by the component loggers, which are built before any phase starts and
// are given no context.
func TestRunBootstrapSiteLogsOperationClusterAndPhase(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	got := runPhased(t, happyPathOptions(t))
	require.NoError(t, got.err)

	phaseSpanIDs := map[string]string{}
	for _, span := range got.spans {
		phaseSpanIDs[strings.TrimPrefix(span.Name(), "phase.")] = span.SpanContext().SpanID().String()
	}
	seenComponents := map[string]map[string]bool{}
	specLoaded := false
	for _, record := range got.records {
		assert.Equal(t, got.traceID, record["trace_id"], "%v", record["msg"])
		assert.Equal(t, operationBootstrapSite, record["operation"], "%v", record["msg"])
		if record["msg"] == "loaded site spec" {
			specLoaded = true
		}
		if specLoaded {
			assert.Equal(t, "nvke-site", record["cluster"], "%v", record["msg"])
		}
		if component, ok := record["component"].(string); ok && record["msg"] != "phase finished" {
			phase, _ := record["phase"].(string)
			if seenComponents[component] == nil {
				seenComponents[component] = map[string]bool{}
			}
			seenComponents[component][phase] = true
			assert.NotEmpty(t, phase, "%v: a record made while a phase runs names it", record["msg"])
			assert.Equal(t, phaseSpanIDs[phase], record["span_id"], "%v: the span ID is the phase span's", record["msg"])
		}
	}
	assert.Contains(t, seenComponents, "bootstrapengine", "the engine's logger is covered")
	assert.True(t, seenComponents["bootstrapengine"][phaseBootstrapCluster], "creating the Kind cluster")
	assert.True(t, seenComponents["bootstrapengine"][phaseFinish], "deleting it")
	assert.True(t, seenComponents["controllerdeploy"][phaseEngineDeploy])
}

// TestRunBootstrapSiteLogsNoRecordWithADuplicateKey guards the rename of the
// bootstrap Kind cluster's field, which would otherwise collide with cluster,
// and for the fields the handler adds.
func TestRunBootstrapSiteLogsNoRecordWithADuplicateKey(t *testing.T) {
	setBootstrapEnv(t)
	startFakeCWEAPI(t, true)

	got := runPhased(t, happyPathOptions(t))
	require.NoError(t, got.err)

	for _, line := range strings.Split(strings.TrimSpace(got.raw), "\n") {
		seen := map[string]bool{}
		dec := json.NewDecoder(strings.NewReader(line))
		_, err := dec.Token() // the opening brace
		require.NoError(t, err)
		for dec.More() {
			key, err := dec.Token()
			require.NoError(t, err)
			// "component" is repeated by the component loggers, which add theirs to
			// bootstrap-site's; that is not what this guards.
			if key != "component" {
				assert.False(t, seen[key.(string)], "duplicate key %q in %s", key, line)
			}
			seen[key.(string)] = true
			var value json.RawMessage
			require.NoError(t, dec.Decode(&value))
		}
	}
}

func TestRunBootstrapSiteDryRunTimesOnlyPreflight(t *testing.T) {
	got := runPhased(t, &bootstrapSiteOptions{siteSpec: writeSiteSpec(t, validSiteSpec), dryRun: true})

	require.NoError(t, got.err)
	assert.Equal(t, []string{"preflight=succeeded"}, got.phaseLines())
}

func TestRunBootstrapSiteReportsTheFailingPhase(t *testing.T) {
	setBootstrapEnv(t)
	const invalidSpec = `
clusterName: nvke-site
cluster:
  nodes:
    - name: node1
      ip: not-an-ip
  k8sVersion: "1.32.0"
  cniName: ovn-kubernetes
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
`
	tests := []struct {
		name      string
		failed    string
		wantLines []string
		setup     func(t *testing.T) *bootstrapSiteOptions
	}{
		{
			name:      "preflight",
			failed:    phasePreflight,
			wantLines: []string{"preflight=failed"},
			setup: func(t *testing.T) *bootstrapSiteOptions {
				return &bootstrapSiteOptions{siteSpec: writeSiteSpec(t, invalidSpec)}
			},
		},
		{
			name:      "bootstrap_cluster",
			failed:    phaseBootstrapCluster,
			wantLines: []string{"preflight=succeeded", "bootstrap_cluster=failed"},
			setup: func(t *testing.T) *bootstrapSiteOptions {
				opts := happyPathOptions(t)
				opts.bootstrapRunner = runnerFunc(func(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
					if name == "kind" && len(args) > 1 && args[0] == "create" && args[1] == "cluster" {
						return "", errors.New("kind create failed")
					}
					return fakeBootstrapRunner{}.Run(ctx, env, name, args...)
				})
				return opts
			},
		},
		{
			name:      "engine_deploy",
			failed:    phaseEngineDeploy,
			wantLines: []string{"preflight=succeeded", "bootstrap_cluster=succeeded", "engine_deploy=failed", "finish=succeeded"},
			setup: func(t *testing.T) *bootstrapSiteOptions {
				// Short, so the test does not wait out the default for a controller that never reports healthy.
				t.Setenv("NVKECTL_NVKE_CONTROLLER_READY_TIMEOUT", "1s")
				opts := happyPathOptions(t)
				opts.cweRunner = unhealthyCWERunner{}
				opts.diagnosticsDir = filepath.Join(t.TempDir(), "cluster-diagnostics")
				return opts
			},
		},
		{
			name:   "site_cluster",
			failed: phaseSiteCluster,
			wantLines: []string{
				"preflight=succeeded", "bootstrap_cluster=succeeded", "engine_deploy=succeeded", "site_cluster=failed", "finish=succeeded",
			},
			setup: func(t *testing.T) *bootstrapSiteOptions {
				startFakeCWEAPI(t, false)
				opts := happyPathOptions(t)
				opts.clusterTimeout = 5 * time.Millisecond
				return opts
			},
		},
		{
			name:   "finish",
			failed: phaseFinish,
			wantLines: []string{
				"preflight=succeeded", "bootstrap_cluster=succeeded", "engine_deploy=succeeded", "site_cluster=succeeded", "finish=failed",
			},
			setup: func(t *testing.T) *bootstrapSiteOptions {
				startFakeCWEAPI(t, true)
				opts := happyPathOptions(t)
				opts.untaintControlPlane = func(context.Context, string, *slog.Logger) ([]string, error) {
					return nil, errors.New("nodes is forbidden")
				}
				return opts
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runPhased(t, tt.setup(t))

			require.Error(t, got.err)
			assert.Equal(t, tt.wantLines, got.phaseLines())
			span := got.phaseSpan(tt.failed)
			require.NotNil(t, span)
			assert.Equal(t, codes.Error, span.Status().Code)
			for _, other := range []string{phasePreflight, phaseBootstrapCluster, phaseEngineDeploy, phaseSiteCluster, phaseFinish} {
				if other != tt.failed && got.phaseSpan(other) != nil {
					assert.NotEqual(t, codes.Error, got.phaseSpan(other).Status().Code, "%s did not fail", other)
				}
			}
		})
	}
}

func TestUTCTimeReplacesOnlyTheTopLevelTime(t *testing.T) {
	local := time.Date(2026, 10, 6, 4, 32, 13, 0, time.FixedZone("PDT", -7*60*60))
	replace := utcTime(func(_ []string, attr slog.Attr) slog.Attr { return attr })

	got := replace(nil, slog.Time(slog.TimeKey, local))
	assert.True(t, got.Value.Time().Equal(local))
	assert.Equal(t, time.UTC, got.Value.Time().Location())

	nested := replace([]string{"group"}, slog.Time(slog.TimeKey, local))
	assert.Equal(t, local.Location(), nested.Value.Time().Location(), "a time inside a group is left alone")
	other := replace(nil, slog.Time("started", local))
	assert.Equal(t, local.Location(), other.Value.Time().Location(), "another attribute is left alone")
}

func TestSetupLoggingWritesUTCTimeOnlyAsJSON(t *testing.T) {
	// Not parallel: setupLogging replaces the default logger and writes to os.Stderr.
	local := time.Date(2026, 10, 6, 4, 32, 13, 0, time.FixedZone("PDT", -7*60*60))
	tests := []struct {
		name     string
		jsonLogs bool
		want     string
	}{
		{name: "json", jsonLogs: true, want: `"time":"2026-10-06T11:32:13Z"`},
		{name: "text", jsonLogs: false, want: `time=2026-10-06T04:32:13.000-07:00`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
			require.NoError(t, err)
			previousStderr, previousLogger := os.Stderr, slog.Default()
			os.Stderr = file
			t.Cleanup(func() { os.Stderr = previousStderr; slog.SetDefault(previousLogger) })

			require.NoError(t, setupLogging(false, tt.jsonLogs, nil))
			record := slog.NewRecord(local, slog.LevelInfo, "x", 0)
			require.NoError(t, slog.Default().Handler().Handle(context.Background(), record))

			written, err := os.ReadFile(file.Name())
			require.NoError(t, err)
			assert.Contains(t, string(written), tt.want)
		})
	}
}

// cancelingCWERunner cancels the command's context on its first call and fails
// it, as a SIGINT during the controller deploy does.
type cancelingCWERunner struct {
	fakeCWERunner
	cancel context.CancelFunc
}

func (r cancelingCWERunner) Run(ctx context.Context, _ string, _ ...string) (string, error) {
	r.cancel()
	return "", ctx.Err()
}

// TestRunBootstrapSiteEndsEveryPhaseWhenInterrupted checks that a cancelled
// context does not leave a phase open: the interrupted phase is reported as
// failed, and the cleanup still runs, is timed as finish and tears Kind down.
func TestRunBootstrapSiteEndsEveryPhaseWhenInterrupted(t *testing.T) {
	setBootstrapEnv(t)
	base, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runner := &recordingRunner{next: fakeBootstrapRunner{}}
	opts := happyPathOptions(t)
	opts.bootstrapRunner = runner
	opts.cweRunner = cancelingCWERunner{fakeCWERunner: newFakeCWERunner(t), cancel: cancel}
	opts.diagnosticsDir = filepath.Join(t.TempDir(), "cluster-diagnostics")

	got := runPhasedWith(base, t, opts)

	require.Error(t, got.err)
	assert.Equal(t, []string{
		"preflight=succeeded", "bootstrap_cluster=succeeded", "engine_deploy=failed", "finish=succeeded",
	}, got.phaseLines())
	assert.True(t, runner.sawArgs("delete", "cluster"), "the cleanup runs on a cancelled context; calls: %v", runner.calls)
	require.NotNil(t, got.phaseSpan(phaseFinish), "the finish span ended")
	assert.Equal(t, codes.Unset, got.phaseSpan(phaseFinish).Status().Code)
	assert.Equal(t, codes.Error, got.phaseSpan(phaseEngineDeploy).Status().Code)
}

// failingKindDelete makes the teardown of the bootstrap Kind cluster fail.
func failingKindDelete(next bootstrapengine.CommandRunner) bootstrapengine.CommandRunner {
	return runnerFunc(func(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
		if name == "kind" && len(args) > 0 && args[0] == "delete" {
			return "", errors.New("kind delete failed")
		}
		return next.Run(ctx, env, name, args...)
	})
}

func TestRunBootstrapSiteFailsFinishWhenTheTeardownFails(t *testing.T) {
	setBootstrapEnv(t)

	t.Run("after a successful run", func(t *testing.T) {
		startFakeCWEAPI(t, true)
		opts := happyPathOptions(t)
		opts.bootstrapRunner = failingKindDelete(opts.bootstrapRunner)

		got := runPhased(t, opts)

		require.NoError(t, got.err, "a teardown failure is logged, not returned")
		assert.Equal(t, []string{
			"preflight=succeeded", "bootstrap_cluster=succeeded", "engine_deploy=succeeded", "site_cluster=succeeded", "finish=failed",
		}, got.phaseLines())
		span := got.phaseSpan(phaseFinish)
		require.NotNil(t, span)
		assert.Equal(t, codes.Error, span.Status().Code)
		assert.Contains(t, span.Status().Description, "kind delete failed")
	})

	t.Run("after an earlier phase failed", func(t *testing.T) {
		t.Setenv("NVKECTL_NVKE_CONTROLLER_READY_TIMEOUT", "1s")
		opts := happyPathOptions(t)
		opts.cweRunner = unhealthyCWERunner{}
		opts.diagnosticsDir = filepath.Join(t.TempDir(), "cluster-diagnostics")
		opts.bootstrapRunner = failingKindDelete(opts.bootstrapRunner)

		got := runPhased(t, opts)

		require.Error(t, got.err)
		assert.Equal(t, []string{
			"preflight=succeeded", "bootstrap_cluster=succeeded", "engine_deploy=failed", "finish=failed",
		}, got.phaseLines())
		finish := got.phaseSpan(phaseFinish)
		require.NotNil(t, finish)
		assert.Contains(t, finish.Status().Description, "kind delete failed")
		assert.NotContains(t, finish.Status().Description, got.err.Error(), "the failed phase's error is not repeated on finish")
	})
}
