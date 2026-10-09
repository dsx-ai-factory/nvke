// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nke/controller/api"
)

// doerFunc serves a canned response, as a controller of one vintage or the
// other would.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func jsonResponse(body string) api.HttpRequestDoer {
	return doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)),
		}, nil
	})
}

// throughCompat returns what a caller of the generated client would read:
// the body after the shim has had it, and the length the response claims.
func throughCompat(t *testing.T, body string) ([]byte, int64) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://controller/api/nodePools", http.NoBody)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	resp, err := labelShapeCompat{next: jsonResponse(body)}.Do(req)
	if err != nil {
		t.Fatalf("compat doer: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return got, resp.ContentLength
}

// TestCompatDecodesBothControllerVintages is the whole point: the generated
// client must decode a node-pool list from a controller on either side of
// #1306, because the lane qualifies releases from both.
func TestCompatDecodesBothControllerVintages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		want  map[string]string
		pool  string
		isMap bool
	}{
		{
			name: "before #1306, labels are a list",
			body: `{"nodePools":[{"name":"workers","nodeLabels":["a=1","b=2"]}]}`,
			want: map[string]string{"a": "1", "b": "2"},
			pool: "workers",
		},
		{
			name: "before #1306, no labels is still a list",
			body: `{"nodePools":[{"name":"workers","nodeLabels":[]}]}`,
			want: map[string]string{},
			pool: "workers",
		},
		{
			name: "from #1306, labels are already an object",
			body: `{"nodePools":[{"name":"workers","nodeLabels":{"a":"1"}}]}`,
			want: map[string]string{"a": "1"},
			pool: "workers",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, contentLength := throughCompat(t, tc.body)

			var decoded api.ListNodePoolsResponse
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("the generated client cannot decode this: %v", err)
			}
			if len(decoded.NodePools) != 1 || decoded.NodePools[0].Name != tc.pool {
				t.Fatalf("decoded %+v, want the single pool %q", decoded.NodePools, tc.pool)
			}
			got := decoded.NodePools[0].NodeLabels
			if len(got) != len(tc.want) {
				t.Fatalf("labels = %v, want %v", got, tc.want)
			}
			for key, value := range tc.want {
				if got[key] != value {
					t.Errorf("label %q = %q, want %q", key, got[key], value)
				}
			}

			// A rewritten body that still announces the length of the
			// original is a body the caller reads short.
			if contentLength != int64(len(body)) {
				t.Errorf("ContentLength = %d, body is %d bytes", contentLength, len(body))
			}
		})
	}
}

// TestCompatLeavesEverythingElseAlone: the rewrite is keyed on a field name,
// so it has to be shown not to touch a same-named field of another shape, or
// a body it cannot parse.
func TestCompatLeavesEverythingElseAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"a list of things that are not labels", `{"nodePools":[{"nodeLabels":[1,2]}]}`},
		{"labels that are not a list at all", `{"nodePools":[{"nodeLabels":"a=1"}]}`},
		{"a body that is not JSON", `<html>502</html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := throughCompat(t, tc.body)
			if string(got) != tc.body {
				t.Errorf("body = %s, want it unchanged: %s", got, tc.body)
			}
		})
	}
}

// TestCompatReachesLabelsAtAnyDepth guards the recursive walk: nodeLabels
// also hangs off control-plane config inside a cluster response, not just off
// the node pools of a list.
func TestCompatReachesLabelsAtAnyDepth(t *testing.T) {
	body, _ := throughCompat(t, `{"cluster":{"controlPlane":{"config":{"nodeLabels":["role=cp"]}}}}`)
	if !strings.Contains(string(body), `{"role":"cp"}`) {
		t.Errorf("nested labels not reshaped: %s", body)
	}
}

// TestSchemaMismatchIsOnlyTheDisagreement keeps the fail-fast narrow: it has
// to catch what the lane actually hit and nothing a retry could clear.
func TestSchemaMismatchIsOnlyTheDisagreement(t *testing.T) {
	// Verbatim from the decode the readiness spec retried for 45 minutes in
	// run 37854934923, produced rather than asserted so it stays true.
	var labelsAsMap struct {
		NodeLabels map[string]string `json:"nodeLabels"`
	}
	decodeErr := json.Unmarshal([]byte(`{"nodeLabels":[]}`), &labelsAsMap)
	if decodeErr == nil {
		t.Fatal("expected the list-into-map decode to fail")
	}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"the shape disagreement", decodeErr, true},
		{"it, wrapped the way the client wraps it", fmt.Errorf("listing node pools: %w", decodeErr), true},
		// A response cut short is worth another poll; only a type the client
		// cannot hold says the two will never agree.
		{"truncated JSON", json.Unmarshal([]byte(`{"nodePools":`), &labelsAsMap), false},
		{"a node that is not ready yet", fmt.Errorf("node w-1 is not KubernetesReady"), false},
		{"nothing wrong", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SchemaMismatch(tc.err); got != tc.want {
				t.Errorf("SchemaMismatch(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
