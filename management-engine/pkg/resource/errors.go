// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	strictjson "sigs.k8s.io/json"
)

const (
	namePointer                = "/metadata/name"
	uidPointer                 = "/metadata/uid"
	pageKeyParameter           = "pageKey"
	pageSizeParameter          = "pageSize"
	propagationPolicyParameter = "propagationPolicy"

	// Problem response codes.
	alreadyExistsCode        = "alreadyExists"
	bodyTooLargeCode         = "bodyTooLarge"
	conflictCode             = "conflict"
	datastoreUnavailableCode = "datastoreUnavailable"
	deadlineFailedCode       = "deadlineFailed"
	forbiddenCode            = "forbidden"
	internalErrorCode        = "internalError"
	invalidRequestCode       = "invalidRequest"
	invalidResourceCode      = "invalidResource"
	methodNotAllowedCode     = "methodNotAllowed"
	notFoundCode             = "notFound"
	pageExpiredCode          = "pageExpired"
	preconditionFailedCode   = "preconditionFailed"
	preconditionRequiredCode = "preconditionRequired"
	projectionFailedCode     = "projectionFailed"
	requestTimeoutCode       = "requestTimeout"
	unauthorizedCode         = "unauthorized"
	unsupportedMediaTypeCode = "unsupportedMediaType"

	// Field diagnostic codes.
	identityMismatchCode    = "identityMismatch"
	invalidCode             = "invalid"
	invalidJSONCode         = "invalidJson"
	invalidPreconditionCode = "invalidPrecondition"
	invalidQueryCode        = "invalidQuery"
	readOnlyCode            = "readOnly"
	unexpectedBodyCode      = "unexpectedBody"
)

// FieldError identifies a public JSON field or query parameter with an approved diagnostic.
type FieldError struct {
	Field   string `json:"field,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error is a sanitized problem response shared by direct and HTTP clients.
type Error struct {
	Status int          `json:"status"`
	Code   string       `json:"code"`
	Title  string       `json:"title"`
	Errors []FieldError `json:"errors,omitempty"`
}

// Error returns the approved diagnostic without datastore details.
func (e *Error) Error() string { return e.Title }

// problem constructs an error from diagnostics approved for public responses.
func problem(status int, code, title string) *Error {
	return &Error{Status: status, Code: code, Title: title}
}

// Invalid reports public input validation failures.
func Invalid(fields ...FieldError) error {
	e := problem(http.StatusBadRequest, invalidRequestCode, "The request is invalid.")
	e.Errors = fields
	return e
}

// Unauthorized returns the public 401 response for scope-resolution or datastore authentication failures.
func Unauthorized() error {
	return problem(http.StatusUnauthorized, unauthorizedCode, "Authentication is required.")
}

// Forbidden reports a denied operation without exposing policy details.
func Forbidden() error {
	return problem(http.StatusForbidden, forbiddenCode, "The operation is not permitted.")
}

// translate maps datastore failures to public errors and exposes only fields in the public JSON shape.
// Raw datastore messages and rejected values are never copied into public diagnostics.
func translate[E any](err error) error {
	if public, ok := errors.AsType[*Error](err); ok {
		return public
	}
	switch {
	case apierrors.IsForbidden(err):
		return Forbidden()
	case apierrors.IsUnauthorized(err):
		return Unauthorized()
	case apierrors.IsNotFound(err):
		return problem(http.StatusNotFound, notFoundCode, "The resource was not found.")
	case apierrors.IsAlreadyExists(err):
		return problem(http.StatusConflict, alreadyExistsCode, "The resource already exists.")
	case apierrors.IsResourceExpired(err):
		return problem(http.StatusGone, pageExpiredCode, "The page has expired.")
	case apierrors.IsConflict(err):
		return problem(http.StatusConflict, conflictCode, "The write conflicts with current state.")
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		e := problem(http.StatusUnprocessableEntity, invalidResourceCode, "The desired state is invalid.")
		var status apierrors.APIStatus
		if errors.As(err, &status) && status.Status().Details != nil {
			for _, cause := range status.Status().Details.Causes {
				if p, ok := publicPath[E](cause.Field); ok {
					e.Errors = append(e.Errors, FieldError{
						Field: p, Code: invalidCode, Message: "The field does not satisfy the resource schema.",
					})
				}
			}
		}
		return e
	default:
		return problem(http.StatusServiceUnavailable, datastoreUnavailableCode, "The datastore is unavailable.")
	}
}

// publicPath checks a datastore field against the public JSON type using a null-valued probe.
// Matching JSON names make a separate resource-specific error-path mapping unnecessary.
func publicPath[E any](path string) (string, bool) {
	if path == "metadata.name" || path == "metadata.uid" {
		return "/" + strings.ReplaceAll(path, ".", "/"), true
	}
	if !strings.HasPrefix(path, "spec.") {
		return "", false
	}
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '.' || r == '[' || r == ']' })
	var probe any
	for _, part := range slices.Backward(parts) {
		if _, err := strconv.Atoi(part); err == nil {
			probe = []any{probe}
		} else {
			probe = map[string]any{part: probe}
		}
	}
	data, err := json.Marshal(probe)
	var value E
	if err != nil {
		return "", false
	}
	invalid, err := strictjson.UnmarshalStrict(data, &value)
	if err != nil || len(invalid) != 0 {
		return "", false
	}
	for i := range parts {
		parts[i] = strings.NewReplacer("~", "~0", "/", "~1").Replace(parts[i])
	}
	return "/" + strings.Join(parts, "/"), true
}

// resourceVersionConflict distinguishes resourceVersion or UID precondition failures from field ownership conflicts.
func resourceVersionConflict(err error) bool {
	if !apierrors.IsConflict(err) {
		return false
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	if details := status.Status().Details; details != nil {
		for _, cause := range details.Causes {
			if cause.Type == metav1.CauseTypeFieldManagerConflict {
				return false
			}
		}
	}
	return true
}

// duplicateApply recognizes rejection of create-only apply against an existing resource.
// Only Create uses this narrow cause check. Other admission failures must retain their validation response.
func duplicateApply(err error) bool {
	if !apierrors.IsInvalid(err) {
		return false
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Details == nil {
		return false
	}
	causes := status.Status().Details.Causes
	return len(causes) == 1 &&
		causes[0].Field == "metadata.resourceVersion" &&
		causes[0].Type == metav1.CauseTypeFieldValueInvalid
}
