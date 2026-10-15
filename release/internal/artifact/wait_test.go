// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cdn answers 200 for the paths given and 403 for everything else, which is how
// the private bucket reports an object that is not there.
func cdn(t *testing.T, present ...string) *httptest.Server {
	t.Helper()
	ok := map[string]bool{}
	for _, p := range present {
		ok[p] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok[r.URL.Path] {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWaitForPassesWhenEverythingIsPublished(t *testing.T) {
	srv := cdn(t, "/a.qcow2", "/b.qcow2")
	var out bytes.Buffer

	err := Checker{}.WaitFor(context.Background(), &out, []Location{
		{URL: srv.URL + "/a.qcow2", Wanted: []string{"os-image-amd64"}},
		{URL: srv.URL + "/b.qcow2", Wanted: []string{"os-image-arm64"}},
	}, 0, DefaultPoll)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "all 2 artifacts") {
		t.Errorf("output does not say everything is published:\n%s", out.String())
	}
}

// Without a wait, a run that resolved a release fails at once and names what is
// missing, because those artifacts were published when the release was cut.
func TestWaitForFailsFastWithNoBudget(t *testing.T) {
	srv := cdn(t, "/a.qcow2")
	var out bytes.Buffer

	start := time.Now()
	err := Checker{}.WaitFor(context.Background(), &out, []Location{
		{URL: srv.URL + "/a.qcow2", Wanted: []string{"os-image-amd64"}},
		{URL: srv.URL + "/gone.qcow2", Wanted: []string{"os-image-arm64"}},
	}, 0, DefaultPoll)

	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v, want it to fail without waiting", took)
	}
	for _, want := range []string{"1 of the 2 artifacts", "are not published", "gone.qcow2", "os-image-arm64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%s", want, err)
		}
	}
	// The one that is there must not be reported as missing.
	if strings.Contains(err.Error(), "/a.qcow2") {
		t.Errorf("error names an artifact that is published:\n%s", err)
	}
}

// An artifact that is still building answers once it is pushed, and the wait is
// what lets a beta be qualified by the tag push that started the publishers.
func TestWaitForWaitsForOneThatArrives(t *testing.T) {
	var pushed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !pushed.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// A short interval, because this test is about waiting and not about how
	// long the command waits in a run.
	const every = 20 * time.Millisecond

	// Published while the first wait is in flight.
	time.AfterFunc(every/2, func() { pushed.Store(true) })

	var out bytes.Buffer
	err := Checker{}.WaitFor(context.Background(), &out,
		[]Location{{URL: srv.URL + "/late.qcow2", Wanted: []string{"os-image-amd64"}}}, 10*every, every)
	if err != nil {
		t.Fatalf("the artifact arrived within the budget: %v", err)
	}
	if !strings.Contains(out.String(), "not published yet") {
		t.Errorf("output does not show it waited:\n%s", out.String())
	}
}

// The budget bounds the wait, so a publisher that never finishes fails the run
// rather than holding it open.
func TestWaitForGivesUpAtTheBudget(t *testing.T) {
	srv := cdn(t)
	var out bytes.Buffer

	err := Checker{}.WaitFor(context.Background(), &out,
		[]Location{{URL: srv.URL + "/never.qcow2", Wanted: []string{"controller"}}},
		50*time.Millisecond, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), "were still not published after") {
		t.Errorf("error does not say the budget ran out:\n%s", err)
	}
}

// A credential the registry rejects is not a publisher that has not finished,
// so the wait ends on it rather than re-reading the same answer until the
// budget runs out.
func TestWaitForStopsOnASettledFailure(t *testing.T) {
	// TLS, because ociExists reaches a registry over https. A plain server
	// fails to connect instead, which is a transient answer and would be
	// waited on, so the test would prove the opposite of what it means to.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "https://")

	var out bytes.Buffer
	start := time.Now()
	err := Checker{Client: srv.Client()}.WaitFor(context.Background(), &out,
		[]Location{{URL: "oci://" + host + "/org/thing:v1", Wanted: []string{"thing"}}},
		time.Hour, time.Minute)

	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), "waiting will not change that") {
		t.Errorf("err = %v, want it to say waiting does not help", err)
	}
	// The budget is an hour and the interval a minute, so anything that waited
	// would still be sleeping.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("took %v, want it to give up at once", elapsed)
	}
	if strings.Contains(out.String(), "asking again") {
		t.Errorf("it announced a retry:\n%s", out.String())
	}
}
