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

	// configFile is the NVKE authorization config: where each issuer's
	// callers get their tenant, who is an operator, and which callers hold
	// which roles.
	configFile = "authorization.yaml"
)

// bundle is one load of the bundle files. It never changes after load.
type bundle struct {
	policies      *cedar.PolicySet
	entities      types.EntityMap
	config        compiledConfig
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
	for _, file := range []string{policiesFile, rolesFile, configFile} {
		content, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		raw[file] = content
		fmt.Fprintf(hash, "%d\n", len(content))
		hash.Write(content)
	}

	policies, err := cedar.NewPolicySetFromBytes(policiesFile, raw[policiesFile])
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", policiesFile, err)
	}
	b := &bundle{policies: policies, entities: types.EntityMap{}, actions: opts.Actions, resourceTypes: opts.ResourceTypes}
	hash.Sum(b.hash[:0])

	// One action per API action, with no parents, so a policy grants only the
	// actions it names.
	for _, action := range opts.Actions {
		uid := types.NewEntityUID(actionType, types.String(action))
		b.entities[uid] = types.Entity{UID: uid, Parents: types.NewEntityUIDSet(), Attributes: types.NewRecord(nil)}
	}
	roleErrs := loadRoles(raw[rolesFile], b.entities)
	errs := append(b.checkPolicies(), roleErrs...)
	// The checks below need every role. After roles.json fails, their errors
	// would only repeat that failure, so loadConfig doesn't check grants.
	var declared types.EntityMap
	if len(roleErrs) == 0 {
		errs = append(errs, b.checkEntities()...)
		errs = append(errs, b.checkNames()...)
		declared = b.entities
	}
	if b.config, err = loadConfig(raw[configFile], opts.Issuers, declared); err != nil {
		errs = append(errs, err)
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
		return "", fmt.Errorf("resolve the bundle directory: %w", err)
	default:
		return resolved, nil
	}
}

// entityJSON is one role in rolesFile, as Cedar entity JSON. Attrs is here so
// a role may carry Cedar's usual empty "attrs": {}; loadRoles refuses any
// attribute, because checkPolicies refuses every policy that would read one.
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

// loadRoles adds the roles in raw, the content of rolesFile, to dst. Every
// entity must be a role with a trimmed, non-empty ID, and none may appear
// twice: a duplicate would replace the first, so a repeated role with more
// parents would grant more. The file decodes strictly, because a duplicate or
// misspelled "parents" key would drop parents without an error.
func loadRoles(raw []byte, dst types.EntityMap) []error {
	var entities []*entityJSON
	strictErrs, err := kjson.UnmarshalStrict(raw, &entities)
	if err == nil {
		err = errors.Join(strictErrs...)
	}
	if err != nil {
		return []error{fmt.Errorf("parse %s: %w", rolesFile, err)}
	}
	if len(entities) == 0 {
		return []error{fmt.Errorf("%s declares no roles", rolesFile)}
	}

	var errs []error
	for i, e := range entities {
		if e == nil {
			errs = append(errs, fmt.Errorf("%s[%d] is null", rolesFile, i))
			continue
		}
		uid := e.UID.uid()
		if uid.Type != roleType {
			errs = append(errs, fmt.Errorf("%s[%d] is %s; the file holds %s entities only", rolesFile, i, uid, roleType))
			continue
		}
		if err := checkTrimmed(e.UID.ID); err != nil {
			errs = append(errs, fmt.Errorf("%s[%d] id %w", rolesFile, i, err))
			continue
		}
		if _, duplicate := dst[uid]; duplicate {
			errs = append(errs, fmt.Errorf("%s[%d] declares %s again", rolesFile, i, uid))
			continue
		}
		if len(e.Attrs) > 0 {
			errs = append(errs, fmt.Errorf("%s[%d] has attributes, which no policy may read", rolesFile, i))
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

// checkEntities checks that every parent is a declared role. cedar-go doesn't
// check that a parent exists, so a role nested in an undeclared role would make
// its holders members of a role no file defines.
func (b *bundle) checkEntities() []error {
	var errs []error
	for _, uid := range sortedUIDs(b.entities) {
		for _, parent := range slices.SortedFunc(b.entities[uid].Parents.All(), compareUIDs) {
			if _, declared := b.entities[parent]; !declared || parent.Type != roleType {
				errs = append(errs, fmt.Errorf("%s has parent %s, which isn't a declared %s", uid, parent, roleType))
			}
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
