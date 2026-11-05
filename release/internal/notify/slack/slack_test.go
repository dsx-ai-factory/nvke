// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nvke/release/internal/notify"
)

// server answers each request with the next of replies, and records them.
type server struct {
	*httptest.Server
	calls    atomic.Int32
	requests []*http.Request
	bodies   []map[string]any
}

type reply struct {
	status int
	header map[string]string
	body   string
}

func newServer(t *testing.T, replies ...reply) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(s.calls.Add(1)) - 1
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.requests, s.bodies = append(s.requests, r), append(s.bodies, body)
		rep := replies[min(i, len(replies)-1)]
		for k, v := range rep.header {
			w.Header().Set(k, v)
		}
		if rep.status != 0 {
			w.WriteHeader(rep.status)
		}
		_, _ = w.Write([]byte(rep.body))
	}))
	t.Cleanup(s.Close)
	return s
}

func client(s *server, slept *[]time.Duration) *Client {
	return &Client{
		Token:   "xoxb-test-token",
		BaseURL: s.URL,
		Sleep: func(_ context.Context, d time.Duration) error {
			if slept != nil {
				*slept = append(*slept, d)
			}
			return nil
		},
	}
}

var message = notify.Message{
	Channel:  "C0123456789",
	Title:    "Stream failed",
	Body:     "stage `bootstrap`",
	Severity: notify.SeverityError,
}

func TestPost(t *testing.T) {
	s := newServer(t, reply{body: `{"ok":true,"ts":"1700000000.000100"}`})
	m := message
	m.ThreadTS = "1700000000.000001"
	m.Mentions = []string{"<!subteam^S1|@oncall>"}

	ts, err := client(s, nil).Post(context.Background(), m)
	if err != nil || ts != "1700000000.000100" {
		t.Fatalf("Post = %q, %v", ts, err)
	}

	r := s.requests[0]
	if r.URL.Path != "/chat.postMessage" || r.Method != http.MethodPost {
		t.Errorf("request = %s %s", r.Method, r.URL.Path)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer xoxb-test-token" {
		t.Errorf("Authorization = %q", got)
	}
	b := s.bodies[0]
	if b["channel"] != "C0123456789" || b["thread_ts"] != "1700000000.000001" || b["text"] != "<!subteam^S1|@oncall>" {
		t.Errorf("body = %v", b)
	}
	att := b["attachments"].([]any)[0].(map[string]any)
	if att["title"] != "Stream failed" || att["color"] != "#e01e5a" {
		t.Errorf("attachment = %v", att)
	}
}

// A root message sends no thread_ts at all: an empty one is not "no thread" to
// Slack.
func TestPostRootOmitsThreadTS(t *testing.T) {
	s := newServer(t, reply{body: `{"ok":true,"ts":"1.2"}`})
	if _, err := client(s, nil).Post(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if _, present := s.bodies[0]["thread_ts"]; present {
		t.Error("thread_ts was sent for a root message")
	}
}

func TestPostRetriesARateLimitHonouringRetryAfter(t *testing.T) {
	s := newServer(t,
		reply{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "7"}},
		reply{body: `{"ok":true,"ts":"1.2"}`},
	)
	var slept []time.Duration
	ts, err := client(s, &slept).Post(context.Background(), message)
	if err != nil || ts != "1.2" {
		t.Fatalf("Post = %q, %v", ts, err)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Errorf("slept %v, want one wait of 7s", slept)
	}
}

func TestRetryAfterIsBounded(t *testing.T) {
	for header, want := range map[string]time.Duration{
		"3":    3 * time.Second,
		"9999": maxRetryAfter,
		"":     time.Second,
		"soon": time.Second,
		"-5":   time.Second,
		// Large enough that seconds * time.Second overflows int64: it used to come
		// out negative, which the caller reads as "do not retry".
		"10000000000":             maxRetryAfter,
		"9223372036854775807":     maxRetryAfter,
		"99999999999999999999999": maxRetryAfter,
		"-99999999999999999999":   time.Second,
	} {
		if got := retryAfter(header); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", header, got, want)
		}
	}
}

// Slack saying it did not act is safe to ask again, up to the attempt limit.
func TestPostRetriesWhenSlackDidNotAct(t *testing.T) {
	for name, rep := range map[string]reply{
		"503":                 {status: http.StatusServiceUnavailable},
		"429":                 {status: http.StatusTooManyRequests},
		"service_unavailable": {body: `{"ok":false,"error":"service_unavailable"}`},
		"request_timeout":     {body: `{"ok":false,"error":"request_timeout"}`},
		"ratelimited":         {body: `{"ok":false,"error":"ratelimited"}`},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, rep)
			if _, err := client(s, nil).Post(context.Background(), message); err == nil {
				t.Fatal("Post succeeded against a server that only fails")
			}
			if got := s.calls.Load(); got != defaultAttempts {
				t.Errorf("%d attempts, want %d", got, defaultAttempts)
			}
		})
	}
}

// chat.postMessage has no idempotency key, so an outcome that leaves it unknown
// whether Slack acted must not be retried: the retry would post twice.
func TestPostDoesNotRetryWhenDeliveryIsUncertain(t *testing.T) {
	for name, rep := range map[string]reply{
		"500":            {status: http.StatusInternalServerError},
		"502":            {status: http.StatusBadGateway},
		"504":            {status: http.StatusGatewayTimeout},
		"internal_error": {body: `{"ok":false,"error":"internal_error"}`},
		"fatal_error":    {body: `{"ok":false,"error":"fatal_error"}`},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, rep)
			_, err := client(s, nil).Post(context.Background(), message)
			if err == nil || !strings.Contains(err.Error(), "may have been delivered") {
				t.Fatalf("err = %v, want one saying the message may have been delivered", err)
			}
			if got := s.calls.Load(); got != 1 {
				t.Errorf("%d attempts, want 1", got)
			}
		})
	}
}

// The case that started this: Slack accepts the post and the response is lost.
// Asking again would post the message a second time.
func TestPostDoesNotRetryALostResponse(t *testing.T) {
	var accepted atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		accepted.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close() // the request arrived; no response ever does
	}))
	defer srv.Close()

	c := &Client{Token: "xoxb-test-token", BaseURL: srv.URL, Sleep: func(context.Context, time.Duration) error { return nil }}
	_, err := c.Post(context.Background(), message)
	if err == nil || !strings.Contains(err.Error(), "may have been delivered") {
		t.Fatalf("err = %v, want one saying the message may have been delivered", err)
	}
	if got := accepted.Load(); got != 1 {
		t.Errorf("the server received %d posts, want 1", got)
	}
}

// A failure to connect proves nothing was sent, so it is safe to try again.
func TestPostRetriesAConnectionThatNeverOpened(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens here any more: connection refused

	var slept []time.Duration
	c := &Client{Token: "xoxb-test-token", BaseURL: url, Sleep: func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}}
	if _, err := c.Post(context.Background(), message); err == nil {
		t.Fatal("Post succeeded with nothing listening")
	}
	if len(slept) != defaultAttempts-1 {
		t.Errorf("waited %d times, want %d: one before each retry", len(slept), defaultAttempts-1)
	}
}

// Slack saying no is final: asking again gets the same answer.
func TestPostDoesNotRetryARefusal(t *testing.T) {
	s := newServer(t, reply{body: `{"ok":false,"error":"not_in_channel"}`})
	_, err := client(s, nil).Post(context.Background(), message)
	if err == nil || !strings.Contains(err.Error(), "not_in_channel") || !strings.Contains(err.Error(), "invite the app") {
		t.Fatalf("err = %v, want the code and the hint", err)
	}
	if got := s.calls.Load(); got != 1 {
		t.Errorf("%d attempts, want 1", got)
	}
}

func TestPostRetriesRatelimitedInTheBody(t *testing.T) {
	s := newServer(t,
		reply{body: `{"ok":false,"error":"ratelimited"}`},
		reply{body: `{"ok":true,"ts":"1.2"}`},
	)
	if _, err := client(s, nil).Post(context.Background(), message); err != nil {
		t.Fatal(err)
	}
}

// A huge Retry-After used to overflow to a negative wait, which stopped the
// retry. It is bounded now, so the retry happens, after the longest wait.
func TestPostRetriesAfterAnOverflowingRetryAfter(t *testing.T) {
	s := newServer(t,
		reply{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "10000000000"}},
		reply{body: `{"ok":true,"ts":"1.2"}`},
	)
	var slept []time.Duration
	if _, err := client(s, &slept).Post(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != maxRetryAfter {
		t.Errorf("slept %v, want one wait of %v", slept, maxRetryAfter)
	}
}

// The token authenticates the request and must not come back in an error.
func TestErrorsNeverCarryTheToken(t *testing.T) {
	for _, rep := range []reply{
		{body: `{"ok":false,"error":"invalid_auth"}`},
		{status: http.StatusInternalServerError},
		{status: http.StatusTooManyRequests},
		{body: "not json"},
	} {
		s := newServer(t, rep)
		_, err := client(s, nil).Post(context.Background(), message)
		if err == nil {
			t.Fatal("Post succeeded")
		}
		if strings.Contains(err.Error(), "xoxb-test-token") {
			t.Errorf("error carries the token: %v", err)
		}
	}
}

func TestPostStopsWhenCancelled(t *testing.T) {
	s := newServer(t, reply{status: http.StatusTooManyRequests})
	ctx, cancel := context.WithCancel(context.Background())
	c := client(s, nil)
	c.Sleep = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	if _, err := c.Post(ctx, message); err == nil {
		t.Fatal("Post succeeded")
	}
	if got := s.calls.Load(); got != 1 {
		t.Errorf("%d attempts after cancellation, want 1", got)
	}
}
