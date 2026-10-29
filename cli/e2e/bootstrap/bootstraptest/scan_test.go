// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	pgPassword = "pg-generated-password-0123456789"
	etcdText   = "an etcd key that is text, 32 ch"
)

// scanFixture is a run's files and a bootstrap cluster the test controls.
type scanFixture struct {
	scan      *SecretScan
	dir       string
	log       string
	diag      string
	key       string
	reachable atomic.Bool
	objects   []runtime.Object
	failGet   bool
}

func newScanFixture(t *testing.T) *scanFixture {
	t.Helper()
	f := &scanFixture{dir: t.TempDir()}
	f.log = filepath.Join(f.dir, "bootstrap.log")
	f.diag = filepath.Join(f.dir, "diagnostics")
	f.key = filepath.Join(f.dir, "key.json")
	require.NoError(t, os.WriteFile(f.log, []byte("msg=\"nothing to see\"\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(f.diag, "nvke"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.diag, "nvke", "all-resources.txt"), []byte("ok\n"), 0o600))
	require.NoError(t, os.WriteFile(f.key, []byte(keyManifest(b64(etcdText))), 0o600))

	otlp, err := NewOTLPSettings()
	require.NoError(t, err)
	f.reachable.Store(true)
	f.objects = []runtime.Object{
		postgresSecret(map[string][]byte{"password": []byte(pgPassword)}),
		controllerDeployment(corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_HEADERS", Value: otlp.ControllerHeaders()}),
	}
	f.scan = &SecretScan{
		OTLP:           otlp,
		Known:          &SecretSet{},
		KnownFile:      filepath.Join(f.dir, "known.json"),
		LogFile:        f.log,
		DiagnosticsDir: f.diag,
		KeyFile:        f.key,
		Controller:     "nvke-controller",
		PollInterval:   time.Millisecond,
		Connect: func(context.Context) (kubernetes.Interface, error) {
			if !f.reachable.Load() {
				return nil, errors.New("kind: no such cluster")
			}
			kube := fake.NewClientset(f.objects...)
			if f.failGet {
				kube.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("connection refused")
				})
			}
			return kube, nil
		},
	}
	for _, v := range otlp.Secrets() {
		f.scan.Known.Add(v)
	}
	return f
}

func (f *scanFixture) writeLog(t *testing.T, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(f.log, []byte(content), 0o644))
}

func (f *scanFixture) writeDiag(t *testing.T, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.diag, "nvke", "all-resources.txt"), []byte(content), 0o600))
}

func (f *scanFixture) logText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.log)
	require.NoError(t, err)
	return string(data)
}

var errBootstrapFailed = errors.New("exit status 1")

func TestSecretScanFinish(t *testing.T) {
	t.Run("a passing run whose output is clean", func(t *testing.T) {
		f := newScanFixture(t)
		require.NoError(t, f.scan.Finish(context.Background(), nil))

		known, err := LoadSecretSet(f.scan.KnownFile)
		require.NoError(t, err)
		f.writeLog(t, "x "+pgPassword)
		hits, err := known.FindIn(f.log)
		require.NoError(t, err)
		assert.Len(t, hits, 1, "the password it read is saved for the search at the end of the lane")
		f.writeLog(t, "x "+etcdText)
		hits, err = known.FindIn(f.log)
		require.NoError(t, err)
		assert.Len(t, hits, 1, "and so is the etcd key, decoded")
	})

	t.Run("each known value in the log fails the run and withholds the log", func(t *testing.T) {
		values := map[string]func(f *scanFixture) string{
			"the OTLP token":        func(f *scanFixture) string { return f.scan.OTLP.Token },
			"the encoded header":    func(f *scanFixture) string { return "Bearer%20" + f.scan.OTLP.Token },
			"the tenant pair":       func(f *scanFixture) string { return "x-tenant=" + f.scan.OTLP.Tenant },
			"the Postgres password": func(*scanFixture) string { return pgPassword },
			"the stored etcd key":   func(*scanFixture) string { return b64(etcdText) },
			"the decoded etcd key":  func(*scanFixture) string { return etcdText },
		}
		for name, value := range values {
			t.Run(name, func(t *testing.T) {
				f := newScanFixture(t)
				leak := value(f)
				f.writeLog(t, "time=x msg=y field="+leak+"\n")

				err := f.scan.Finish(context.Background(), nil)
				require.Error(t, err)
				assert.Contains(t, err.Error(), f.log)
				assert.NotContains(t, err.Error(), leak, "the error goes to a public CI log")
				assert.NotContains(t, f.logText(t), leak, "a lane that prints the log afterwards prints a notice")
				assert.Equal(t, withheldLog, f.logText(t))
			})
		}
	})

	t.Run("a value only in the diagnostics fails the run and leaves the log", func(t *testing.T) {
		f := newScanFixture(t)
		f.writeDiag(t, "v="+pgPassword)
		err := f.scan.Finish(context.Background(), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), filepath.Join(f.diag, "nvke", "all-resources.txt"))
		assert.NotContains(t, err.Error(), pgPassword)
		assert.NotEqual(t, withheldLog, f.logText(t))
	})

	t.Run("a failed run whose log holds a value fails and withholds the log", func(t *testing.T) {
		f := newScanFixture(t)
		f.writeLog(t, pgPassword)
		require.Error(t, f.scan.Finish(context.Background(), errBootstrapFailed))
		assert.Equal(t, withheldLog, f.logText(t))
	})

	t.Run("a passing run needs the controller to hold the headers", func(t *testing.T) {
		f := newScanFixture(t)
		f.objects[1] = controllerDeployment(corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_HEADERS", Value: "authorization=Bearer%20other"})
		err := f.scan.Finish(context.Background(), nil)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), f.scan.OTLP.Token)
	})

	t.Run("a failed run is not held to the controller's headers", func(t *testing.T) {
		f := newScanFixture(t)
		f.objects[1] = controllerDeployment()
		assert.NoError(t, f.scan.Finish(context.Background(), errBootstrapFailed))
	})
}

func TestSecretScanFinishUnreadable(t *testing.T) {
	tests := []struct {
		name      string
		arrange   func(t *testing.T, f *scanFixture)
		bootstrap error
		wantErr   bool
	}{
		{
			name:    "a passing run with the cluster gone",
			arrange: func(_ *testing.T, f *scanFixture) { f.reachable.Store(false) },
			wantErr: true,
		},
		{
			name:    "a passing run with no key file",
			arrange: func(t *testing.T, f *scanFixture) { require.NoError(t, os.Remove(f.key)) },
			wantErr: true,
		},
		{
			name:    "a passing run with no Postgres Secret",
			arrange: func(_ *testing.T, f *scanFixture) { f.objects = f.objects[1:] },
			wantErr: true,
		},
		{
			name: "a failed run that never got as far as a cluster or a key",
			arrange: func(t *testing.T, f *scanFixture) {
				f.reachable.Store(false)
				require.NoError(t, os.Remove(f.key))
			},
			bootstrap: errBootstrapFailed,
		},
		{
			name: "a failed run whose cluster has no Postgres Secret yet",
			arrange: func(t *testing.T, f *scanFixture) {
				f.objects = f.objects[1:]
				require.NoError(t, os.Remove(f.key))
			},
			bootstrap: errBootstrapFailed,
		},
		{
			name:      "a failed run whose cluster was up and cannot be reached now",
			arrange:   func(_ *testing.T, f *scanFixture) { f.reachable.Store(false); f.scan.clusterSeen.Store(true) },
			bootstrap: errBootstrapFailed,
			wantErr:   true,
		},
		{
			name:      "a failed run whose Postgres Secret read fails",
			arrange:   func(_ *testing.T, f *scanFixture) { f.failGet = true },
			bootstrap: errBootstrapFailed,
			wantErr:   true,
		},
		{
			name: "a failed run whose key file holds no key",
			arrange: func(t *testing.T, f *scanFixture) {
				require.NoError(t, os.WriteFile(f.key, []byte(`{"data":{}}`), 0o600))
			},
			bootstrap: errBootstrapFailed,
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newScanFixture(t)
			tt.arrange(t, f)
			err := f.scan.Finish(context.Background(), tt.bootstrap)
			known, loadErr := LoadSecretSet(f.scan.KnownFile)
			require.NoError(t, loadErr)
			if !tt.wantErr {
				require.NoError(t, err)
				assert.NotEqual(t, withheldLog, f.logText(t))
				assert.Empty(t, known.Incomplete())
				assert.NoError(t, ScanKnownSecrets(f.scan.KnownFile, f.log, f.diag))
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "the search for them proves nothing")
			assert.Equal(t, withheldLog, f.logText(t), "a log that could not be fully searched is not kept")
			assert.NotEmpty(t, known.Incomplete(), "the incomplete search is saved for the search at the end of the lane")
			assert.Error(t, ScanKnownSecrets(f.scan.KnownFile, f.log, f.diag))
		})
	}
}

func TestSecretScanWatchesForTheCluster(t *testing.T) {
	f := newScanFixture(t)
	f.reachable.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, f.scan.Begin(ctx))
	assert.False(t, f.scan.clusterSeen.Load())

	f.reachable.Store(true)
	require.Eventually(t, f.scan.clusterSeen.Load, 5*time.Second, time.Millisecond, "the cluster comes up during the run")

	// It was up, so failing to read it afterwards is not "never built".
	f.reachable.Store(false)
	require.Error(t, f.scan.Finish(ctx, errBootstrapFailed))
}

func TestSecretScanWatchEndsWithoutSeeingTheCluster(t *testing.T) {
	f := newScanFixture(t)
	f.reachable.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, f.scan.Begin(ctx))
	time.Sleep(20 * time.Millisecond) // a few polls
	cancel()
	time.Sleep(20 * time.Millisecond)

	f.reachable.Store(true)
	time.Sleep(20 * time.Millisecond)
	assert.False(t, f.scan.clusterSeen.Load(), "a watch that has ended records nothing, and a cluster never seen is not an unreadable one")
}

func TestSecretScanBeginSavesTheOTLPValues(t *testing.T) {
	f := newScanFixture(t)
	f.scan.Known = &SecretSet{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, f.scan.Begin(ctx))

	known, err := LoadSecretSet(f.scan.KnownFile)
	require.NoError(t, err)
	assert.Equal(t, 2, known.Len(), "the token and the tenant pair, saved before the run starts")
}

func TestScanKnownSecrets(t *testing.T) {
	set := &SecretSet{}
	set.Add(planted)
	file := filepath.Join(t.TempDir(), "known.json")
	require.NoError(t, set.WriteFile(file))

	clean := tree(t, map[string]string{"boot.log": "ok", "diag/all.txt": "ok"})
	leaky := tree(t, map[string]string{"diag/late.txt": "collected after the bootstrap: " + planted})

	require.NoError(t, ScanKnownSecrets(file, filepath.Join(clean, "boot.log"), filepath.Join(clean, "diag"), ""))

	err := ScanKnownSecrets(file, filepath.Join(clean, "boot.log"), filepath.Join(leaky, "diag"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Join(leaky, "diag", "late.txt"))
	assert.NotContains(t, err.Error(), planted)

	assert.Error(t, ScanKnownSecrets(filepath.Join(t.TempDir(), "missing.json"), clean), "no values saved, so nothing was searched for")

	empty := filepath.Join(t.TempDir(), "empty.json")
	require.NoError(t, (&SecretSet{}).WriteFile(empty))
	assert.Error(t, ScanKnownSecrets(empty, clean), "a clean result for no values proves nothing")
}

func TestNewSecretScanFromEnv(t *testing.T) {
	opts := BootstrapOptions{LogFile: "/tmp/boot.log", EncryptionKeyOut: "/tmp/key.json", DiagnosticsDir: "/tmp/diag"}
	setAll := func(t *testing.T) {
		t.Helper()
		t.Setenv(EnvKnownSecretsFile, "/tmp/known.json")
		t.Setenv(EnvBootstrapCluster, "nvkectl-bootstrap")
		t.Setenv(EnvControllerName, "nvke-controller")
	}

	t.Run("off when the lane did not ask", func(t *testing.T) {
		t.Setenv(EnvKnownSecretsFile, "")
		scan, err := NewSecretScanFromEnv(opts)
		require.NoError(t, err)
		assert.Nil(t, scan)
	})
	t.Run("on", func(t *testing.T) {
		setAll(t)
		scan, err := NewSecretScanFromEnv(opts)
		require.NoError(t, err)
		require.NotNil(t, scan)
		assert.Equal(t, "/tmp/boot.log", scan.LogFile)
		assert.Equal(t, "/tmp/key.json", scan.KeyFile)
		assert.Equal(t, "/tmp/diag", scan.DiagnosticsDir)
		assert.Equal(t, "nvke-controller", scan.Controller)
		assert.Contains(t, scan.Env(), "OTEL_EXPORTER_OTLP_HEADERS="+scan.OTLP.Headers())
	})
	for name, unset := range map[string]string{"the cluster": EnvBootstrapCluster, "the controller": EnvControllerName} {
		t.Run("asked for without "+name, func(t *testing.T) {
			setAll(t)
			t.Setenv(unset, "")
			_, err := NewSecretScanFromEnv(opts)
			assert.Error(t, err)
		})
	}
	t.Run("asked for without a log, a key file or a diagnostics directory", func(t *testing.T) {
		setAll(t)
		for name, missing := range map[string]BootstrapOptions{
			"the log":         {EncryptionKeyOut: "/tmp/key.json", DiagnosticsDir: "/tmp/diag"},
			"the key file":    {LogFile: "/tmp/boot.log", DiagnosticsDir: "/tmp/diag"},
			"the diagnostics": {LogFile: "/tmp/boot.log", EncryptionKeyOut: "/tmp/key.json"},
		} {
			_, err := NewSecretScanFromEnv(missing)
			assert.Error(t, err, name)
		}
	})
}

func TestSecretScanUnreadablePasswordInOutputs(t *testing.T) {
	f := newScanFixture(t)
	f.failGet = true
	f.writeLog(t, "time=x msg=y field="+pgPassword+"\n")
	f.writeDiag(t, "v="+pgPassword)

	require.Error(t, f.scan.Finish(context.Background(), errBootstrapFailed))
	assert.Equal(t, withheldLog, f.logText(t), "the password could not be read, so the log cannot be called clean")

	err := ScanKnownSecrets(f.scan.KnownFile, f.log, f.diag)
	require.Error(t, err, "the search at the end of the lane runs against an incomplete set, and must not pass")
	assert.Contains(t, err.Error(), "incomplete")
	assert.NotContains(t, err.Error(), pgPassword)
}
