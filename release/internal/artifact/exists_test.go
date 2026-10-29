// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExistsOverHTTP(t *testing.T) {
	// The bucket answers 403 for an object that is not there, because it is
	// private and does not list.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/there.qcow2" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	t.Run("it is there", func(t *testing.T) {
		if err := Exists(context.Background(), srv.URL+"/there.qcow2"); err != nil {
			t.Errorf("the URL answers 200: %v", err)
		}
	})

	t.Run("a 403 names both explanations", func(t *testing.T) {
		err := Exists(context.Background(), srv.URL+"/gone.qcow2")
		if err == nil {
			t.Fatal("expected an error, got none")
		}
		for _, want := range []string{"could not confirm", "gone.qcow2", "HTTP 403", "a network that is not allowed"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not mention %q:\n%s", want, err)
			}
		}
	})

	// A status that is not 403 means neither of those things, so saying so
	// would send a reader looking for a network problem that is not there.
	t.Run("another status says only what it was", func(t *testing.T) {
		gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(gone.Close)

		err := Exists(context.Background(), gone.URL+"/x.qcow2")
		if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
			t.Fatalf("err = %v, want it to report the 404", err)
		}
		// The server's ephemeral port can contain "403", so match the explanation, not the number.
		if strings.Contains(err.Error(), "a network that is not allowed") {
			t.Errorf("a 404 should not carry the 403 explanation:\n%s", err)
		}
	})

	t.Run("an unreachable host is reported", func(t *testing.T) {
		err := Exists(context.Background(), "http://127.0.0.1:1/x")
		if err == nil || !strings.Contains(err.Error(), "could not confirm") {
			t.Errorf("err = %v, want it to say it could not confirm", err)
		}
	})
}

// A location this cannot check is an error and never a pass, because treating
// it as present would let a plan name something nothing ever looked for.
func TestExistsRejectsAnUnknownScheme(t *testing.T) {
	err := Exists(context.Background(), "ftp://host/x")
	if err == nil || !strings.Contains(err.Error(), "names no scheme this understands") {
		t.Errorf("err = %v, want it to reject the scheme", err)
	}
}

func TestIsScheme(t *testing.T) {
	for _, tc := range []struct {
		location string
		want     bool
	}{
		{"https://cdn.example/x.qcow2", true},
		{"http://cdn.example/x.qcow2", true},
		{"oci://nvcr.io/org/nvke-controller:v1", true},
		{"nvcr.io/org/nvke-controller:v1", false},
		{"cdn.example", false},
		{"", false},
	} {
		if got := IsScheme(tc.location); got != tc.want {
			t.Errorf("IsScheme(%q) = %v, want %v", tc.location, got, tc.want)
		}
	}
}
