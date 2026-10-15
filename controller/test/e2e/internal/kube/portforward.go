// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/httpstream" //nolint:staticcheck // see dialer
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// ServiceForwarder gives HTTP access to a Service inside the management cluster
// over a port-forward to a backing pod. The tunnel is plain TCP, so the request
// through it is entirely the caller's, headers included, and it needs only a
// kubeconfig and API-server reachability. It is established lazily and
// re-established whenever found dead, which suffices because the suite's long
// waits are polling loops of short requests.
type ServiceForwarder struct {
	starter sessionStarter
	target  string
	baseURL string
	client  *http.Client

	coldStartBudget time.Duration
	reconnectBudget time.Duration
	retryBackoff    time.Duration
	maxRetryBackoff time.Duration

	mu            sync.Mutex
	current       *forwardSession
	closed        bool
	everConnected bool
}

const (
	// coldStartBudget bounds the first connection, where failure means an
	// undeployed NVKE or the wrong kubeconfig.
	coldStartBudget = 5 * time.Second
	// reconnectBudget has to outlast a pod replacement, because an NVKE rollout
	// breaks these and the waiters in internal/nvke abort on the first error.
	reconnectBudget     = 90 * time.Second
	retryBackoff        = time.Second
	maxRetryBackoff     = 10 * time.Second
	sessionStartTimeout = 30 * time.Second
	localDialTimeout    = 10 * time.Second
)

var (
	// errForwarderClosed ends the retry loop: a closed forwarder is not coming back.
	errForwarderClosed = errors.New("port-forward is closed")

	// errNoLocalPort stands in when GetPorts succeeds but reports nothing, so
	// the wrapped error is never nil.
	errNoLocalPort = errors.New("port-forward reported no local port")
)

// dialError marks a request that never reached the forward; retrying it would only repeat the dial.
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

type forwardSession struct {
	addr string
	done <-chan struct{}
	stop func()

	// err is written before done is closed and read only after, so that close
	// is the happens-before edge and no lock is needed.
	err error
}

// sessionStarter is an interface so reconnects can be tested without a cluster.
type sessionStarter interface {
	Start(ctx context.Context) (*forwardSession, error)
	// Target names what is being forwarded to, for error messages.
	Target() string
}

// NewServiceForwarder builds a forwarder for namespace/service:port, where port
// is the Service port, named or numeric. Nothing is dialed until first use.
func NewServiceForwarder(restConfig *rest.Config, namespace, service, port string) (*ServiceForwarder, error) {
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building Kubernetes clientset: %w", err)
	}
	starter := &podForwardStarter{
		restConfig: restConfig,
		clientset:  clientset,
		namespace:  namespace,
		service:    service,
		port:       port,
	}
	return newServiceForwarder(starter, serviceBaseURL(namespace, service, port)), nil
}

// serviceBaseURL is the authority requests through the forward carry. It is
// never resolved, and exists so requests carry a stable Host header as the
// local port changes. The port is omitted unless numeric, because a named one
// (the NVKE Service publishes "metrics") fails url.Parse.
func serviceBaseURL(namespace, service, port string) string {
	host := fmt.Sprintf("http://%s.%s.svc", service, namespace)
	if _, err := strconv.Atoi(port); err != nil {
		return host
	}
	return host + ":" + port
}

func newServiceForwarder(starter sessionStarter, baseURL string) *ServiceForwarder {
	f := &ServiceForwarder{
		starter:         starter,
		target:          starter.Target(),
		baseURL:         baseURL,
		coldStartBudget: coldStartBudget,
		reconnectBudget: reconnectBudget,
		retryBackoff:    retryBackoff,
		maxRetryBackoff: maxRetryBackoff,
	}
	f.client = &http.Client{
		Transport: &recoveringTransport{forwarder: f, next: &http.Transport{
			DialContext: f.dialContext,
			// A connection that went stale while the forward was down would
			// fail the next request through it, and http.Transport only retries
			// idempotent ones.
			DisableKeepAlives: true,
		}},
	}
	return f
}

// recoveringTransport drops the forward session behind a request that failed at the transport level and retries a GET or HEAD once on a fresh one.
type recoveringTransport struct {
	forwarder *ServiceForwarder
	next      http.RoundTripper
}

func (t *recoveringTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	var dial *dialError
	if err == nil || req.Context().Err() != nil || errors.As(err, &dial) {
		return resp, err
	}
	// A wedged SPDY session still accepts the local dial and then resets every stream, so nothing but the request sees it die.
	slog.Warn("port-forward session replaced after a transport failure", "target", t.forwarder.target, "method", req.Method, "path", req.URL.Path, "err", err)
	t.forwarder.Reset()
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return nil, err
	}
	return t.next.RoundTrip(req)
}

// BaseURL is the scheme and authority to build request URLs against.
func (f *ServiceForwarder) BaseURL() string { return f.baseURL }

// HTTPClient returns a client whose requests travel through the forward. It
// sets no headers of its own.
func (f *ServiceForwarder) HTTPClient() *http.Client { return f.client }

// Dial opens a raw TCP connection through the forward, for a caller that speaks
// something other than plain HTTP to the Service, such as its own TLS
// handshake. It reconnects exactly as a request through HTTPClient would.
func (f *ServiceForwarder) Dial(ctx context.Context) (net.Conn, error) {
	return f.dialContext(ctx, "tcp", "")
}

// Get performs a GET of path through the forward. Any response is data; the
// error is non-nil only for transport failures.
func (f *ServiceForwarder) Get(ctx context.Context, path string) (int, []byte, error) {
	requestURL := f.baseURL + "/" + strings.TrimPrefix(path, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, http.NoBody)
	if err != nil {
		return 0, nil, fmt.Errorf("building request for %s: %w", path, err)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s through port-forward to %s: %w", path, f.target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response body for %s: %w", path, err)
	}

	return resp.StatusCode, body, nil
}

// Reset drops the live session without closing the forwarder, so the next
// request establishes a new one against whatever pod backs the Service then.
//
// This is what a caller that replaced the pod itself wants. Close would leave a
// forwarder that never reconnects, and callers such as internal/nvke's API client
// hold one from before the rollout.
func (f *ServiceForwarder) Reset() {
	f.mu.Lock()
	session := f.current
	f.current = nil
	f.mu.Unlock()

	if session != nil {
		session.stop()
	}
}

// Close tears the forward down. It is safe to call more than once.
func (f *ServiceForwarder) Close() {
	f.mu.Lock()
	session := f.current
	f.current = nil
	f.closed = true
	f.mu.Unlock()

	if session != nil {
		session.stop()
	}
}

// dialContext ignores the requested address, a placeholder, and connects to the
// live forward, re-establishing it when the previous one is gone.
func (f *ServiceForwarder) dialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	deadline := time.Now().Add(f.budget())
	backoff := f.retryBackoff

	var lastErr error
	for retry := false; ; retry = true {
		if retry {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, f.maxRetryBackoff)
		}

		// Each attempt gets what is left of the budget, not its own full
		// sessionStartTimeout plus localDialTimeout. Checking the deadline only
		// between attempts bounds how many we make, not how long we wait: one
		// attempt can spend 40s, so a 5s cold start took 40s to give up, and the
		// caller saw a slow hang instead of the error naming the service.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		attemptCtx, cancel := context.WithTimeout(ctx, remaining)

		session, err := f.session(attemptCtx)
		if err != nil {
			cancel()
			if errors.Is(err, errForwarderClosed) {
				return nil, &dialError{err}
			}
			lastErr = err
			continue
		}

		// Canceling after a successful dial is safe: the context bounds the
		// dial, not the connection it returns.
		conn, err := (&net.Dialer{Timeout: localDialTimeout}).DialContext(attemptCtx, network, session.addr)
		cancel()
		if err == nil {
			f.markConnected()
			return conn, nil
		}
		// The listener is gone but nothing told us; drop it and rebuild.
		lastErr = err
		f.discard(session)
	}
	return nil, &dialError{fmt.Errorf("connecting to %s through a port-forward: %w", f.target, lastErr)}
}

func (f *ServiceForwarder) budget() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.everConnected {
		return f.reconnectBudget
	}
	return f.coldStartBudget
}

func (f *ServiceForwarder) markConnected() {
	f.mu.Lock()
	f.everConnected = true
	f.mu.Unlock()
}

// session returns the live forward, starting one if there is none and replacing
// one that ended. Why the previous one died is carried into the failure of its
// replacement, being the more informative half.
func (f *ServiceForwarder) session(ctx context.Context) (*forwardSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return nil, errForwarderClosed
	}

	var deadErr error
	if f.current != nil {
		select {
		case <-f.current.done:
			dead := f.current
			f.current = nil
			dead.stop()
			deadErr = dead.err
		default:
			return f.current, nil
		}
	}

	startCtx, cancel := context.WithTimeout(ctx, sessionStartTimeout)
	defer cancel()

	session, err := f.starter.Start(startCtx)
	if err != nil {
		if deadErr != nil {
			return nil, fmt.Errorf("establishing port-forward to %s: %w (the previous forward ended with: %w)", f.target, err, deadErr)
		}
		return nil, fmt.Errorf("establishing port-forward to %s: %w", f.target, err)
	}
	f.current = session
	return session, nil
}

// discard drops session if it is still the current one.
func (f *ServiceForwarder) discard(session *forwardSession) {
	f.mu.Lock()
	if f.current != session {
		f.mu.Unlock()
		return
	}
	f.current = nil
	f.mu.Unlock()

	session.stop()
}

// podForwardStarter resolves a Service to a ready pod and forwards to it.
type podForwardStarter struct {
	restConfig *rest.Config
	clientset  kubernetes.Interface
	namespace  string
	service    string
	port       string
}

func (s *podForwardStarter) Target() string {
	return fmt.Sprintf("%s/%s:%s", s.namespace, s.service, s.port)
}

func (s *podForwardStarter) Start(ctx context.Context) (*forwardSession, error) {
	podName, podPort, err := s.resolve(ctx)
	if err != nil {
		return nil, err
	}

	requestURL := s.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(s.namespace).
		Name(podName).
		SubResource("portforward").
		URL()

	dialer, err := s.dialer(requestURL)
	if err != nil {
		return nil, err
	}

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	forwarder, err := portforward.NewOnAddresses(
		dialer,
		[]string{"127.0.0.1"},
		// Local port 0, so this cannot collide with a developer's Tilt
		// port-forward or a concurrent suite run.
		[]string{fmt.Sprintf("0:%d", podPort)},
		stopCh,
		readyCh,
		io.Discard,
		io.Discard,
	)
	if err != nil {
		return nil, fmt.Errorf("building port-forward to pod %s/%s: %w", s.namespace, podName, err)
	}

	doneCh := make(chan struct{})
	session := &forwardSession{done: doneCh}
	go func() {
		defer close(doneCh)
		session.err = forwarder.ForwardPorts()
	}()

	var stopOnce sync.Once
	session.stop = func() {
		stopOnce.Do(func() { close(stopCh) })
		<-doneCh
	}

	select {
	case <-readyCh:
	case <-doneCh:
		return nil, fmt.Errorf("port-forward to pod %s/%s ended before it was ready: %w", s.namespace, podName, session.err)
	case <-ctx.Done():
		session.stop()
		return nil, fmt.Errorf("waiting for port-forward to pod %s/%s: %w", s.namespace, podName, ctx.Err())
	}

	ports, err := forwarder.GetPorts()
	if err == nil && len(ports) == 0 {
		err = errNoLocalPort
	}
	if err != nil {
		session.stop()
		return nil, fmt.Errorf("reading local port of port-forward to pod %s/%s: %w", s.namespace, podName, err)
	}

	session.addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0].Local)))
	return session, nil
}

// dialer prefers SPDY over websockets, falling back to plain SPDY as kubectl does.
//
// It speaks apimachinery's httpstream, which staticcheck reports as deprecated in
// favor of k8s.io/streaming. That is the Dialer both spdy.NewDialer and
// portforward.NewFallbackDialer take, so it is not ours to choose.
func (s *podForwardStarter) dialer(requestURL *url.URL) (httpstream.Dialer, error) {
	transport, upgrader, err := spdy.RoundTripperFor(s.restConfig)
	if err != nil {
		return nil, fmt.Errorf("building SPDY round tripper: %w", err)
	}
	spdyDialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, requestURL)

	websocketDialer, err := portforward.NewSPDYOverWebsocketDialer(requestURL, s.restConfig)
	if err != nil {
		return nil, fmt.Errorf("building websocket dialer: %w", err)
	}

	return portforward.NewFallbackDialer(websocketDialer, spdyDialer, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	}), nil
}

// resolve picks a ready pod behind the Service and the pod-side port its
// selected port targets, on every reconnect, so a lost pod lands its heir.
func (s *podForwardStarter) resolve(ctx context.Context) (string, int32, error) {
	service, err := s.clientset.CoreV1().Services(s.namespace).Get(ctx, s.service, metav1.GetOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("getting Service %s/%s: %w", s.namespace, s.service, err)
	}
	if len(service.Spec.Selector) == 0 {
		return "", 0, fmt.Errorf("service %s/%s has no selector, so it has no pods to forward to", s.namespace, s.service)
	}

	servicePort, err := selectServicePort(service, s.port)
	if err != nil {
		return "", 0, err
	}

	pods, err := s.clientset.CoreV1().Pods(s.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(service.Spec.Selector).String(),
	})
	if err != nil {
		return "", 0, fmt.Errorf("listing pods for Service %s/%s: %w", s.namespace, s.service, err)
	}

	pod := pickReadyPod(pods.Items)
	if pod == nil {
		return "", 0, fmt.Errorf("no ready pod backs Service %s/%s", s.namespace, s.service)
	}

	podPort, err := resolveTargetPort(pod, servicePort)
	if err != nil {
		return "", 0, err
	}
	return pod.Name, podPort, nil
}

// selectServicePort finds the Service port named or numbered port; an empty
// one takes the only port of a single-port Service, a convenience for tests.
func selectServicePort(service *corev1.Service, port string) (*corev1.ServicePort, error) {
	if len(service.Spec.Ports) == 1 && port == "" {
		return &service.Spec.Ports[0], nil
	}
	number, parseErr := strconv.ParseInt(port, 10, 32)
	for i := range service.Spec.Ports {
		candidate := &service.Spec.Ports[i]
		if candidate.Name == port {
			return candidate, nil
		}
		if parseErr == nil && number == int64(candidate.Port) {
			return candidate, nil
		}
	}
	return nil, fmt.Errorf("service %s/%s has no port %q", service.Namespace, service.Name, port)
}

// pickReadyPod returns the alphabetically first ready pod, for stability.
func pickReadyPod(pods []corev1.Pod) *corev1.Pod {
	var first *corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if !podIsReady(pod) {
			continue
		}
		if first == nil || pod.Name < first.Name {
			first = pod
		}
	}
	return first
}

func podIsReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// resolveTargetPort maps a targetPort onto a container port number in pod. A
// named one, which is what the NVKE chart uses, is looked up in the pod's ports.
func resolveTargetPort(pod *corev1.Pod, servicePort *corev1.ServicePort) (int32, error) {
	target := servicePort.TargetPort
	if target.Type == intstr.Int {
		if value := target.IntValue(); value != 0 {
			// IntValue widens a stored int32, so narrowing back cannot overflow.
			return int32(value), nil //nolint:gosec // int32 round-trip
		}
		// An unset targetPort defaults to the Service port.
		return servicePort.Port, nil
	}
	for _, container := range pod.Spec.Containers {
		for _, containerPort := range container.Ports {
			if containerPort.Name == target.StrVal {
				return containerPort.ContainerPort, nil
			}
		}
	}
	return 0, fmt.Errorf("pod %s/%s has no container port named %q", pod.Namespace, pod.Name, target.StrVal)
}
