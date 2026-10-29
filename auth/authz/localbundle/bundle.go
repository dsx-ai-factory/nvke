// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package localbundle

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"
	kjson "sigs.k8s.io/json"
)

// The bundle's files.
const (
	// policiesFile holds the policies, in Cedar. A policy's @id annotation
	// names it in decisions and errors.
	policiesFile = "policies.cedar"

	// rolesFile holds the NVKE::Role entities and how they nest.
	rolesFile = "roles.json"

	// actionsFile holds the NVKE::Action entities: the API's actions and the
	// action groups they belong to.
	actionsFile = "actions.json"

	// roleMappingsFile holds the role mappings: which callers hold which
	// roles.
	roleMappingsFile = "roleMappings.json"
)

// bundle is one load of the bundle files. It never changes after load.
type bundle struct {
	policies      *cedar.PolicySet
	entities      types.EntityMap
	mappings      roleMappings
	actions       []string
	resourceTypes []string

	// hash is a SHA-256 over each file's length and content, in a fixed
	// order, so a reload can tell whether the bundle changed.
	hash [sha256.Size]byte
}

// policyName names a policy by its @id, or by cedar-go's positional ID.
func (b *bundle) policyName(id cedar.PolicyID) string {
	if p := b.policies.Get(id); p != nil && p.Annotations()["id"] != "" {
		return string(p.Annotations()["id"])
	}
	return string(id)
}

// load reads and checks the bundle in opts.Dir. A file that can't be read or
// parsed skips the checks that need it; otherwise the error joins every
// problem found.
func load(opts Options) (*bundle, error) {
	if opts.Dir == "" {
		return nil, errors.New("the bundle directory is not set")
	}
	dir, err := resolveGeneration(opts.Dir)
	if err != nil {
		return nil, err
	}

	raw := map[string][]byte{}
	hash := sha256.New()
	for _, file := range []string{policiesFile, rolesFile, actionsFile, roleMappingsFile} {
		content, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", file, err)
		}
		raw[file] = content
		fmt.Fprintf(hash, "%d\n", len(content))
		hash.Write(content)
	}

	policies, err := cedar.NewPolicySetFromBytes(policiesFile, raw[policiesFile])
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", policiesFile, err)
	}
	b := &bundle{policies: policies, entities: types.EntityMap{}, actions: opts.Actions, resourceTypes: opts.ResourceTypes}
	hash.Sum(b.hash[:0])

	entityErrs := append(
		loadEntities(rolesFile, raw[rolesFile], roleType, b.entities),
		loadEntities(actionsFile, raw[actionsFile], actionType, b.entities)...,
	)
	errs := append(b.checkPolicies(), entityErrs...)
	// The checks below need every entity. After an entity file fails, their
	// errors would only repeat that failure.
	if len(entityErrs) == 0 {
		errs = append(errs, b.checkEntities(opts.Actions)...)
		errs = append(errs, b.checkNames()...)
		if b.mappings, err = compileRoleMappings(raw[roleMappingsFile], b.entities, opts.Issuers); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return b, nil
}

// resolveGeneration returns the directory that holds the current generation of
// a mounted ConfigMap, so every file in one load comes from the same update.
// The kubelet writes each update to a new directory, then points the "..data"
// symlink at it. A directory with no "..data" is read as it is.
func resolveGeneration(dir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(dir, "..data"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return dir, nil
	case err != nil:
		return "", fmt.Errorf("resolving the bundle directory: %w", err)
	default:
		return resolved, nil
	}
}

// entityJSON is one entity in an entity file. Attrs is here so a file may
// carry Cedar's usual empty "attrs": {}; loadEntities refuses any attribute,
// because checkPolicies refuses every policy that would read one.
type entityJSON struct {
	UID     entityRef                  `json:"uid"`
	Parents []entityRef                `json:"parents"`
	Attrs   map[string]json.RawMessage `json:"attrs"`
}

// entityRef names an entity by type and ID.
type entityRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// uid returns r as a Cedar entity UID.
func (r entityRef) uid() types.EntityUID {
	return types.NewEntityUID(types.EntityType(r.Type), types.String(r.ID))
}

// loadEntities adds the entities in raw, the content of file, to dst. Every
// entity must have entityType, and none may appear twice: a duplicate would
// replace the first, so a repeated role with more parents would grant more.
// The file decodes strictly, because a duplicate or misspelled "parents" key
// would drop parents without an error.
func loadEntities(file string, raw []byte, entityType types.EntityType, dst types.EntityMap) []error {
	var entities []entityJSON
	strictErrs, err := kjson.UnmarshalStrict(raw, &entities)
	if err != nil {
		return []error{fmt.Errorf("parsing %s: %w", file, err)}
	}
	if len(strictErrs) > 0 {
		return []error{fmt.Errorf("parsing %s: %w", file, errors.Join(strictErrs...))}
	}

	var errs []error
	for i, e := range entities {
		uid := e.UID.uid()
		if uid.Type != entityType {
			errs = append(errs, fmt.Errorf("%s[%d] is %s; the file holds %s entities only", file, i, uid, entityType))
			continue
		}
		if _, duplicate := dst[uid]; duplicate {
			errs = append(errs, fmt.Errorf("%s[%d] declares %s again", file, i, uid))
			continue
		}
		if len(e.Attrs) > 0 {
			errs = append(errs, fmt.Errorf("%s[%d] has attributes, which no policy may read", file, i))
			continue
		}
		parents := make([]types.EntityUID, 0, len(e.Parents))
		for _, p := range e.Parents {
			parents = append(parents, p.uid())
		}
		dst[uid] = types.Entity{UID: uid, Parents: types.NewEntityUIDSet(parents...), Attributes: types.NewRecord(nil)}
	}
	return errs
}

// checkEntities checks the loaded entities as a whole:
//
//   - At least one role, or no caller could hold a role.
//   - Every parent is a declared entity of the same type. cedar-go doesn't
//     check that a parent exists, so a role nested in an undeclared role
//     would make its holders members of a role no file defines.
//   - Every action the API asks about is declared, and isn't an action group.
//     A permit on `action in NVKE::Action::"getCluster"` would otherwise
//     grant every action nested in getCluster.
func (b *bundle) checkEntities(actions []string) []error {
	var errs []error
	roles := 0
	groupMember := map[types.EntityUID]types.EntityUID{}
	for _, uid := range sortedUIDs(b.entities) {
		if uid.Type == roleType {
			roles++
		}
		for _, parent := range slices.SortedFunc(b.entities[uid].Parents.All(), compareUIDs) {
			if _, declared := b.entities[parent]; !declared || parent.Type != uid.Type {
				errs = append(errs, fmt.Errorf("%s has parent %s, which isn't a declared %s", uid, parent, uid.Type))
			}
			if _, seen := groupMember[parent]; !seen && uid.Type == actionType {
				groupMember[parent] = uid
			}
		}
	}
	if roles == 0 {
		errs = append(errs, fmt.Errorf("%s declares no roles", rolesFile))
	}
	for _, action := range actions {
		uid := types.NewEntityUID(actionType, types.String(action))
		if _, declared := b.entities[uid]; !declared {
			errs = append(errs, fmt.Errorf("%s doesn't declare the API's action %q", actionsFile, action))
		} else if member, isGroup := groupMember[uid]; isGroup {
			errs = append(errs, fmt.Errorf("%s makes the API's action %q a parent of %s; an API action can't be an action group",
				actionsFile, action, member))
		}
	}
	return errs
}

// sortedUIDs returns the entities' UIDs in order, so errors list in the same
// order on every load.
func sortedUIDs(entities types.EntityMap) []types.EntityUID {
	return slices.SortedFunc(maps.Keys(entities), compareUIDs)
}

// compareUIDs orders UIDs by their Cedar form, such as NVKE::Role::"admin".
func compareUIDs(a, b types.EntityUID) int {
	return strings.Compare(a.String(), b.String())
}
