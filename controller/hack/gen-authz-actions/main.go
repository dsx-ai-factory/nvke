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
// api.BasePath, and the path, verbatim including the {param} spelling.
//
// The document is read with kin-openapi, the same library (though not the same
// pinned version — see go.mod and api/oapi-codegen.mod) oapi-codegen reads it
// with. That is the point rather than a convenience: a hand-written method list
// is a way for this table to be missing a route the router registers, and
// PathItem.Operations() is the same enumeration the router is generated from. It
// does not make the two incapable of disagreeing — a version skew between the
// two kin-openapi pins could parse the same document differently — it is
// router_test.go's parity tests that catch a disagreement in the committed
// output, not identity between the libraries.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

// Paths are flags so the //go:generate directive in internal/authz can name them
// relative to that package, rather than the tool assuming it was run from the
// controller root.
//
//nolint:gochecknoglobals // flag targets, which have to outlive flag.Parse
var (
	specPath   = flag.String("spec", "api/openapi.yaml", "the OpenAPI document to read")
	outPath    = flag.String("out", "internal/authz/actions.gen.go", "the Go source file to write")
	bundlePath = flag.String("bundle", "deploy/authz", "directory to write the generated bundle half to")

	// chartBundlePath is a second copy of the generated bundle half, packaged
	// into the chart so `helm template` can render it without reaching outside
	// its own directory tree (design-authz-chart-validation.md §4's "populated
	// by the same go:generate step"). Optional and empty by default so a caller
	// building only the Go half (or a repo without this chart) does not have to
	// pass it.
	chartBundlePath = flag.String("chart-bundle", "",
		"optional second directory to also write actions.json/schema.json to, kept in sync with -bundle")

	// safeMethods are the HTTP methods that do not change state. They decide the
	// coarse action group, because the method is a fact about the request where
	// an operationId prefix is a naming convention a future operation can break.
	//
	// HEAD and OPTIONS are here and were missing while the group was a GET test:
	// http.ServeMux serves HEAD off a GET pattern, and a spec that declares
	// either explicitly gets a route registered for it.
	//
	// These groups are what let a policy say "readers may read" and stay correct
	// as endpoints are added, instead of naming every operation and going stale.
	//
	// It shares this var block with the flag targets above only because grouper
	// requires one var block per file; it is not itself a flag target. It is a
	// global for a different reason: it is a fixed table computed once at
	// package init and read by every operation this generator processes, not a
	// value flag.Parse fills in.
	//
	//nolint:gochecknoglobals // a fixed table, computed once and read by every generator invocation — not a flag target
	safeMethods = map[string]bool{
		http.MethodGet:     true,
		http.MethodHead:    true,
		http.MethodOptions: true,
		http.MethodTrace:   true,
	}
)

const (
	// uuidFormat is the only path-parameter format this API uses, and the only
	// one the middleware knows how to canonicalize. See idFormatOf.
	uuidFormat = "uuid"

	// The Cedar vocabulary this emits. Spelled once each, because the schema and
	// the entity file have to agree on them and a typo in either produces a
	// bundle that loads and grants nothing.
	actionType = "NKE::Action"
	roleType   = "Role"

	// The two coarse action groups, derived from the HTTP method.
	groupRead  = "read"
	groupWrite = "write"

	// Cedar's JSON schema spelling: every shape and attribute is a {"type": ...}
	// object, and a Record is the shape of an entity.
	typeKey    = "type"
	shapeKey   = "shape"
	recordType = "Record"
	stringType = "String"
)

type operation struct {
	Pattern      string // "GET /api/clusters/{clusterID}"
	Method       string // "GET"
	Action       string // "getCluster"
	ResourceType string // "NKE::Cluster", or "" for a collection
	IDParam      string // "clusterID", or "" for a collection
	IDFormat     string // "uuid", or "" for a collection
	Collection   string // "clusters" — the first path segment
}

func (o operation) group() string {
	if safeMethods[o.Method] {
		return groupRead
	}
	return groupWrite
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen-authz-actions:", err)
		os.Exit(1)
	}
}

func run() error {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(*specPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", *specPath, err)
	}

	ops, err := collect(doc)
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
	if err := writeFileAtomic(*outPath, src, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", *outPath, err)
	}
	fmt.Fprintf(os.Stdout, "gen-authz-actions: wrote %s (%d operations)\n", *outPath, len(ops))

	// The bundle's generated half. The schema is what lets a Cedar validator
	// reject a policy naming an action or attribute that does not exist; without
	// it, such a policy loads clean and silently never matches.
	//
	// Written to -bundle and, if set, also to -chart-bundle -- the chart's own
	// files/authz/ copy (design-authz-chart-validation.md §4) -- so both stay
	// byte-identical outputs of this one generation pass rather than a second,
	// hand-maintained copy someone has to remember to update.
	bundleDirs := []string{*bundlePath}
	if *chartBundlePath != "" {
		bundleDirs = append(bundleDirs, *chartBundlePath)
	}
	schema := renderSchema(ops)
	actions := renderActionEntities(ops)
	for _, bundleDir := range bundleDirs {
		if err := os.MkdirAll(bundleDir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", bundleDir, err)
		}
		if err := writeJSON(filepath.Join(bundleDir, "schema.json"), schema); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "gen-authz-actions: wrote %s\n", filepath.Join(bundleDir, "schema.json"))

		if err := writeJSON(filepath.Join(bundleDir, "actions.json"), actions); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "gen-authz-actions: wrote %s (%d action entities + 2 groups)\n",
			filepath.Join(bundleDir, "actions.json"), len(ops))
	}
	return nil
}

func writeJSON(path string, v any) error {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	if err := writeFileAtomic(path, append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// writeFileAtomic writes data to path without ever leaving a half-written
// file at path itself: it writes to a temp file in the same directory (same
// directory so the final os.Rename is same-filesystem and therefore atomic
// on POSIX, unlike a cross-filesystem rename) and renames it over path only
// after the write and an explicit Sync/Close succeed.
//
// run() writes several output files in sequence (actions.gen.go, then each
// bundleDir's schema.json and actions.json) and is not transactional across
// that sequence -- a failure partway through can still leave some files
// updated and others not, the same drift this generator exists to prevent.
// This only closes the narrower, concrete risk within a single file: with
// plain os.WriteFile, a disk-full or interrupted write truncates the
// existing file in place, corrupting it; with rename-based replacement, a
// failed write only ever leaves behind an orphaned temp file, never a
// corrupted target.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, createErr := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if createErr != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, createErr)
	}
	tmpName := tmp.Name()

	if writeErr := writeAndCloseAtomicTemp(tmp, data, perm); writeErr != nil {
		// Best-effort cleanup: the write/chmod/sync/close sequence below
		// failed before any rename happened, so tmpName is the only copy of
		// this failed attempt and would otherwise be left orphaned in dir.
		os.Remove(tmpName)
		return fmt.Errorf("writing %s: %w", tmpName, writeErr)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpName, path, err)
	}
	return nil
}

// writeAndCloseAtomicTemp writes data to tmp, sets its final permissions,
// and syncs and closes it -- everything writeFileAtomic needs done to tmp
// before the rename that makes the write visible at its real path. Split out
// so writeFileAtomic's own error handling (remove the orphaned temp file on
// any failure here) has one call to wrap, rather than repeating the same
// "close tmp, then return this error" shape after every step.
func writeAndCloseAtomicTemp(tmp *os.File, data []byte, perm os.FileMode) error {
	defer tmp.Close() // closed again deliberately below to observe its error; this is only the fallback for an earlier-returning error path

	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	return tmp.Close()
}

// renderSchema emits the Cedar schema for the NKE namespace: every entity type
// a policy may name, and every action, with what each applies to.
//
// It is what lets a validator reject a policy naming an unknown action or
// attribute. policy_validation_test.go type-checks the shipped bundle against
// it; with no schema every policy is accepted and a typo becomes a rule that
// never fires.
func renderSchema(ops []operation) map[string]any {
	entityTypes := map[string]any{
		// Roles nest: "operator" is a member of "reader" so one grant covers
		// both. Cedar enforces memberOfTypes when a schema is loaded, and
		// omitting it here rejects the entity file — leaving the entity store
		// empty and every request denied.
		roleType: map[string]any{
			"memberOfTypes": []string{roleType},
			shapeKey:        emptyRecord(),
		},
		"Principal": map[string]any{
			"memberOfTypes": []string{roleType},
			shapeKey:        emptyRecord(),
		},
		"Collection": map[string]any{
			shapeKey: emptyRecord(),
		},
	}

	// Every resource type the routes can produce, and none of them declares an
	// attribute.
	//
	// That is a decision, not an omission. The resource is synthesized per
	// request from the path — the middleware runs before the handler and has not
	// read a database — so every resource entity carries an empty record. An
	// attribute declared here and populated nowhere is worse than absent: Cedar
	// skips a policy whose guard errors, so an unguarded `resource.site == "x"`
	// forbid is dropped and the permit it existed to override wins, and the
	// `resource has site` form the validator steers authors towards is simply
	// always false. Both shapes read in review as a restriction. Neither is one.
	//
	// Undeclared, the strict validator refuses either shape outright, which is
	// how an author finds out at build time instead of in production. See
	// policy_validation_test.go.
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
			shapeKey: emptyRecord(),
		}
	}

	appliesTo := func(resources []string) map[string]any {
		return map[string]any{"appliesTo": map[string]any{
			"principalTypes": []string{"Principal"},
			"resourceTypes":  resources,
			"context": map[string]any{typeKey: recordType, "attributes": map[string]any{
				"issuer": map[string]any{typeKey: stringType},
				"method": map[string]any{typeKey: stringType},
			}},
		}}
	}

	allResources := make([]string, 0, len(resourceTypes))
	for full := range resourceTypes {
		allResources = append(allResources, strings.TrimPrefix(full, "NKE::"))
	}
	sort.Strings(allResources)

	actions := map[string]any{
		groupRead:  appliesTo(allResources),
		groupWrite: appliesTo(allResources),
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

// emptyRecord is the Cedar shape of an entity type that declares no attribute,
// which here is every one of them. See the note in renderSchema.
func emptyRecord() map[string]any {
	return map[string]any{typeKey: recordType, "attributes": map[string]any{}}
}

// entityJSON is one Cedar entity in the JSON entity format.
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
	entities := make([]entityJSON, 0, len(ops)+2)
	entities = append(entities,
		entityJSON{UID: uidJSON{Type: actionType, ID: groupRead}, Attrs: map[string]any{}, Parents: []uidJSON{}},
		entityJSON{UID: uidJSON{Type: actionType, ID: groupWrite}, Attrs: map[string]any{}, Parents: []uidJSON{}},
	)
	for _, op := range ops {
		entities = append(entities, entityJSON{
			UID:     uidJSON{Type: actionType, ID: op.Action},
			Attrs:   map[string]any{},
			Parents: []uidJSON{{Type: actionType, ID: op.group()}},
		})
	}
	return entities
}

func collect(doc *openapi3.T) ([]operation, error) {
	if doc.Paths == nil {
		return nil, fmt.Errorf("%s declares no paths", *specPath)
	}

	var ops []operation
	for path, item := range doc.Paths.Map() {
		// Operations() is the enumeration oapi-codegen registers routes from:
		// all nine methods, not the five a hand-written list happened to name.
		for method, op := range item.Operations() {
			if op.OperationID == "" {
				// Every route the router registers comes from an operationId, so
				// one missing means a route with no name a policy could use.
				return nil, fmt.Errorf("%s %s: no operationId", method, path)
			}
			resourceType, idParam, err := resourceOf(path)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", method, path, err)
			}
			idFormat := ""
			if idParam != "" {
				if idFormat, err = idFormatOf(item, op, idParam); err != nil {
					return nil, fmt.Errorf("%s %s: %w", method, path, err)
				}
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
				Pattern:      method + " " + api.BasePath + path,
				Method:       method,
				Action:       op.OperationID,
				ResourceType: resourceType,
				IDParam:      idParam,
				IDFormat:     idFormat,
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
// identifier, from the LAST {param} in the path.
//
// The last rather than the first, because the last one names the object the
// operation acts on: a DELETE on /clusters/{clusterID}/nodePools/{nodePoolID}
// deletes a node pool, and taking the first parameter would authorize it as a
// decision about the cluster — a different object, of a different type, with a
// different policy. Every path in this API has at most one parameter today, so
// the rule is stated here before a nested path arrives rather than after.
//
// What the rule does not do is carry the parent: the node pool is named, the
// cluster it is in is not. Expressing "in this cluster" needs an attribute on
// the resource, and the resource is synthesized from the path — see the note in
// renderSchema.
//
// The type comes from the parameter name rather than from the collection
// segment, because the parameter is already singular: {clusterID} gives
// NKE::Cluster where "clusters" would need a de-pluralizer, and a de-pluralizer
// is a guess. A path with no parameter is a collection and returns "", "".
func resourceOf(path string) (string, string, error) {
	resourceType, idParam := "", ""
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
		resourceType, idParam = "NKE::"+strings.ToUpper(base[:1])+base[1:], name
	}
	return resourceType, idParam, nil
}

// idFormatOf returns the declared format of the path parameter carrying the
// resource identifier, and refuses anything the middleware cannot canonicalize.
//
// The format is in the table because the middleware has to name the same object
// the handler acts on, and the handler acts on the *bound* value: four spellings
// of one UUID — canonical, braced, bare-32-hex, urn:uuid — all bind to the same
// uuid.UUID and would otherwise be four distinct Cedar entities, only one of
// which a resource-scoped forbid names. Canonicalising needs to know what the
// value is, and a format nobody taught it is a guess.
func idFormatOf(item *openapi3.PathItem, op *openapi3.Operation, idParam string) (string, error) {
	// Operation-level parameters override path-item-level ones of the same
	// name, so the operation's list is searched first.
	for _, params := range []openapi3.Parameters{op.Parameters, item.Parameters} {
		for _, ref := range params {
			p := ref.Value
			if p == nil || p.In != openapi3.ParameterInPath || p.Name != idParam {
				continue
			}
			if p.Schema == nil || p.Schema.Value == nil {
				return "", fmt.Errorf("path parameter %q declares no schema", idParam)
			}
			if format := p.Schema.Value.Format; format != uuidFormat {
				return "", fmt.Errorf(
					"path parameter %q has format %q; the middleware can only canonicalize %q, "+
						"so teach it this format before adding the route",
					idParam, format, uuidFormat,
				)
			}
			return uuidFormat, nil
		}
	}
	return "", fmt.Errorf("path parameter %q is in the path but is not declared", idParam)
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
		fmt.Fprintf(&b, "\t%q: {Action: %q, ResourceType: %q, IDParam: %q, IDFormat: %q, Collection: %q},\n",
			op.Pattern, op.Action, op.ResourceType, op.IDParam, op.IDFormat, op.Collection)
	}
	b.WriteString("}\n")

	src, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("formatting generated source: %w", err)
	}
	return src, nil
}
