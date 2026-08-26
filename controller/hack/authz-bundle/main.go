// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command authz-bundle assembles the policy bundle cedar-agent loads, from the
// sources under deploy/authz.
//
// The sidecar wants two files in shapes nobody would choose to author:
// policies as a JSON array of {id, content} objects holding exactly one Cedar
// statement each — a whole file in one content is rejected with "unexpected
// token permit" — and entities as a single JSON array. The sources are split the
// way a reviewer would want them instead:
//
//	policies.cedar   hand-written, native Cedar syntax
//	roles.json       hand-written, roles and who holds them
//	actions.json     generated, one entity per operation      -> gen-authz-actions
//	schema.json      generated, the entity and action types   -> gen-authz-actions
//
// and this produces:
//
//	bundle/policies.json   for --policies
//	bundle/entities.json   for --data
//	bundle/schema.json     for --schema, copied through
//
// Splitting uses cedar-go rather than a hand-rolled scan for ";", which would
// cut a policy in half at a semicolon inside a string literal. Parsing here also
// means a syntax error fails the build rather than becoming a sidecar that
// starts with an empty policy store and refuses every request.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cedar-policy/cedar-go"
)

// Paths are flags rather than constants because this is its own module: it is
// invoked from its own directory, not from the controller root, so it cannot
// assume where deploy/authz is relative to the process.
var (
	sourceDir = flag.String("source", "../../deploy/authz", "directory holding the bundle sources")
	outputDir = flag.String("out", "../../deploy/authz/bundle", "directory to write the assembled bundle to")
)

// policyJSON is one entry of cedar-agent's --policies file.
type policyJSON struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "authz-bundle:", err)
		os.Exit(1)
	}
}

func run() error {
	if err := os.MkdirAll(*outputDir, 0o755); err != nil { //nolint:gosec // a directory of public policy
		return fmt.Errorf("creating %s: %w", *outputDir, err)
	}

	policies, err := buildPolicies(filepath.Join(*sourceDir, "policies.cedar"))
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*outputDir, "policies.json"), policies); err != nil {
		return err
	}
	fmt.Printf("authz-bundle: wrote %s/policies.json (%d policies)\n", *outputDir, len(policies))

	entities, err := mergeEntities(
		filepath.Join(*sourceDir, "actions.json"),
		filepath.Join(*sourceDir, "roles.json"),
	)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*outputDir, "entities.json"), entities); err != nil {
		return err
	}
	fmt.Printf("authz-bundle: wrote %s/entities.json (%d entities)\n", *outputDir, len(entities))

	schema, err := os.ReadFile(filepath.Join(*sourceDir, "schema.json"))
	if err != nil {
		return fmt.Errorf("reading the schema: %w", err)
	}
	if err := os.WriteFile(filepath.Join(*outputDir, "schema.json"), schema, 0o644); err != nil { //nolint:gosec // public policy
		return fmt.Errorf("writing the schema: %w", err)
	}
	fmt.Printf("authz-bundle: wrote %s/schema.json\n", *outputDir)
	return nil
}

// buildPolicies parses the Cedar source and emits one entry per statement.
//
// A policy's id is its @id annotation when it carries one, so a diagnostic names
// something a reader can find in the source. Without it the id would be
// cedar-go's positional "policy0", which changes meaning the moment a policy is
// inserted above it — and those ids are what appear in the logs when a request
// is allowed.
func buildPolicies(path string) ([]policyJSON, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	set, err := cedar.NewPolicySetFromBytes(filepath.Base(path), source)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	var out []policyJSON
	seen := map[string]bool{}
	for policyID, policy := range set.All() {
		id := string(policyID)
		if annotated, ok := policy.Annotations()["id"]; ok && annotated != "" {
			id = string(annotated)
		}
		if seen[id] {
			// cedar-agent keys its store by id, so a duplicate would silently
			// drop a policy — and a dropped permit is an outage while a dropped
			// forbid is a hole.
			return nil, fmt.Errorf("%s: two policies share the id %q", path, id)
		}
		seen[id] = true
		out = append(out, policyJSON{ID: id, Content: string(policy.MarshalCedar())})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s declares no policies", path)
	}

	// set.All() iterates a map, so the output order is not stable. Sorting keeps
	// the generated file byte-identical between runs, which is what lets it be
	// checked in and diffed.
	sortPolicies(out)
	return out, nil
}

func sortPolicies(policies []policyJSON) {
	for i := 1; i < len(policies); i++ {
		for j := i; j > 0 && policies[j].ID < policies[j-1].ID; j-- {
			policies[j], policies[j-1] = policies[j-1], policies[j]
		}
	}
}

// mergeEntities concatenates the entity files into the single array --data
// wants, and refuses a duplicate uid: the later one would win, so a role
// redefined in the wrong file would quietly change who holds what.
func mergeEntities(paths ...string) ([]json.RawMessage, error) {
	var merged []json.RawMessage
	seen := map[string]string{}

	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		var entities []json.RawMessage
		if err := json.Unmarshal(raw, &entities); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		for _, entity := range entities {
			var uid struct {
				UID struct {
					Type string `json:"type"`
					ID   string `json:"id"`
				} `json:"uid"`
			}
			if err := json.Unmarshal(entity, &uid); err != nil {
				return nil, fmt.Errorf("parsing an entity in %s: %w", path, err)
			}
			if uid.UID.Type == "" || uid.UID.ID == "" {
				return nil, fmt.Errorf("%s: an entity has no uid", path)
			}
			key := uid.UID.Type + `::"` + uid.UID.ID + `"`
			if first, ok := seen[key]; ok {
				return nil, fmt.Errorf("%s declares %s, already declared in %s", path, key, first)
			}
			seen[key] = path
			merged = append(merged, entity)
		}
	}
	return merged, nil
}

func writeJSON(path string, v any) error {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil { //nolint:gosec // public policy
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
