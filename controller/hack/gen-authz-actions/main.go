// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command gen-authz-actions writes internal/authz/actions.gen.go from
// api/openapi.yaml.
//
// The authorization middleware sits outside the mux and identifies an operation
// by the route pattern http.ServeMux matched. That pattern has to be mapped to
// something a policy can name — the operationId — and to the resource the
// operation acts on. Both are already in the OpenAPI document that
// api/server.gen.go is generated from, so both are taken from there rather than
// maintained by hand: a table that can drift from the router is a route nobody
// notices is unprotected.
//
// The pattern is built the same way HandlerWithOptions builds it: the method,
// the "/api" base URL, and the path, verbatim including the {param} spelling.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// baseURL is api.HandlerWithOptions' BaseURL, set in internal/server.NewServer.
const baseURL = "/api"

// Paths are flags so the //go:generate directive in internal/authz can name them
// relative to that package, rather than the tool assuming it was run from the
// controller root.
var (
	specPath   = flag.String("spec", "api/openapi.yaml", "the OpenAPI document to read")
	outPath    = flag.String("out", "internal/authz/actions.gen.go", "the Go source file to write")
	bundlePath = flag.String("bundle", "deploy/authz", "directory to write the generated bundle half to")
)

// methods is the set of HTTP verbs read out of a path item, lowercase as
// OpenAPI spells them.
var methods = []string{"get", "put", "post", "patch", "delete"} //nolint:gochecknoglobals // a fixed table

type operation struct {
	Pattern      string // "GET /api/clusters/{clusterID}"
	Method       string // "GET"
	Action       string // "getCluster"
	ResourceType string // "NKE::Cluster", or "" for a collection
	IDParam      string // "clusterID", or "" for a collection
	Collection   string // "clusters" — the first path segment
}

// group returns the coarse action group an operation belongs to. Derived from
// the method rather than from the operationId's prefix: "get"/"list" is a naming
// convention that a future operation can break, where the method cannot be
// anything but what it is.
//
// These groups are what let a policy say "readers may read" and stay correct as
// endpoints are added, instead of naming all 26 operations and going stale.
func (o operation) group() string {
	if o.Method == "GET" {
		return "read"
	}
	return "write"
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen-authz-actions:", err)
		os.Exit(1)
	}
}

func run() error {
	raw, err := os.ReadFile(*specPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", *specPath, err)
	}

	// A path item holds operations keyed by method alongside non-operation keys
	// such as "parameters", so the values are held as raw nodes and only the
	// method keys are decoded.
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parsing %s: %w", *specPath, err)
	}

	ops, err := collect(doc.Paths)
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		// An empty table would authorize nothing and look like a working build.
		return fmt.Errorf("no operations found in %s", *specPath)
	}

	src, err := render(ops)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*outPath, src, 0o644); err != nil { //nolint:gosec // generated source, not a secret
		return fmt.Errorf("writing %s: %w", *outPath, err)
	}
	fmt.Printf("gen-authz-actions: wrote %s (%d operations)\n", *outPath, len(ops))

	// The bundle's generated half. The schema is what makes cedar-agent reject a
	// policy naming an action or attribute that does not exist; without it, such
	// a policy loads clean and silently never matches.
	bundleDir := *bundlePath
	if err := os.MkdirAll(bundleDir, 0o755); err != nil { //nolint:gosec // a directory of public policy
		return fmt.Errorf("creating %s: %w", bundleDir, err)
	}
	if err := writeJSON(filepath.Join(bundleDir, "schema.json"), renderSchema(ops)); err != nil {
		return err
	}
	fmt.Printf("gen-authz-actions: wrote %s\n", filepath.Join(bundleDir, "schema.json"))

	if err := writeJSON(filepath.Join(bundleDir, "actions.json"), renderActionEntities(ops)); err != nil {
		return err
	}
	fmt.Printf("gen-authz-actions: wrote %s (%d action entities + 2 groups)\n",
		filepath.Join(bundleDir, "actions.json"), len(ops))
	return nil
}

func writeJSON(path string, v any) error {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil { //nolint:gosec // public policy, not a secret
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// renderSchema emits the Cedar schema for the NKE namespace: every entity type
// a policy may name, and every action, with what each applies to.
//
// Loading it is not optional. cedar-agent validates policies against the schema
// as they are stored, and rejects one naming an unknown action or attribute with
// a "did you mean" — but only if a schema is present. With no schema every
// policy is accepted and a typo becomes a rule that never fires.
func renderSchema(ops []operation) map[string]any {
	entityTypes := map[string]any{
		// Roles nest: "operator" is a member of "reader" so one grant covers
		// both. Cedar enforces memberOfTypes when a schema is loaded, and
		// omitting it here rejects the entity file — leaving the entity store
		// empty and every request denied, with only a line in the sidecar's log
		// to say so.
		"Role": map[string]any{
			"memberOfTypes": []string{"Role"},
			"shape":         map[string]any{"type": "Record", "attributes": map[string]any{}},
		},
		"Principal": map[string]any{
			"memberOfTypes": []string{"Role"},
			"shape": map[string]any{"type": "Record", "attributes": map[string]any{
				// Declared but not required: a principal scoped to no site carries
				// an empty set rather than being absent, so a policy reading it
				// does not error.
				"sites": map[string]any{
					"type": "Set", "element": map[string]any{"type": "String"}, "required": false,
				},
			}},
		},
		"Collection": map[string]any{
			"shape": map[string]any{"type": "Record", "attributes": map[string]any{}},
		},
	}

	// Every resource type the routes can produce, each carrying the optional
	// site attribute an ownership policy would read.
	resourceTypes := map[string]bool{"NKE::Collection": true}
	for _, op := range ops {
		if op.ResourceType != "" {
			resourceTypes[op.ResourceType] = true
		}
	}
	for full := range resourceTypes {
		short := strings.TrimPrefix(full, "NKE::")
		if _, ok := entityTypes[short]; ok {
			continue
		}
		entityTypes[short] = map[string]any{
			"shape": map[string]any{"type": "Record", "attributes": map[string]any{
				"site": map[string]any{"type": "String", "required": false},
			}},
		}
	}

	appliesTo := func(resources []string) map[string]any {
		return map[string]any{"appliesTo": map[string]any{
			"principalTypes": []string{"Principal"},
			"resourceTypes":  resources,
			"context": map[string]any{"type": "Record", "attributes": map[string]any{
				"issuer": map[string]any{"type": "String"},
				"method": map[string]any{"type": "String"},
			}},
		}}
	}

	allResources := make([]string, 0, len(resourceTypes))
	for full := range resourceTypes {
		allResources = append(allResources, strings.TrimPrefix(full, "NKE::"))
	}
	sort.Strings(allResources)

	actions := map[string]any{
		"read":  appliesTo(allResources),
		"write": appliesTo(allResources),
	}
	for _, op := range ops {
		resource := "Collection"
		if op.ResourceType != "" {
			resource = strings.TrimPrefix(op.ResourceType, "NKE::")
		}
		entry := appliesTo([]string{resource})
		entry["memberOf"] = []map[string]string{{"id": op.group()}}
		actions[op.Action] = entry
	}

	return map[string]any{"NKE": map[string]any{
		"entityTypes": entityTypes,
		"actions":     actions,
	}}
}

// entityJSON is one Cedar entity in cedar-agent's JSON entity format.
type entityJSON struct {
	UID     uidJSON        `json:"uid"`
	Attrs   map[string]any `json:"attrs"`
	Parents []uidJSON      `json:"parents"`
}

type uidJSON struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// renderActionEntities emits the NKE::Action half of the entity store: one
// entity per operation, each a member of its coarse group.
//
// The parent relationship is the point. It is what lets a policy say "readers
// may read" once and stay correct as operations are added, rather than naming
// each of them and going quietly stale.
func renderActionEntities(ops []operation) []entityJSON {
	entities := []entityJSON{
		{UID: uidJSON{Type: "NKE::Action", ID: "read"}, Attrs: map[string]any{}, Parents: []uidJSON{}},
		{UID: uidJSON{Type: "NKE::Action", ID: "write"}, Attrs: map[string]any{}, Parents: []uidJSON{}},
	}
	for _, op := range ops {
		entities = append(entities, entityJSON{
			UID:     uidJSON{Type: "NKE::Action", ID: op.Action},
			Attrs:   map[string]any{},
			Parents: []uidJSON{{Type: "NKE::Action", ID: op.group()}},
		})
	}
	return entities
}

func collect(paths map[string]map[string]yaml.Node) ([]operation, error) {
	var ops []operation
	for path, item := range paths {
		for _, method := range methods {
			node, ok := item[method]
			if !ok {
				continue
			}
			var op struct {
				OperationID string `yaml:"operationId"`
			}
			if err := node.Decode(&op); err != nil {
				return nil, fmt.Errorf("%s %s: %w", method, path, err)
			}
			if op.OperationID == "" {
				// Every route the router registers comes from an operationId, so
				// one missing means a route with no name a policy could use.
				return nil, fmt.Errorf("%s %s: no operationId", method, path)
			}
			resourceType, idParam, err := resourceOf(path)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", method, path, err)
			}
			// Collection only matters to Resolver.Resolve when there is no object
			// identifier — that is what it names the NKE::Collection resource as.
			// An object route never reads it, so it is left unset there rather
			// than populated with a value nothing looks at.
			collection := ""
			if idParam == "" {
				collection = collectionOf(path)
			}
			ops = append(ops, operation{
				Pattern:      strings.ToUpper(method) + " " + baseURL + path,
				Method:       strings.ToUpper(method),
				Action:       op.OperationID,
				ResourceType: resourceType,
				IDParam:      idParam,
				Collection:   collection,
			})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Pattern < ops[j].Pattern })
	return ops, nil
}

// collectionOf returns the first path segment: "/nodePools/{id}/configHistory"
// gives "nodePools". It names the NKE::Collection entity a collection operation
// is authorized against.
func collectionOf(path string) string {
	// strings.Split always returns at least one element, even for "" (a lone
	// [""]), so segments[0] is always defined; there is no empty case to guard.
	return strings.Split(strings.Trim(path, "/"), "/")[0]
}

// resourceOf derives the Cedar entity type and the path parameter carrying its
// identifier, from the first {param} in the path.
//
// The type comes from the parameter name rather than from the collection
// segment, because the parameter is already singular: {clusterID} gives
// NKE::Cluster where "clusters" would need a de-pluralizer, and a de-pluralizer
// is a guess. A path with no parameter is a collection and returns "", "".
func resourceOf(path string) (resourceType, idParam string, err error) {
	for _, segment := range strings.Split(strings.Trim(path, "/"), "/") {
		if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")
		base, ok := strings.CutSuffix(name, "ID")
		if !ok || base == "" {
			// Every path parameter in this API is named <thing>ID. One that is
			// not has no derivable entity type, and guessing is how a resource
			// silently becomes the wrong one.
			return "", "", fmt.Errorf("path parameter %q does not end in \"ID\"", name)
		}
		return "NKE::" + strings.ToUpper(base[:1]) + base[1:], name, nil
	}
	return "", "", nil
}

func render(ops []operation) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Code generated by hack/gen-authz-actions from api/openapi.yaml. DO NOT EDIT.

package authz

// operations maps the route pattern http.ServeMux matched to the Cedar action
// and resource it authorizes. A pattern absent from this map is denied.
var operations = map[string]Operation{
`)
	for _, op := range ops {
		fmt.Fprintf(&b, "\t%q: {Action: %q, ResourceType: %q, IDParam: %q, Collection: %q},\n",
			op.Pattern, op.Action, op.ResourceType, op.IDParam, op.Collection)
	}
	b.WriteString("}\n")

	src, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("formatting generated source: %w", err)
	}
	return src, nil
}
