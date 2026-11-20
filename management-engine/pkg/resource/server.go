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
	"reflect"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/fields"
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

// queryParameters is shared by HTTP dispatch and registration descriptions.
func queryParameters(op Operation, selectable, versionFilter bool) []string {
	var allowed []string
	switch op {
	case OpList:
		allowed = []string{pageSizeParameter, pageKeyParameter}
		if selectable {
			allowed = append(allowed, fieldSelectorParameter)
		}
	case OpDelete:
		allowed = []string{propagationPolicyParameter}
	}
	if versionFilter && (op == OpGet || op == OpList) {
		allowed = append(allowed, versionParameter)
	}
	return allowed
}

// Register installs supported operations and JSON 405 fallbacks. Wrap the completed mux with WithLimits.
// Omitting verbs or the corresponding reader/writer disables those operations.
// Fields are public JSON paths supported by the reader's equality filters.
func Register[E any](
	mux *http.ServeMux,
	path string,
	reader ResourceReader[E],
	writer ResourceWriter[E],
	selectorFields []string,
	naming Naming,
	verbs ...Operation,
) error {
	if err := naming.Validate(); err != nil {
		return err
	}
	seen := map[reflect.Type]*jsonShape{}
	shape, err := buildJSONShape(reflect.TypeFor[E](), naming, seen)
	if err != nil {
		return err
	}
	listShape, err := buildJSONShape(reflect.TypeFor[List[E]](), naming, seen)
	if err != nil {
		return err
	}
	selectors := make(map[string]string, len(selectorFields))
	for _, field := range selectorFields {
		wire, ok := shape.path(strings.Split(field, "."))
		if !ok {
			return fmt.Errorf("selectable field %q is not a declared public field", field)
		}
		selectors[strings.Join(wire, ".")] = field
	}
	var collection, item []string
	for _, route := range resourceRoutes {
		if slices.Contains(verbs, route.op) && ((route.op == OpGet || route.op == OpList) && reader != nil ||
			(route.op == OpCreate || route.op == OpUpdate || route.op == OpDelete) && writer != nil) {
			pattern := route.method + " " + path + route.suffix
			mux.Handle(pattern, operationHandler[E]{
				reader: reader, writer: writer, op: route.op,
				naming: naming, shape: shape, listShape: listShape, selectors: selectors,
			})
			if route.op == OpCreate || route.op == OpList {
				collection = append(collection, route.method)
			} else {
				item = append(item, route.method)
			}
		}
	}
	mux.Handle(path, methodNotAllowedHandler{allow: strings.Join(collection, ", "), naming: naming})
	mux.Handle(path+itemPathSuffix, methodNotAllowedHandler{allow: strings.Join(item, ", "), naming: naming})
	return nil
}

type methodNotAllowedHandler struct {
	allow  string
	naming Naming
}

// ServeHTTP returns a JSON 405 response with the allowed methods.
func (h methodNotAllowedHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", h.allow)
	WriteError(w, problem(http.StatusMethodNotAllowed, methodNotAllowedCode, "The operation is not supported."), h.naming)
}

type operationHandler[E any] struct {
	reader    ResourceReader[E]
	writer    ResourceWriter[E]
	op        Operation
	naming    Naming
	shape     *jsonShape
	listShape *jsonShape
	selectors map[string]string
}

type versionFilteredReader[E any] interface {
	WithVersionFilter(string) (ResourceReader[E], error)
}

// ServeHTTP dispatches one operation and writes its public response and ETag.
func (h operationHandler[E]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	opts := &RequestOptions{}
	value, status, err := h.dispatch(r.WithContext(WithOptions(r.Context(), opts)))
	var body []byte
	if err == nil && value != nil {
		body, err = h.responseJSON(value)
	}
	switch {
	case err != nil:
		writeError(w, err, h.naming, h.shape)
	case value == nil:
		w.WriteHeader(status)
	default:
		if opts.ETag != "" {
			w.Header().Set("ETag", strconv.Quote(opts.ETag))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(append(body, '\n'))
	}
}

func (h operationHandler[E]) responseJSON(value any) ([]byte, error) {
	shape := h.shape
	if h.op == OpList {
		shape = h.listShape
	}
	return responseJSON(value, h.naming, shape)
}

// responseJSON applies the selected field naming to any declared response envelope.
func responseJSON(value any, naming Naming, shape *jsonShape) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil || naming == CamelCase {
		return data, err
	}
	return shape.translate(data, false)
}

// dispatch parses the transport contract and invokes the registered CRUDL operation.
func (h operationHandler[E]) dispatch(r *http.Request) (any, int, error) {
	versionReader, supportsVersionFilter := h.reader.(versionFilteredReader[E])
	values, err := query(r, h.naming, queryParameters(h.op, len(h.selectors) != 0, supportsVersionFilter)...)
	if err != nil {
		return nil, 0, err
	}
	if values.Has(versionParameter) {
		h.reader, err = versionReader.WithVersionFilter(values.Get(versionParameter))
		if err != nil {
			return nil, 0, err
		}
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
		in, err := decode[E](r, h.shape)
		if err != nil {
			return nil, 0, err
		}
		if h.op == OpCreate {
			value, err := h.writer.Create(r.Context(), in)
			return value, http.StatusCreated, err
		}
		value, err := h.writer.Update(r.Context(), r.PathValue("name"), in)
		return value, http.StatusOK, err
	case OpGet:
		value, err := h.reader.Get(r.Context(), r.PathValue("name"))
		return value, http.StatusOK, err
	case OpList:
		opts := ListOptions{PageKey: values.Get(pageKeyParameter), FieldSelector: values.Get(fieldSelectorParameter)}
		if values.Has(fieldSelectorParameter) && opts.FieldSelector == "" {
			return nil, 0, invalidSelector()
		}
		if opts.FieldSelector != "" {
			opts.FieldSelector, err = h.canonicalSelector(opts.FieldSelector)
			if err != nil {
				return nil, 0, err
			}
		}
		if values.Has(pageSizeParameter) {
			opts.PageSize, err = strconv.Atoi(values.Get(pageSizeParameter))
			if err != nil || opts.PageSize == 0 {
				return nil, 0, Invalid()
			}
		}
		value, err := h.reader.List(r.Context(), opts)
		return value, http.StatusOK, err
	case OpDelete:
		err = h.writer.Delete(r.Context(), r.PathValue("name"), DeleteOptions{
			PropagationPolicy: values.Get(propagationPolicyParameter),
		})
		return nil, http.StatusAccepted, err
	default:
		return nil, 0, problem(http.StatusInternalServerError, internalErrorCode, "The request could not be completed.")
	}
}

func (h operationHandler[E]) canonicalSelector(value string) (string, error) {
	if len(value) > MaxFieldSelectorBytes {
		return "", invalidSelector()
	}
	selector, err := fields.ParseSelector(value)
	if err != nil || selector.Empty() {
		return "", invalidSelector()
	}
	selector, err = selector.Transform(func(name, value string) (string, string, error) {
		canonical, ok := h.selectors[name]
		if !ok {
			return "", "", invalidSelector()
		}
		return canonical, value, nil
	})
	if err != nil {
		return "", err
	}
	return selector.String(), nil
}

// WithLimits applies body limits, cooperative deadlines and no-store responses.
// Zero options use DefaultMaxBodyBytes and MaxRequestTimeout. Longer deadlines are capped.
// Writers must support read deadlines or expose their underlying writer through Unwrap.
func WithLimits(next http.Handler, opts ServerOptions) http.Handler {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if opts.RequestTimeout <= 0 || opts.RequestTimeout > MaxRequestTimeout {
		opts.RequestTimeout = MaxRequestTimeout
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
		), h.opts.Naming)
		return
	}
	// net/http resets this for the next request. Do not unbound an unread
	// timed-out body while the server is finishing the current request.
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, h.opts.MaxBodyBytes)
	h.next.ServeHTTP(w, r.WithContext(ctx))
}

// WriteError writes public Error diagnostics and replaces unrecognized errors with a generic 500 response.
// Without a resource shape, snake_case body pointers are omitted.
func WriteError(w http.ResponseWriter, err error, naming Naming) {
	writeError(w, err, naming, nil)
}

func writeError(w http.ResponseWriter, err error, naming Naming, shape *jsonShape) {
	var public *Error
	if !errors.As(err, &public) {
		public = problem(http.StatusInternalServerError, internalErrorCode, "The request could not be completed.")
	}
	formatted := *public
	formatted.Code = naming.FieldName(public.Code)
	formatted.Errors = slices.Clone(public.Errors)
	for index := range formatted.Errors {
		field := &formatted.Errors[index]
		field.Code = naming.FieldName(field.Code)
		if strings.HasPrefix(field.Field, "/") {
			if naming != CamelCase || shape != nil {
				field.Field = shape.pointer(field.Field)
			}
		} else {
			field.Field = naming.FieldName(field.Field)
		}
	}
	encodeErr := naming.Validate()
	var body []byte
	if encodeErr == nil {
		body, encodeErr = responseJSON(formatted, naming, errorShape)
	}
	if encodeErr != nil {
		// Do not recursively encode an error when the declared error shape itself failed.
		public = problem(http.StatusInternalServerError, internalErrorCode, "The request could not be completed.")
		public.Code = naming.FieldName(public.Code)
		body, _ = json.Marshal(map[string]any{
			"status": public.Status, "code": public.Code, "title": public.Title,
		})
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(public.Status)
	_, _ = w.Write(append(body, '\n'))
}

// errorShape is fixed by the public Error type; an unsupported change must fail at startup.
var errorShape = func() *jsonShape {
	shape, err := newJSONShape(reflect.TypeFor[Error](), SnakeCase)
	if err != nil {
		panic(fmt.Sprintf("building public error shape: %v", err))
	}
	return shape
}()

// decode checks the media type and maps strict JSON, size, and read failures to public errors.
func decode[E any](r *http.Request, shape *jsonShape) (E, error) {
	var value E
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return value, problem(http.StatusUnsupportedMediaType, unsupportedMediaTypeCode, "Use application/json.")
	}
	body, err := decodeNamedStrict(r.Body, &value, shape)
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
	return decodeNamedStrict(reader, dest, nil)
}

func decodeNamedStrict(reader io.Reader, dest any, shape *jsonShape) (map[string]any, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("reading JSON: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return nil, fmt.Errorf("JSON object required")
	}
	var body map[string]any
	strictErrors, decodeErr := strictjson.UnmarshalStrict(data, &body)
	if decodeErr != nil || len(strictErrors) != 0 {
		return nil, fmt.Errorf("invalid or duplicate JSON fields")
	}
	if shape != nil {
		data, err = shape.translate(data, true)
		if err != nil {
			return nil, err
		}
	}
	strictErrors, decodeErr = strictjson.UnmarshalStrict(data, dest)
	if decodeErr != nil || len(strictErrors) != 0 {
		return nil, fmt.Errorf("invalid or unknown JSON fields")
	}
	if shape != nil {
		// Intent needs canonical int64 values after typed validation has checked the original numeric spellings.
		// Replace the wire keys instead of merging them with canonical names.
		body = nil
		if err := strictjson.UnmarshalCaseSensitivePreserveInts(data, &body); err != nil {
			return nil, err
		}
	}
	return body, nil
}

// query permits only the named parameters, each appearing once.
func query(r *http.Request, naming Naming, allowed ...string) (url.Values, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, Invalid()
	}
	canonical := make(url.Values, len(values))
	names := make(map[string]string, len(allowed))
	for _, name := range allowed {
		names[naming.FieldName(name)] = name
	}
	for key, entries := range values {
		name, ok := names[key]
		if !ok || len(entries) != 1 {
			return nil, Invalid(FieldError{
				Code: invalidQueryCode, Message: "The query contains an unsupported or repeated parameter.",
			})
		}
		canonical[name] = entries
	}
	return canonical, nil
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
