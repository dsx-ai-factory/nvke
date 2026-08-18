// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

func newAPIProxy(apiBaseURL *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			rewriteAPIProxyRequest(r, apiBaseURL)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("Error proxying request to backing API: %v", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
		},
	}
}

func rewriteAPIProxyRequest(r *httputil.ProxyRequest, apiBaseURL *url.URL) {
	r.SetURL(apiBaseURL)
	r.Out.URL.Path = strings.TrimPrefix(r.Out.URL.Path, "/api")
	r.Out.URL.RawPath = ""
	r.Out.URL.RawQuery = r.In.URL.RawQuery
}

// Serve the UI locally, and proxy API requests to the backing API at the address specified in the API_BASE_URL env var.
// Useful when developing the UI locally while using the API of a remote CWE instance.
// sanitizeForLog strips CR/LF from operator-supplied values so they cannot forge
// additional log entries.
func sanitizeForLog(v string) string {
	return strings.NewReplacer("\n", "", "\r", "").Replace(v)
}

func main() {
	uiPort := os.Getenv("UI_PORT")
	if uiPort == "" {
		uiPort = "8081"
	}

	apiBaseURL := os.Getenv("API_BASE_URL")
	if apiBaseURL == "" {
		log.Fatal("API_BASE_URL environment variable is not set")
	}
	parsedAPIBaseURL, err := url.Parse(apiBaseURL)
	if err != nil {
		log.Fatalf("Error parsing API_BASE_URL: %v", err)
	}

	m := http.NewServeMux()
	m.Handle("/api/", newAPIProxy(parsedAPIBaseURL))

	m.Handle("/", http.FileServer(http.Dir("./ui")))

	srv := &http.Server{
		Addr:              "127.0.0.1:" + uiPort,
		Handler:           m,
		ReadHeaderTimeout: 5 * time.Second,
	}

	//nolint:gosec // G706: values are passed through sanitizeForLog, which strips CR/LF; gosec's taint analysis does not model the sanitizer.
	log.Printf("Serving UI at http://%s and proxying API requests to %s", sanitizeForLog(srv.Addr), sanitizeForLog(apiBaseURL))

	log.Fatal(srv.ListenAndServe())
}
