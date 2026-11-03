// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	strictjson "sigs.k8s.io/json"
)

type route struct {
	op     Operation
	method string
	suffix string
}

const itemPathSuffix = "/{name}"

// resourceRoutes maps CRUDL operations to collection and item HTTP routes.
var resourceRoutes = [...]route{
	{OpCreate, http.MethodPost, ""},
	{OpList, http.MethodGet, ""},
	{OpGet, http.MethodGet, itemPathSuffix},
	{OpUpdate, http.MethodPut, itemPathSuffix},
	{OpDelete, http.MethodDelete, itemPathSuffix},
}

// Register installs supported operations and JSON 405 fallbacks. Wrap the completed mux with WithLimits.
// Omitting verbs enables no operations. The server also enforces its registered operations.
func Register[E any](mux *http.ServeMux, path string, server ResourceServer[E], verbs ...Operation) {
	var collection, item []string
	for _, route := range resourceRoutes {
		if slices.Contains(verbs, route.op) {
			pattern := route.method + " " + path + route.suffix
			mux.Handle(pattern, operationHandler[E]{server: server, op: route.op})
			if route.op == OpCreate || route.op == OpList {
				collection = append(collection, route.method)
			} else {
				item = append(item, route.method)
			}
		}
	}
	mux.Handle(path, methodNotAllowedHandler{allow: strings.Join(collection, ", ")})
	mux.Handle(path+itemPathSuffix, methodNotAllowedHandler{allow: strings.Join(item, ", ")})
}

type methodNotAllowedHandler struct {
	allow string
}

// ServeHTTP returns a JSON 405 response with the allowed methods.
func (h methodNotAllowedHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", h.allow)
	WriteError(w, problem(http.StatusMethodNotAllowed, methodNotAllowedCode, "The operation is not supported."))
}

type operationHandler[E any] struct {
	server ResourceServer[E]
	op     Operation
}

// ServeHTTP dispatches one operation and writes its public response and ETag.
func (h operationHandler[E]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	opts := &RequestOptions{}
	value, status, err := h.dispatch(r.WithContext(WithOptions(r.Context(), opts)))
	switch {
	case err != nil:
		WriteError(w, err)
	case value == nil:
		w.WriteHeader(status)
	default:
		if opts.ETag != "" {
			w.Header().Set("ETag", strconv.Quote(opts.ETag))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
}

// dispatch parses the transport contract and invokes the registered CRUDL operation.
func (h operationHandler[E]) dispatch(r *http.Request) (any, int, error) {
	var allowed []string
	switch h.op {
	case OpList:
		allowed = []string{pageSizeParameter, pageKeyParameter}
	case OpDelete:
		allowed = []string{propagationPolicyParameter}
	}
	values, err := query(r, allowed...)
	if err != nil {
		return nil, 0, err
	}
	if h.op != OpCreate && h.op != OpUpdate {
		if err = noBody(r); err != nil {
			return nil, 0, err
		}
	}
	if h.op == OpUpdate || h.op == OpDelete {
		requestOptions(r.Context()).IfMatch, err = versionHeader(r)
		if err != nil {
			return nil, 0, err
		}
	}
	switch h.op {
	case OpCreate, OpUpdate:
		in, err := decode[E](r)
		if err != nil {
			return nil, 0, err
		}
		if h.op == OpCreate {
			value, err := h.server.Create(r.Context(), in)
			return value, http.StatusCreated, err
		}
		value, err := h.server.Update(r.Context(), r.PathValue("name"), in)
		return value, http.StatusOK, err
	case OpGet:
		value, err := h.server.Get(r.Context(), r.PathValue("name"))
		return value, http.StatusOK, err
	case OpList:
		opts := ListOptions{PageKey: values.Get(pageKeyParameter)}
		if values.Has(pageSizeParameter) {
			opts.PageSize, err = strconv.Atoi(values.Get(pageSizeParameter))
			if err != nil || opts.PageSize == 0 {
				return nil, 0, Invalid()
			}
		}
		value, err := h.server.List(r.Context(), opts)
		return value, http.StatusOK, err
	case OpDelete:
		err = h.server.Delete(r.Context(), r.PathValue("name"), DeleteOptions{
			PropagationPolicy: values.Get(propagationPolicyParameter),
		})
		return nil, http.StatusAccepted, err
	default:
		return nil, 0, problem(http.StatusInternalServerError, internalErrorCode, "The request could not be completed.")
	}
}

// WithLimits applies request deadlines, a body size limit, and no-store responses to a complete surface.
// Zero options use a 1 MiB body limit and a 30-second deadline. Longer deadlines are capped at 30 seconds.
// Writers must support read deadlines directly or expose their underlying writer through Unwrap.
// Cancellation is cooperative and does not emit a timeout response.
// The hosting http.Server still needs transport timeouts for work outside this handler, such as reading headers.
func WithLimits(next http.Handler, opts ServerOptions) http.Handler {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 20
	}
	if opts.RequestTimeout <= 0 || opts.RequestTimeout > requestTimeout {
		opts.RequestTimeout = requestTimeout
	}
	return limitsHandler{next: next, opts: opts}
}

type limitsHandler struct {
	next http.Handler
	opts ServerOptions
}

// ServeHTTP sets the request deadline, bounds body reads, and disables response caching.
func (h limitsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.opts.RequestTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	err := http.NewResponseController(w).SetReadDeadline(deadline)
	if err != nil {
		WriteError(w, problem(
			http.StatusInternalServerError, deadlineFailedCode, "The request deadline could not be established.",
		))
		return
	}
	// net/http resets this for the next request. Do not unbound an unread
	// timed-out body while the server is finishing the current request.
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, h.opts.MaxBodyBytes)
	h.next.ServeHTTP(w, r.WithContext(ctx))
}

// WriteError writes public Error diagnostics and replaces unrecognized errors with a generic 500 response.
func WriteError(w http.ResponseWriter, err error) {
	var public *Error
	if !errors.As(err, &public) {
		public = problem(http.StatusInternalServerError, internalErrorCode, "The request could not be completed.")
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(public.Status)
	_ = json.NewEncoder(w).Encode(public)
}

// decode checks the media type and maps strict JSON, size, and read failures to public errors.
func decode[E any](r *http.Request) (E, error) {
	var value E
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return value, problem(http.StatusUnsupportedMediaType, unsupportedMediaTypeCode, "Use application/json.")
	}
	body, err := decodeStrict(r.Body, &value)
	if err != nil {
		if timeout, ok := errors.AsType[net.Error](err); ok && timeout.Timeout() {
			r.Close = true
			return value, problem(http.StatusRequestTimeout, requestTimeoutCode, "The request body deadline expired.")
		}
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return value, problem(http.StatusRequestEntityTooLarge, bodyTooLargeCode, "The request body exceeds the limit.")
		}
		return value, Invalid(FieldError{
			Code: invalidJSONCode, Message: "Use one JSON object with documented fields and unique keys.",
		})
	}
	requestOptions(r.Context()).body = body
	return value, nil
}

// decodeStrict accepts one object with known fields and unique keys, preserving original field presence.
// The caller must bound the reader before this function buffers its contents.
func decodeStrict(reader io.Reader, dest any) (map[string]any, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("reading JSON: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return nil, fmt.Errorf("JSON object required")
	}
	var body map[string]any
	for _, target := range []any{&body, dest} {
		strictErrors, decodeErr := strictjson.UnmarshalStrict(data, target)
		if decodeErr != nil || len(strictErrors) != 0 {
			return nil, fmt.Errorf("invalid, unknown or duplicate JSON fields")
		}
	}
	return body, nil
}

// query permits only the named parameters, each appearing once.
func query(r *http.Request, allowed ...string) (url.Values, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, Invalid()
	}
	for key, entries := range values {
		if !slices.Contains(allowed, key) || len(entries) != 1 {
			return nil, Invalid(FieldError{
				Code: invalidQueryCode, Message: "The query contains an unsupported or repeated parameter.",
			})
		}
	}
	return values, nil
}

// versionHeader extracts one quoted If-Match resourceVersion. The ResourceServer enforces its presence and validity.
func versionHeader(r *http.Request) (string, error) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || len(values[0]) < 3 || values[0][0] != '"' || values[0][len(values[0])-1] != '"' {
		return "", Invalid(FieldError{Code: invalidPreconditionCode, Message: "Use one quoted resource version."})
	}
	value := values[0][1 : len(values[0])-1]
	if strings.ContainsAny(value, "\" ,*") {
		return "", Invalid(FieldError{Code: invalidPreconditionCode, Message: "Use one quoted resource version."})
	}
	return value, nil
}

// noBody rejects any body content without reading beyond its first byte.
func noBody(r *http.Request) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(data) != 0 {
		return Invalid(FieldError{Code: unexpectedBodyCode, Message: "This operation does not accept a request body."})
	}
	return nil
}
