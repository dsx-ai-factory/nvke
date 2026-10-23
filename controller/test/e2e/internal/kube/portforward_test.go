// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

type fakeSession struct {
	server   *http.Server
	listener net.Listener
	done     chan struct{}
	stopOnce sync.Once
}

func (s *fakeSession) stop() {
	s.stopOnce.Do(func() {
		_ = s.server.Close()
		close(s.done)
	})
}

// dropListener leaves done open: the forward is gone but nothing said so.
func (s *fakeSession) dropListener() {
	_ = s.listener.Close()
}

type fakeStarter struct {
	mu       sync.Mutex
	starts   int
	failures int
	sessions []*fakeSession
}

func (f *fakeStarter) Target() string { return "test/fake:8080" }

func (f *fakeStarter) Start(ctx context.Context) (*forwardSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.starts++
	if f.failures > 0 {
		f.failures--
		return nil, errors.New("simulated start failure")
	}

	generation := f.starts
	session, err := serveSession(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Generation", fmt.Sprint(generation))
		w.Header().Set("X-Host", r.Host)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	if err != nil {
		return nil, err
	}
	f.sessions = append(f.sessions, session)

	return session.forward(), nil
}

func serveSession(ctx context.Context, handler http.Handler) (*fakeSession, error) {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: handler}
	session := &fakeSession{server: server, listener: listener, done: make(chan struct{})}
	go func() { _ = server.Serve(listener) }()
	return session, nil
}

func (s *fakeSession) forward() *forwardSession {
	return &forwardSession{addr: s.listener.Addr().String(), done: s.done, stop: s.stop}
}

// nameLabel is the selector the fake Service and its pods agree on.
const nameLabel = "app.kubernetes.io/name"

// newTestForwarder scales the budgets and backoff down, keeping the cold-start
// one smaller than the reconnect one. The cold-start budget also covers every
// test's first request: listen, serve and dial a loopback socket under -race on
// a loaded runner. At 10ms that failed about one run in ten on macOS with six
// test processes sharing the machine; the deadline surfaced as the dial's
// "i/o timeout". The two tests that measure the budget check against 2s and 5s
// ceilings, so 1s keeps them meaningful.
func newTestForwarder(starter sessionStarter, baseURL string) *ServiceForwarder {
	forwarder := newServiceForwarder(starter, baseURL)
	forwarder.coldStartBudget = time.Second
	forwarder.reconnectBudget = 2 * time.Second
	forwarder.retryBackoff = time.Millisecond
	forwarder.maxRetryBackoff = 5 * time.Millisecond
	return forwarder
}

func (f *fakeStarter) setFailures(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = n
}

func (f *fakeStarter) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

// mustGetOK fails unless the stock request came back 200; `what` names the step.
func mustGetOK(t *testing.T, forwarder *ServiceForwarder, what string) {
	t.Helper()
	status, body, err := forwarder.Get(t.Context(), "/healthz")
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if status != http.StatusOK {
		t.Fatalf("%s: status %d, body %q", what, status, body)
	}
}

func (f *fakeStarter) lastSession() *fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[len(f.sessions)-1]
}

func TestServiceForwarderDialsLazilyAndReuses(t *testing.T) {
	starter := &fakeStarter{}
	forwarder := newTestForwarder(starter, "http://svc.ns.svc:8080")
	defer forwarder.Close()

	if got := starter.startCount(); got != 0 {
		t.Fatalf("forwarder started %d sessions before any request, want 0", got)
	}

	for i := range 3 {
		mustGetOK(t, forwarder, fmt.Sprintf("request %d", i))
	}

	if got := starter.startCount(); got != 1 {
		t.Fatalf("forwarder started %d sessions for 3 requests, want 1", got)
	}
}

func TestServiceForwarderReconnectsAfterForwardEnds(t *testing.T) {
	starter := &fakeStarter{}
	forwarder := newTestForwarder(starter, "http://svc.ns.svc:8080")
	defer forwarder.Close()

	mustGetOK(t, forwarder, "first request")

	// The forward ends the way a real one does when its pod goes away.
	starter.lastSession().stop()

	mustGetOK(t, forwarder, "request after the forward ended")
	if got := starter.startCount(); got != 2 {
		t.Fatalf("forwarder started %d sessions, want 2 (one reconnect)", got)
	}
}

func TestServiceForwarderReconnectsAfterSilentListenerLoss(t *testing.T) {
	starter := &fakeStarter{}
	forwarder := newTestForwarder(starter, "http://svc.ns.svc:8080")
	defer forwarder.Close()

	mustGetOK(t, forwarder, "first request")

	// Listener gone, done never closed: the forwarder has to notice on its own.
	starter.lastSession().dropListener()

	mustGetOK(t, forwarder, "request after silent listener loss")
	if got := starter.startCount(); got != 2 {
		t.Fatalf("forwarder started %d sessions, want 2 (one reconnect)", got)
	}
}

// A forward that used to work gets the generous budget.
func TestServiceForwarderRidesOutARollout(t *testing.T) {
	starter := &fakeStarter{}
	forwarder := newTestForwarder(starter, "http://svc.ns.svc:8080")
	defer forwarder.Close()

	mustGetOK(t, forwarder, "first request")

	// The pod goes away and nothing is forwardable for a while.
	starter.lastSession().stop()
	starter.setFailures(5)

	mustGetOK(t, forwarder, "request across a rollout should have ridden it out")
}

// A forwarder that never connected fails inside the cold-start budget.
func TestServiceForwarderFailsFastBeforeItEverConnects(t *testing.T) {
	starter := &fakeStarter{failures: 1_000}
	forwarder := newTestForwarder(starter, "http://svc.ns.svc:8080")
	defer forwarder.Close()

	start := time.Now()
	if _, _, err := forwarder.Get(t.Context(), "/healthz"); err == nil {
		t.Fatal("expected an error when the forward never comes up")
	}
	if elapsed := time.Since(start); elapsed > forwarder.reconnectBudget {
		t.Fatalf("cold start took %v, longer than the reconnect budget %v", elapsed, forwarder.reconnectBudget)
	}
	if got := starter.startCount(); got < 1 {
		t.Fatal("forwarder never attempted to start")
	}
}

// blockingStarter never comes up and never fails: Start blocks until the context
// it is handed ends. That is what a forward does when the apiserver accepts the
// stream request and then nothing answers, and it is the case sessionStartTimeout
// exists for.
type blockingStarter struct {
	mu     sync.Mutex
	starts int
}

func (b *blockingStarter) Target() string { return "test/blocking:8080" }

func (b *blockingStarter) Start(ctx context.Context) (*forwardSession, error) {
	b.mu.Lock()
	b.starts++
	b.mu.Unlock()

	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockingStarter) startCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.starts
}

// The budget has to bound the wall clock, not just the number of attempts.
// A single attempt can sit in Start for sessionStartTimeout and then spend
// localDialTimeout on top, 40s between them, so checking the deadline only
// between attempts let a 5s cold start take 40s to give up — and the caller saw
// a slow hang rather than the error naming the service it could not reach.
func TestServiceForwarderBudgetBoundsOneSlowAttempt(t *testing.T) {
	starter := &blockingStarter{}
	forwarder := newTestForwarder(starter, "http://svc.ns.svc:8080")
	defer forwarder.Close()

	start := time.Now()
	_, _, err := forwarder.Get(t.Context(), "/healthz")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when the forward never starts")
	}
	// Deliberately loose. The assertion is that the wait is governed by the
	// budget at all, not by sessionStartTimeout, which is 30s and thirty times
	// the cold-start budget this forwarder runs with.
	if limit := 5 * time.Second; elapsed > limit {
		t.Fatalf("a %v budget took %v to give up, past the %v ceiling: the deadline is not bounding a single attempt",
			forwarder.coldStartBudget, elapsed, limit)
	}
	if starter.startCount() < 1 {
		t.Fatal("forwarder never attempted to start")
	}
}

func TestServiceForwarderCloseStopsTheForward(t *testing.T) {
	starter := &fakeStarter{}
	forwarder := newTestForwarder(starter, "http://svc.ns.svc:8080")

	mustGetOK(t, forwarder, "first request")
	session := starter.lastSession()

	forwarder.Close()
	select {
	case <-session.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop the forward")
	}

	if _, _, err := forwarder.Get(t.Context(), "/healthz"); err == nil {
		t.Fatal("expected an error after Close")
	}
	forwarder.Close() // idempotent
}

// resolvingStarter is the real pod-resolution path with the SPDY tunnel
// replaced by a loopback server.
type resolvingStarter struct {
	inner *podForwardStarter

	mu       sync.Mutex
	sessions []*fakeSession
}

func (r *resolvingStarter) Target() string { return r.inner.Target() }

func (r *resolvingStarter) Start(ctx context.Context) (*forwardSession, error) {
	podName, podPort, err := r.inner.resolve(ctx)
	if err != nil {
		return nil, err
	}

	session, err := serveSession(ctx, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Pod", podName)
		w.Header().Set("X-Pod-Port", fmt.Sprint(podPort))
		w.WriteHeader(http.StatusOK)
	}))
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.sessions = append(r.sessions, session)
	r.mu.Unlock()

	return session.forward(), nil
}

func (r *resolvingStarter) lastSession() *fakeSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[len(r.sessions)-1]
}

func controllerForwarder(t *testing.T) (*fake.Clientset, *resolvingStarter, *ServiceForwarder) {
	t.Helper()
	clientset := fake.NewClientset(controllerService(), readyControllerPod("cwe-old"))
	starter := &resolvingStarter{inner: &podForwardStarter{
		clientset: clientset,
		namespace: "nvke-controller",
		service:   "nvke-controller",
		port:      "8080",
	}}
	forwarder := newTestForwarder(starter, "http://nvke-controller.nvke-controller.svc:8080")
	t.Cleanup(forwarder.Close)
	return clientset, starter, forwarder
}

func readyControllerPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "nvke-controller",
			Name:      name,
			Labels:    map[string]string{nameLabel: "nvke-controller"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "nvke-controller",
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
		}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

func controllerService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "nvke-controller", Name: "nvke-controller"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{nameLabel: "nvke-controller"},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8080, TargetPort: intstr.FromString("http")},
			},
		},
	}
}

// A rollout is what drops these forwards, so every reconnect re-resolves rather
// than redialling the same pod name forever. The gap in the middle — the old pod
// gone, the new one not yet Ready — must surface as an error rather than wedge
// the forwarder.
func TestServiceForwarderRedialResolvesTheReplacementPod(t *testing.T) {
	clientset, starter, forwarder := controllerForwarder(t)

	if pod := requestPod(t, forwarder); pod != "cwe-old" {
		t.Fatalf("first request landed on pod %q, want cwe-old", pod)
	}

	pods := clientset.CoreV1().Pods("nvke-controller")
	if err := pods.Delete(t.Context(), "cwe-old", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	starter.lastSession().stop()

	if _, _, err := forwarder.Get(t.Context(), "/healthz"); err == nil {
		t.Fatal("expected an error while no pod backs the Service")
	}

	if _, err := pods.Create(t.Context(), readyControllerPod("cwe-new"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if pod := requestPod(t, forwarder); pod != "cwe-new" {
		t.Fatalf("request after the rollout landed on pod %q, want cwe-new", pod)
	}
}

func requestPod(t *testing.T, forwarder *ServiceForwarder) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, forwarder.BaseURL()+"/healthz", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := forwarder.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Pod-Port"); got != "8080" {
		t.Fatalf("forwarded to pod port %q, want 8080 (named targetPort resolution)", got)
	}
	return resp.Header.Get("X-Pod")
}

func TestSelectServicePort(t *testing.T) {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "nvke-controller", Name: "nvke-controller"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "http", Port: 8080, TargetPort: intstr.FromString("http")},
			{Name: "metrics", Port: 9090, TargetPort: intstr.FromString("metrics")},
		}},
	}

	for _, tc := range []struct {
		name    string
		port    string
		want    int32
		wantErr bool
	}{
		{name: "by number", port: "8080", want: 8080},
		{name: "by name", port: "metrics", want: 9090},
		{name: "unknown", port: "1234", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectServicePort(service, tc.port)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got port %d", got.Port)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Port != tc.want {
				t.Fatalf("got port %d, want %d", got.Port, tc.want)
			}
		})
	}
}

func TestResolveTargetPort(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "nvke-controller", Name: "cwe-0"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
		}}},
	}

	got, err := resolveTargetPort(pod, &corev1.ServicePort{Port: 8080, TargetPort: intstr.FromString("http")})
	if err != nil || got != 8080 {
		t.Fatalf("named targetPort: got %d, err %v", got, err)
	}

	got, err = resolveTargetPort(pod, &corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt32(8080)})
	if err != nil || got != 8080 {
		t.Fatalf("numeric targetPort: got %d, err %v", got, err)
	}

	got, err = resolveTargetPort(pod, &corev1.ServicePort{Port: 8080})
	if err != nil || got != 8080 {
		t.Fatalf("unset targetPort: got %d, err %v", got, err)
	}

	if _, err := resolveTargetPort(pod, &corev1.ServicePort{Port: 9090, TargetPort: intstr.FromString("metrics")}); err == nil {
		t.Fatal("want an error for a container port that does not exist")
	}
}

func TestPickReadyPod(t *testing.T) {
	ready := func(name string, phase corev1.PodPhase, isReady, terminating bool) corev1.Pod {
		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: corev1.PodStatus{Phase: phase, Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionFalse,
			}}},
		}
		if isReady {
			pod.Status.Conditions[0].Status = corev1.ConditionTrue
		}
		if terminating {
			now := metav1.Now()
			pod.DeletionTimestamp = &now
		}
		return pod
	}

	pods := []corev1.Pod{
		ready("cwe-c", corev1.PodRunning, true, true),   // terminating
		ready("cwe-a", corev1.PodPending, false, false), // not running
		ready("cwe-d", corev1.PodRunning, false, false), // not ready
		ready("cwe-b", corev1.PodRunning, true, false),  // the only candidate
	}

	got := pickReadyPod(pods)
	if got == nil || got.Name != "cwe-b" {
		t.Fatalf("picked %v, want cwe-b", got)
	}

	if pickReadyPod(pods[:3]) != nil {
		t.Fatal("want nil when no pod is ready")
	}
}

// TestServiceBaseURL: the NVKE Service publishes its metrics listener under a
// name, and a name in the authority makes every URL built from it unparseable.
func TestServiceBaseURL(t *testing.T) {
	tests := []struct {
		name string
		port string
		want string
	}{
		{
			name: "a numeric port is kept, so the Host header names it",
			port: "8080",
			want: "http://nvke-controller.nvke-controller.svc:8080",
		},
		{
			name: "a named port is dropped, because a name is not a URL port",
			port: "metrics",
			want: "http://nvke-controller.nvke-controller.svc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serviceBaseURL("nvke-controller", "nvke-controller", tt.port)
			if got != tt.want {
				t.Fatalf("serviceBaseURL = %q, want %q", got, tt.want)
			}
			if _, err := url.Parse(got + "/metrics"); err != nil {
				t.Fatalf("a request URL built from %q does not parse: %v", got, err)
			}
		})
	}
}

// wedgedFirstStarter hands out a session that accepts and closes every
// connection first, the way a port-forward whose SPDY connection has wedged
// does, and healthy sessions after.
type wedgedFirstStarter struct {
	inner      *fakeStarter
	mu         sync.Mutex
	starts     int
	wedgedDone chan struct{}
}

func (w *wedgedFirstStarter) Target() string { return w.inner.Target() }

func (w *wedgedFirstStarter) Start(ctx context.Context) (*forwardSession, error) {
	w.mu.Lock()
	w.starts++
	first := w.starts == 1
	w.mu.Unlock()
	if !first {
		return w.inner.Start(ctx)
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	done := make(chan struct{})
	w.mu.Lock()
	w.wedgedDone = done
	w.mu.Unlock()
	return &forwardSession{
		addr: listener.Addr().String(),
		done: done,
		stop: sync.OnceFunc(func() { _ = listener.Close(); close(done) }),
	}, nil
}

func TestServiceForwarderRecoversFromAWedgedSession(t *testing.T) {
	starter := &wedgedFirstStarter{inner: &fakeStarter{}}
	forwarder := newTestForwarder(starter, "http://nvke-controller.nvke-controller.svc:8080")
	t.Cleanup(forwarder.Close)

	mustGetOK(t, forwarder, "GET through a forward whose first session is wedged")

	if starter.inner.startCount() != 1 {
		t.Errorf("fresh sessions started = %d, want 1", starter.inner.startCount())
	}
	select {
	case <-starter.wedgedDone:
	default:
		t.Error("the wedged session was not stopped")
	}
}
