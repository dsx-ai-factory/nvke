// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var (
	ErrInstanceAlreadyExists = errors.New("forge instance already exists")
	ErrInstanceNotFound      = errors.New("forge instance not found")
	ErrBadRequest            = errors.New("forge bad request")
	ErrSubnetExhausted       = errors.New("forge subnet exhausted")
	ErrVpcPrefixExhausted    = errors.New("forge vpc prefix exhausted")
	// ErrTenantNotAuthorized indicates the configured API key is not authorized for the
	// requested tenant/org (403 Forbidden). This is expected when a single key only covers
	// a subset of tenants, so callers can treat it as a benign "no access" condition rather
	// than a hard error.
	ErrTenantNotAuthorized = errors.New("forge tenant not authorized")
)

// Attempt to convert the forge API error to a more specific sentinel error types.
// Unfortunately, forge has some bugs where they return 500 Internal Server Error for some cases,
// and/or only return the information we need in the body.message,
// so we need to parse the response body to determine the error and return a specific error.
// This allows consumers to make business logic decisions based on the error type.
func asForgeAPIError(res *http.Response, err error) error {
	body := ""
	if res != nil && res.Body != nil {
		bodyBytes, _ := io.ReadAll(res.Body)
		body = string(bodyBytes)
		_ = res.Body.Close()
	}

	// This does return a 409 Conflict, but that's not documented.
	// Example Body:
	// {
	//   "source": "forge",
	//   "message": "An Instance with specified name already exists for Tenant",
	//   "data": {
	//     "id": "748e975e-c6bf-46ed-a5d2-748e48594500"
	//   }
	// }
	if strings.Contains(body, "already exists") {
		return fmt.Errorf("%w: %s", ErrInstanceAlreadyExists, body)
	}

	// Example Body:
	// {
	//   "source": "forge",
	//   "message": "Failed to execute sync workflow to create Instance on Site: workflow execution error (type: CreateInstanceV2, workflowID: instance-create-4445c3f5-1067-4d64-86f9-d7f7a8b83b26, runID: 4e46d071-83d5-47de-84bd-bc48e48b9712): activity error (type: CreateInstanceOnSite, scheduledEventID: 5, startedEventID: 6, identity: 1@elektra-site-agent-2@): rpc error: code = Internal desc = Error in DHCP allocation/handling: Prefix: 10.45.144.20 has exhausted all address space (type: Error, retryable: true)",
	//   "data": null
	// }
	if strings.Contains(body, "has exhausted all address space") {
		return fmt.Errorf("%w: %s", ErrSubnetExhausted, body)
	}

	// Example Body:
	// {
	//   "source": "forge",
	//   "message": "Failed to execute sync workflow to create Instance on Site: workflow execution error (type: CreateInstanceV2, workflowID: instance-create-28716800-bee3-4d16-813a-873e5b13af0e, runID: 53f641ba-3f09-470b-af77-8df044f38383): activity error (type: CreateInstanceOnSite, scheduledEventID: 5, startedEventID: 6, identity: 1@elektra-site-agent-2@): rpc error: code = Internal desc = Internal error: IP address exhausted: 7.243.35.0/30 (type: Error, retryable: true)",
	//   "data": null
	// }
	if strings.Contains(body, "IP address exhausted") {
		return fmt.Errorf("%w: %s", ErrVpcPrefixExhausted, body)
	}

	// This does return a 404 Not Found, so we are using that and appending the body.
	// Example Body:
	// {
	//   "source": "forge",
	//   "message": "Could not find Instance with specified ID",
	//   "data": null
	// }
	if res != nil && res.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrInstanceNotFound, body)
	}

	// A 403 Forbidden means the configured API key has no access to this tenant/org.
	// A single key typically only covers a subset of Forge tenants.
	if res != nil && res.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: %s", ErrTenantNotAuthorized, body)
	}

	// Else it's a different forge API error, so we return the original error and body.
	return fmt.Errorf("%w: %s", err, body)
}
