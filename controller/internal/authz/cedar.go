// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultTimeout bounds one decision. The engine is a sidecar on the pod's
	// loopback interface answering in single-digit milliseconds; this is
	// generous by roughly two orders of magnitude, and exists so a wedged
	// sidecar denies rather than holds every request open.
	DefaultTimeout = 250 * time.Millisecond

	// cedar-agent's API. Paths are fixed by the sidecar, not configurable.
	authorizePath = "/v1/is_authorized"
	policiesPath  = "/v1/policies"
	dataPath      = "/v1/data"

	// decisionAllow is the only response value that permits a request.
	decisionAllow = "Allow"
)

// CedarAgent is an Authorizer backed by a cedar-agent sidecar over HTTP.
//
// The sidecar is a calculator, not a trust boundary: it holds no secret,
// authenticates nobody, and every input it is given has already been verified
// here. It is reachable only on the pod's loopback interface.
type CedarAgent struct {
	endpoint string
	client   *http.Client
}

// NewCedarAgent builds a client for a cedar-agent at endpoint, e.g.
// "http://127.0.0.1:8180".
func NewCedarAgent(endpoint string, timeout time.Duration) (*CedarAgent, error) {
	if endpoint == "" {
		return nil, errors.New("cedar agent requires an endpoint")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parsing authorization endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("authorization endpoint %q must be http or https", endpoint)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("authorization endpoint %q has no host", endpoint)
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &CedarAgent{
		endpoint: strings.TrimRight(endpoint, "/"),
		client:   &http.Client{Timeout: timeout},
	}, nil
}

// authorizationCall is cedar-agent's request body. principal, action and
// resource are Cedar entity literals rendered as strings, which is the shape the
// agent's AuthorizationCall expects.
type authorizationCall struct {
	Principal          string         `json:"principal"`
	Action             string         `json:"action"`
	Resource           string         `json:"resource"`
	Context            map[string]any `json:"context"`
	AdditionalEntities []entity       `json:"additional_entities"`
}

// entity is one Cedar entity in the agent's JSON entity format.
type entity struct {
	UID     entityUIDJSON   `json:"uid"`
	Attrs   map[string]any  `json:"attrs"`
	Parents []entityUIDJSON `json:"parents"`
}

type entityUIDJSON struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type authorizationAnswer struct {
	Decision    string `json:"decision"`
	Diagnostics struct {
		Reason []string `json:"reason"`
		Errors []any    `json:"errors"`
	} `json:"diagnostics"`
}

// Authorize asks the sidecar one question.
//
// The resource entity is attached per request rather than stored in the bundle:
// the bundle would otherwise have to mirror every cluster, node and site in the
// database. additional_entities augments the stored entities for this call only,
// leaving the roles and action groups the bundle does hold in place.
func (c *CedarAgent) Name() string { return "cedar-agent" }

var (
	_ Authorizer = (*CedarAgent)(nil)

	// errUnsafeEntityID is the belt to Query.Validate's braces. The middleware
	// validates before dispatching, so reaching this means an engine was called
	// directly — in a test, or by a future caller — and it must still not render
	// a literal it cannot render.
	errUnsafeEntityID = fmt.Errorf("%w: identifier is not safe in an entity literal", ErrInvalidQuery)
)

// entityUID renders a Cedar entity literal, `Type::"id"`.
//
// Rendering is the sidecar's problem and nobody else's: the query crosses a
// process boundary as text. The refusal rule it enforces is not — that lives in
// Query.Validate so both engines share it. See unsafeInEntityID.
func entityUID(entityType, id string) (string, error) {
	if entityType == "" {
		return "", fmt.Errorf("%w: entity type is empty", ErrInvalidQuery)
	}
	if err := checkEntityIDValue(id); err != nil {
		return "", fmt.Errorf("%w: %w", errUnsafeEntityID, err)
	}
	return entityType + `::"` + id + `"`, nil
}

func (c *CedarAgent) Authorize(ctx context.Context, q Query) (Decision, error) {
	principal, err := entityUID(PrincipalType, q.Principal)
	if err != nil {
		return Decision{}, fmt.Errorf("principal: %w", err)
	}
	action, err := entityUID(ActionType, q.Action)
	if err != nil {
		return Decision{}, fmt.Errorf("action: %w", err)
	}
	resource, err := entityUID(q.ResourceType, q.ResourceID)
	if err != nil {
		return Decision{}, fmt.Errorf("resource: %w", err)
	}

	call := authorizationCall{
		Principal: principal,
		Action:    action,
		Resource:  resource,
		Context: map[string]any{
			ContextKeyIssuer: q.Issuer,
			ContextKeyMethod: q.Method,
		},
		AdditionalEntities: []entity{{
			UID:     entityUIDJSON{Type: q.ResourceType, ID: q.ResourceID},
			Attrs:   map[string]any{},
			Parents: []entityUIDJSON{},
		}},
	}

	var answer authorizationAnswer
	if err := c.post(ctx, authorizePath, call, &answer); err != nil {
		return Decision{}, err
	}
	// An Allow that arrived beside an evaluation error is not an Allow. Cedar
	// reports both together: a forbid whose guard errored is skipped, and the
	// permit it existed to override wins. The in-process engine refuses that,
	// and this must too, or which engine is deployed decides whether the request
	// is served.
	if len(answer.Diagnostics.Errors) > 0 && answer.Decision == decisionAllow {
		return Decision{}, fmt.Errorf("policy evaluation errored: %v", answer.Diagnostics.Errors)
	}
	// Anything but the literal "Allow" is a denial, the empty string included:
	// a response this code does not understand must not become a pass.
	return Decision{
		Allow:   answer.Decision == decisionAllow,
		Reasons: answer.Diagnostics.Reason,
	}, nil
}

// Ready reports whether the sidecar holds a usable bundle.
//
// This is not a liveness check. cedar-agent loads its policy, entity and schema
// files at startup on a best-effort basis: a file that is missing, unparseable,
// or invalid against the schema logs an error and the agent serves anyway with
// that store empty — answering 200 OK, and Deny to everything, with nothing in
// the response to say why. An empty store is indistinguishable from a
// deny-everything policy at request time, so it has to be caught here, once,
// before the process starts serving.
func (c *CedarAgent) Ready(ctx context.Context) error {
	var policies []struct {
		ID string `json:"id"`
	}
	if err := c.get(ctx, policiesPath, &policies); err != nil {
		return fmt.Errorf("reading the policy store: %w", err)
	}
	if len(policies) == 0 {
		return errors.New("the policy store is empty: the bundle did not load, " +
			"which would deny every request; check the sidecar's logs for a load error")
	}

	var entities []json.RawMessage
	if err := c.get(ctx, dataPath, &entities); err != nil {
		return fmt.Errorf("reading the entity store: %w", err)
	}
	if len(entities) == 0 {
		return errors.New("the entity store is empty: no principal has a role, " +
			"which would deny every request; check the sidecar's logs for a load error")
	}
	return nil
}

func (c *CedarAgent) post(ctx context.Context, path string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding the authorization query: %w", err)
	}
	// The URL is the operator-configured endpoint joined to a constant path, and
	// NewCedarAgent has already checked its scheme and host. Nothing a caller
	// sends reaches it.
	//nolint:gosec // G704: the endpoint is configuration, not request data
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("building the authorization request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *CedarAgent) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, http.NoBody)
	if err != nil {
		return fmt.Errorf("building the request for %s: %w", path, err)
	}
	return c.do(req, out)
}

func (c *CedarAgent) do(req *http.Request, out any) error {
	//nolint:gosec // G704: see post; the URL is configuration, not request data
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("calling the policy engine: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded: a sidecar answering with something enormous is a failure, not a
	// reason to allocate.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading the policy engine's response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the policy engine answered %d: %s", resp.StatusCode, elide(string(body), 256))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding the policy engine's response: %w", err)
	}
	return nil
}

func elide(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
