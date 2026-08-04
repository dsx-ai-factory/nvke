// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestApiServerIsReady(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		closeFirst bool
		expectRdy  bool
	}{
		{
			name:       "ready when readyz returns HTTP 200",
			statusCode: http.StatusOK,
			expectRdy:  true,
		},
		{
			name:       "not ready on non-2xx response",
			statusCode: http.StatusServiceUnavailable,
			expectRdy:  false,
		},
		{
			name:       "not ready on transport error during apiserver bring-up",
			statusCode: http.StatusOK,
			closeFirst: true,
			expectRdy:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/readyz", r.URL.Path)
				w.WriteHeader(tc.statusCode)
			}))
			t.Cleanup(httpServer.Close)
			if tc.closeFirst {
				httpServer.Close()
			}
			server := &NodeAgentServer{
				clientRegistry: &fakeLocalClientRegistry{localClient: newTestClientset(t, httpServer.URL)},
			}
			assert.Equal(t, tc.expectRdy, server.apiServerIsReady())
		})
	}
}

func TestFetchApiserverReadyzFromRegistry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/readyz", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	registry := &fakeLocalClientRegistry{localClient: newTestClientset(t, server.URL)}

	ready, err := fetchApiserverReadyzFromRegistry(registry)

	require.NoError(t, err)
	assert.True(t, ready)
	assert.Equal(t, 1, registry.localClientCalls)
}

// readinessSequence cycles through a fixed slice of HTTP status codes for
// every /readyz request, so a single test can assert behavior across
// multiple probes (e.g. several 503s followed by a 200). Self-contained on
// this branch; the OIDC-RPC test files reintroduce an identically-shaped
// helper for their own use.
type readinessSequence struct {
	statuses []int
	idx      atomic.Int32
}

func newReadinessSequence(statuses ...int) *readinessSequence {
	return &readinessSequence{statuses: statuses}
}

func (s *readinessSequence) next() int {
	idx := s.idx.Add(1) - 1
	return s.statuses[int(idx)%len(s.statuses)]
}

func (s *readinessSequence) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(s.next())
	})
}

// newReadinessTestServer wires up a NodeAgentServer with a clientRegistry
// pointing at the given httptest handler. Used by the waitForAPIServerReady
// loop tests below; a stripped-down equivalent of the OIDC RPC tests'
// newOIDCEnableTestServerWithReadiness, free of Persister/FileWriter/
// kubeadmFileOps machinery that is irrelevant for readiness loops.
func newReadinessTestServer(t *testing.T, handler http.Handler) *NodeAgentServer {
	t.Helper()
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return &NodeAgentServer{
		NodeStatus:     &NodeStatus{Role: ControlPlaneRole},
		clientRegistry: &fakeLocalClientRegistry{localClient: newTestClientset(t, httpServer.URL)},
	}
}

// TestWaitForAPIServerReady covers the four scenarios of the polling loop in
// (*NodeAgentServer).waitForAPIServerReady:
//
//  1. early success: the first probe succeeds, return nil without sleeping.
//  2. context cancellation: while blocked on the inter-probe sleep, ctx
//     cancellation causes the function to return ctx.Err().
//  3. override behavior: with small apiServerReadyMaxAttempts /
//     apiServerReadyPollInterval fields, the function converges within the
//     overridden budget when the apiserver becomes ready on probe N.
//  4. timeout after max attempts: when the apiserver never becomes ready,
//     the function returns a non-nil error whose message includes the
//     configured timeout (maxAttempts * pollInterval), and the probe count
//     equals maxAttempts + 1 (the loop probes maxAttempts+1 times before
//     failing).
func TestWaitForAPIServerReady(t *testing.T) {
	t.Run("early success returns nil after one probe without sleeping", func(t *testing.T) {
		seq := newReadinessSequence(http.StatusOK)
		srv := newReadinessTestServer(t, seq.handler())
		// Long poll interval acts as a sentinel: if the function ever
		// enters the sleep branch, elapsed time would exceed the assertion.
		srv.apiServerReadyMaxAttempts = 10
		srv.apiServerReadyPollInterval = 1 * time.Second

		start := time.Now()
		err := srv.waitForAPIServerReady(context.Background())
		elapsed := time.Since(start)

		require.NoError(t, err)
		assert.Equal(t, int32(1), seq.idx.Load(),
			"early success must probe exactly once")
		assert.Less(t, elapsed, 100*time.Millisecond,
			"early success must not enter the inter-probe sleep")
	})

	t.Run("returns ctx.Err() on context cancellation during inter-probe sleep", func(t *testing.T) {
		seq := newReadinessSequence(http.StatusServiceUnavailable)
		srv := newReadinessTestServer(t, seq.handler())
		srv.apiServerReadyMaxAttempts = 100
		srv.apiServerReadyPollInterval = 5 * time.Second

		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		start := time.Now()
		go func() { errCh <- srv.waitForAPIServerReady(ctx) }()

		// Give the goroutine time to issue the first probe and enter the
		// select. 200ms is generous; the actual HTTP roundtrip to a local
		// httptest server is <1ms.
		time.Sleep(200 * time.Millisecond)
		cancel()

		select {
		case err := <-errCh:
			require.Error(t, err)
			assert.ErrorIs(t, err, context.Canceled)
			assert.Less(t, time.Since(start), 2*time.Second,
				"ctx cancellation must short-circuit the 5s pollInterval")
		case <-time.After(3 * time.Second):
			t.Fatal("waitForAPIServerReady did not return after ctx cancellation")
		}
	})

	t.Run("converges when override allows enough attempts to reach ready", func(t *testing.T) {
		seq := newReadinessSequence(
			http.StatusServiceUnavailable,
			http.StatusServiceUnavailable,
			http.StatusServiceUnavailable,
			http.StatusOK,
		)
		srv := newReadinessTestServer(t, seq.handler())
		srv.apiServerReadyMaxAttempts = 5
		srv.apiServerReadyPollInterval = 5 * time.Millisecond

		err := srv.waitForAPIServerReady(context.Background())
		require.NoError(t, err)
		assert.Equal(t, int32(4), seq.idx.Load(),
			"must converge on the 4th probe (3x 503 then 1x 200)")
	})

	t.Run("returns timeout error including configured duration after maxAttempts+1 probes", func(t *testing.T) {
		seq := newReadinessSequence(http.StatusServiceUnavailable)
		srv := newReadinessTestServer(t, seq.handler())
		srv.apiServerReadyMaxAttempts = 3
		srv.apiServerReadyPollInterval = 5 * time.Millisecond

		err := srv.waitForAPIServerReady(context.Background())
		require.Error(t, err)

		wantDur := time.Duration(srv.apiServerReadyMaxAttempts) * srv.apiServerReadyPollInterval
		assert.Contains(t, err.Error(), wantDur.String(),
			"error message must include the configured timeout duration")
		assert.Equal(t, int32(srv.apiServerReadyMaxAttempts+1), seq.idx.Load(),
			"loop probes maxAttempts+1 times before returning the timeout error")
	})
}

// hostnameForTest returns os.Hostname() and skips the test on error. The
// production code resolves the hostname the same way, so wiring tests
// against this value lets them seed the fake clientset with a pod named
// kube-apiserver-<actual-hostname>.
func hostnameForTest(t *testing.T) string {
	t.Helper()
	h, err := os.Hostname()
	require.NoError(t, err)
	require.NotEmpty(t, h, "test host must have a hostname")
	return h
}

func mirrorPod(t *testing.T, uid string) *corev1.Pod {
	t.Helper()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("kube-apiserver-%s", hostnameForTest(t)),
			Namespace: "kube-system",
			UID:       types.UID(uid),
		},
	}
}

func TestReadAPIServerMirrorPodUID(t *testing.T) {
	t.Run("returns UID when pod exists", func(t *testing.T) {
		pod := mirrorPod(t, "uid-OLD")
		srv := &NodeAgentServer{
			clientRegistry: &fakeLocalClientRegistry{client: kubernetesfake.NewSimpleClientset(pod)},
		}

		uid, err := srv.readAPIServerMirrorPodUID(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "uid-OLD", uid)
	})

	t.Run("returns empty string and nil error on NotFound", func(t *testing.T) {
		srv := &NodeAgentServer{
			clientRegistry: &fakeLocalClientRegistry{client: kubernetesfake.NewSimpleClientset()},
		}

		uid, err := srv.readAPIServerMirrorPodUID(context.Background())
		require.NoError(t, err, "NotFound is a normal mid-swap state, not an error")
		assert.Empty(t, uid)
	})

	t.Run("returns transport error from Client()", func(t *testing.T) {
		srv := &NodeAgentServer{
			clientRegistry: &fakeLocalClientRegistry{err: errors.New("transport down")},
		}

		uid, err := srv.readAPIServerMirrorPodUID(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "transport down")
		assert.Empty(t, uid)
	})

	t.Run("returns error when registry is nil", func(t *testing.T) {
		srv := &NodeAgentServer{}

		_, err := srv.readAPIServerMirrorPodUID(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "registry")
	})
}

// TestWaitForAPIServerReadyAfterMutation covers the Phase 1 (UID flip) +
// Phase 2 (/readyz convergence) two-phase wait.
func TestWaitForAPIServerReadyAfterMutation(t *testing.T) {
	t.Run("empty preMutationPodUID falls back to waitForAPIServerReady (no mirror-pod read)", func(t *testing.T) {
		// /readyz returns 200 immediately; mirror-pod GET would fail
		// (no pod seeded). We assert the function still succeeds because
		// the empty UID short-circuits Phase 1 entirely.
		seq := newReadinessSequence(http.StatusOK)
		httpServer := httptest.NewServer(seq.handler())
		t.Cleanup(httpServer.Close)
		fakeClient := kubernetesfake.NewSimpleClientset()
		mirrorReads := atomic.Int32{}
		fakeClient.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			mirrorReads.Add(1)
			return false, nil, nil
		})
		srv := &NodeAgentServer{
			NodeStatus: &NodeStatus{Role: ControlPlaneRole},
			clientRegistry: &fakeLocalClientRegistry{
				localClient: newTestClientset(t, httpServer.URL),
				client:      fakeClient,
			},
		}

		err := srv.waitForAPIServerReadyAfterMutation(context.Background(), "")
		require.NoError(t, err)
		assert.Equal(t, int32(0), mirrorReads.Load(),
			"empty preMutationPodUID must skip the mirror-pod read entirely")
		assert.Equal(t, int32(1), seq.idx.Load(),
			"Phase 2 still runs and probes /readyz once")
	})

	t.Run("Phase 1 exits on first non-OLD UID, Phase 2 then succeeds", func(t *testing.T) {
		// Mirror-pod GET returns OLD twice then NEW. /readyz returns 200
		// immediately. Phase 1 should return after the third mirror-pod
		// read; Phase 2 then probes /readyz once.
		seq := newReadinessSequence(http.StatusOK)
		httpServer := httptest.NewServer(seq.handler())
		t.Cleanup(httpServer.Close)

		oldPod := mirrorPod(t, "uid-OLD")
		fakeClient := kubernetesfake.NewSimpleClientset(oldPod)
		callCount := atomic.Int32{}
		fakeClient.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			n := callCount.Add(1)
			if n <= 2 {
				return true, oldPod, nil
			}
			newPod := mirrorPod(t, "uid-NEW")
			return true, newPod, nil
		})

		srv := &NodeAgentServer{
			NodeStatus: &NodeStatus{Role: ControlPlaneRole},
			clientRegistry: &fakeLocalClientRegistry{
				localClient: newTestClientset(t, httpServer.URL),
				client:      fakeClient,
			},
			apiServerReadyMaxAttempts:  10,
			apiServerReadyPollInterval: 5 * time.Millisecond,
		}

		err := srv.waitForAPIServerReadyAfterMutation(context.Background(), "uid-OLD")
		require.NoError(t, err)
		assert.Equal(t, int32(3), callCount.Load(),
			"Phase 1 must probe exactly 3 times (OLD, OLD, NEW)")
		assert.Equal(t, int32(1), seq.idx.Load(),
			"Phase 2 must probe /readyz once after Phase 1 exits")
	})

	t.Run("transient NotFound during swap is treated as 'keep polling' until UID returns as NEW", func(t *testing.T) {
		// Mirror-pod GET returns OLD, then NotFound (kubelet mid-recreate),
		// then NEW. Phase 1 must exit on the NotFound probe (cur == "")
		// per the documented contract, NOT on the NEW probe.
		seq := newReadinessSequence(http.StatusOK)
		httpServer := httptest.NewServer(seq.handler())
		t.Cleanup(httpServer.Close)

		oldPod := mirrorPod(t, "uid-OLD")
		fakeClient := kubernetesfake.NewSimpleClientset(oldPod)
		callCount := atomic.Int32{}
		fakeClient.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			n := callCount.Add(1)
			switch n {
			case 1:
				return true, oldPod, nil
			default:
				return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, oldPod.Name)
			}
		})

		srv := &NodeAgentServer{
			NodeStatus: &NodeStatus{Role: ControlPlaneRole},
			clientRegistry: &fakeLocalClientRegistry{
				localClient: newTestClientset(t, httpServer.URL),
				client:      fakeClient,
			},
			apiServerReadyMaxAttempts:  10,
			apiServerReadyPollInterval: 5 * time.Millisecond,
		}

		err := srv.waitForAPIServerReadyAfterMutation(context.Background(), "uid-OLD")
		require.NoError(t, err)
		assert.Equal(t, int32(2), callCount.Load(),
			"Phase 1 must exit on the NotFound probe (cur == \"\"); contract is documented as a normal mid-swap state")
	})

	t.Run("Phase 1 timeout when UID never flips returns a clear error naming the stuck UID", func(t *testing.T) {
		oldPod := mirrorPod(t, "uid-stuck")
		fakeClient := kubernetesfake.NewSimpleClientset(oldPod)
		fakeClient.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			return true, oldPod, nil
		})

		// /readyz handler is wired up but should never be hit because Phase 1 fails.
		seq := newReadinessSequence(http.StatusOK)
		httpServer := httptest.NewServer(seq.handler())
		t.Cleanup(httpServer.Close)

		srv := &NodeAgentServer{
			NodeStatus: &NodeStatus{Role: ControlPlaneRole},
			clientRegistry: &fakeLocalClientRegistry{
				localClient: newTestClientset(t, httpServer.URL),
				client:      fakeClient,
			},
			apiServerReadyMaxAttempts:  3,
			apiServerReadyPollInterval: 5 * time.Millisecond,
		}

		err := srv.waitForAPIServerReadyAfterMutation(context.Background(), "uid-stuck")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "uid-stuck",
			"error message must name the stuck pre-mutation UID")
		assert.Contains(t, err.Error(), "did not change",
			"error message must explain Phase 1 timed out on the UID flip")
		assert.Equal(t, int32(0), seq.idx.Load(),
			"Phase 2 must not run when Phase 1 fails")
	})

	t.Run("transient mirror-pod read errors are retried and do not abort Phase 1", func(t *testing.T) {
		// Two consecutive transport errors then a NEW UID. Phase 1 must
		// log+retry the errors per the documented contract; the function
		// should eventually succeed.
		seq := newReadinessSequence(http.StatusOK)
		httpServer := httptest.NewServer(seq.handler())
		t.Cleanup(httpServer.Close)

		fakeClient := kubernetesfake.NewSimpleClientset()
		callCount := atomic.Int32{}
		fakeClient.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			n := callCount.Add(1)
			if n <= 2 {
				return true, nil, errors.New("transient transport error")
			}
			return true, mirrorPod(t, "uid-NEW"), nil
		})

		srv := &NodeAgentServer{
			NodeStatus: &NodeStatus{Role: ControlPlaneRole},
			clientRegistry: &fakeLocalClientRegistry{
				localClient: newTestClientset(t, httpServer.URL),
				client:      fakeClient,
			},
			apiServerReadyMaxAttempts:  10,
			apiServerReadyPollInterval: 5 * time.Millisecond,
		}

		err := srv.waitForAPIServerReadyAfterMutation(context.Background(), "uid-OLD")
		require.NoError(t, err)
		assert.GreaterOrEqual(t, callCount.Load(), int32(3),
			"Phase 1 must retry through the transient errors")
	})

	t.Run("returns ctx.Err() on cancellation during Phase 1 sleep", func(t *testing.T) {
		oldPod := mirrorPod(t, "uid-OLD")
		fakeClient := kubernetesfake.NewSimpleClientset(oldPod)
		fakeClient.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			return true, oldPod, nil
		})

		seq := newReadinessSequence(http.StatusServiceUnavailable)
		httpServer := httptest.NewServer(seq.handler())
		t.Cleanup(httpServer.Close)

		srv := &NodeAgentServer{
			NodeStatus: &NodeStatus{Role: ControlPlaneRole},
			clientRegistry: &fakeLocalClientRegistry{
				localClient: newTestClientset(t, httpServer.URL),
				client:      fakeClient,
			},
			apiServerReadyMaxAttempts:  100,
			apiServerReadyPollInterval: 5 * time.Second,
		}

		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		start := time.Now()
		go func() { errCh <- srv.waitForAPIServerReadyAfterMutation(ctx, "uid-OLD") }()

		time.Sleep(200 * time.Millisecond)
		cancel()

		select {
		case err := <-errCh:
			require.Error(t, err)
			assert.ErrorIs(t, err, context.Canceled)
			assert.Less(t, time.Since(start), 2*time.Second,
				"ctx cancellation must short-circuit the 5s pollInterval")
		case <-time.After(3 * time.Second):
			t.Fatal("waitForAPIServerReadyAfterMutation did not return after ctx cancellation")
		}
	})
}
