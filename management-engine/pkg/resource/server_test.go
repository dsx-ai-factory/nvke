// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// deadlineRecorder records read-deadline requests without enforcing timeouts.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
	err      error
}

// SetReadDeadline records the requested deadline and returns the configured error.
func (w *deadlineRecorder) SetReadDeadline(deadline time.Time) error {
	w.deadline = deadline
	return w.err
}

func TestSurfaceLimitsAcrossRoutes(t *testing.T) {
	mux := http.NewServeMux()
	client, err := New[widget, internalWidget](fixtureConfig(nil), widgetInfo(OpCreate))
	require.NoError(t, err)
	for _, path := range []string{"/widgets", "/other-widgets"} {
		Register(mux, path, client, OpCreate)
	}
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"openapi": "3.1.0"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, problem(http.StatusNotFound, "notFound", "The route was not found."))
	})
	var deadline time.Time
	handler := WithLimits(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, _ = r.Context().Deadline()
		mux.ServeHTTP(w, r)
	}), ServerOptions{MaxBodyBytes: 32, RequestTimeout: time.Second})
	for _, tc := range []struct {
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{
			http.MethodPost, "/widgets", `{"value":"` + strings.Repeat("x", 64) + `"}`,
			http.StatusRequestEntityTooLarge, "bodyTooLarge",
		},
		{
			http.MethodPost, "/other-widgets", `{"value":"` + strings.Repeat("x", 64) + `"}`,
			http.StatusRequestEntityTooLarge, "bodyTooLarge",
		},
		{http.MethodGet, "/widgets", "", http.StatusMethodNotAllowed, "methodNotAllowed"},
		{http.MethodGet, "/openapi.json", "", http.StatusOK, ""},
		{http.MethodGet, "/missing", "", http.StatusNotFound, "notFound"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			deadline = time.Time{}
			handler.ServeHTTP(response, req)
			require.False(t, deadline.IsZero(), "the shared boundary covers every route")
			require.Equal(t, deadline, response.deadline)
			require.Positive(t, time.Until(deadline))
			require.LessOrEqual(t, time.Until(deadline), time.Second)
			require.Equal(t, tc.status, response.Code)
			require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			if tc.code != "" {
				var problem Error
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
				require.Equal(t, tc.code, problem.Code)
			}
			if tc.status == http.StatusMethodNotAllowed {
				require.Equal(t, "POST", response.Header().Get("Allow"))
				require.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
			}
		})
	}
}

func TestReadDeadlineFailure(t *testing.T) {
	for name, readError := range map[string]error{
		"unsupported writer": nil,
		"deadline failure":   fmt.Errorf("injected deadline failure"),
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			var writer http.ResponseWriter = response
			if readError != nil {
				writer = &deadlineRecorder{ResponseRecorder: response, err: readError}
			}
			handler := WithLimits(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("a failed read deadline must not dispatch the request")
			}), ServerOptions{})
			handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/widgets", nil))
			require.Equal(t, http.StatusInternalServerError, response.Code)
			var problem Error
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
			require.Equal(t, "deadlineFailed", problem.Code)
		})
	}
}

func TestUnknownOperation(t *testing.T) {
	response := requestHTTP(operationHandler[widget]{op: "unknown"}, t.Context(), http.MethodGet, "/widgets", "", "")
	require.Equal(t, http.StatusInternalServerError, response.Code)
	var problem Error
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	require.Equal(t, "internalError", problem.Code)
}

func requestHTTP(
	handler http.Handler,
	ctx context.Context,
	method string,
	path string,
	body string,
	etag string,
) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	if etag != "" {
		r.Header.Set("If-Match", etag)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(&deadlineRecorder{ResponseRecorder: response}, r)
	return response
}

func TestHTTPWriteBoundary(t *testing.T) {
	var applied map[string]any
	client, err := New[widget, internalWidget](fixtureConfig(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodPatch, r.Method)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&applied))
		metadata := applied["metadata"].(map[string]any)
		metadata["uid"], metadata["generation"] = "widget-uid", 1
		metadata["resourceVersion"] = "1"
		return jsonResponse(r, 200, applied)
	})), widgetInfo(OpCreate))
	require.NoError(t, err)
	mux := http.NewServeMux()
	Register(mux, "/widgets", client, OpCreate)
	handler := WithLimits(mux, ServerOptions{})
	for _, body := range []string{
		`{"metadata":{"name":"x","namespace":"other"},"spec":{"value":"x"}}`,
		`{"metadata":{"name":"x","uid":"injected"},"spec":{"value":"x"}}`,
		`{"metadata":{"name":"x"},"spec":{"value":"x","hidden":"private-default"}}`,
		`{"metadata":{"name":"x"},"spec":{"value":"x"},"status":{"counter":0}}`,
		`{"metadata":{"name":"x"},"spec":{"value":"x","value":"y"}}`,
		`{"metadata":{"name":"x"},"spec":{"Value":"x"}}`,
		`{"metadata":{"name":"x"},"spec":null}`,
		`null`, `[]`, `{}`, `{} {}`, `{} trailing`,
	} {
		t.Run(body, func(t *testing.T) {
			response := requestHTTP(handler, t.Context(), "POST", "/widgets", body, "")
			require.Equal(t, 400, response.Code, response.Body.String())
			require.Nil(t, applied, "invalid input must not reach the datastore")
		})
	}
	response := requestHTTP(handler, t.Context(), "POST", "/widgets",
		`{"metadata":{"name":"x"},"spec":{"value":"x","count":0,"enabled":false,"items":[]}}`, "")
	require.Equal(t, 201, response.Code, response.Body.String())
	require.Equal(t, map[string]any{
		"value": "x", "count": float64(0), "enabled": false, "items": []any{},
	}, applied["spec"])
	require.Equal(t, `"1"`, response.Header().Get("ETag"))
}

func TestSlowBodyDeadlineAndKeepalive(t *testing.T) {
	mux := http.NewServeMux()
	client, err := New[widget, internalWidget](fixtureConfig(nil), widgetInfo(OpCreate))
	require.NoError(t, err)
	Register(mux, "/widgets", client, OpCreate)
	server := httptest.NewServer(WithLimits(mux, ServerOptions{RequestTimeout: 50 * time.Millisecond}))
	defer server.Close()
	// A separate test deadline prevents a broken server-side deadline from hanging this raw TCP probe.
	dial := func() net.Conn {
		t.Helper()
		connection, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", server.Listener.Addr().String())
		require.NoError(t, err)
		require.NoError(t, connection.SetDeadline(time.Now().Add(3*time.Second)))
		t.Cleanup(func() { _ = connection.Close() })
		return connection
	}
	// Drain each response so the next read tests the same keepalive connection.
	read := func(reader *bufio.Reader, status int) {
		t.Helper()
		response, err := http.ReadResponse(reader, nil)
		require.NoError(t, err)
		require.Equal(t, status, response.StatusCode)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
	}
	slow := dial()
	start := time.Now()
	_, err = fmt.Fprint(slow, "POST /widgets HTTP/1.1\r\nHost: test\r\n"+
		"Content-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
	require.NoError(t, err)
	read(bufio.NewReader(slow), 408)
	require.Less(t, time.Since(start), time.Second)
	connection := dial()
	reader := bufio.NewReader(connection)
	for index := range 2 {
		if index == 1 {
			time.Sleep(100 * time.Millisecond)
		}
		_, err = fmt.Fprint(connection, "POST /widgets HTTP/1.1\r\nHost: test\r\n"+
			"Content-Type: application/json\r\nContent-Length: 13\r\n\r\n{\"unknown\":1}")
		require.NoError(t, err)
		read(reader, 400)
	}
}
