// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cedar-policy/cedar-go/types"
)

// loadEntities merges one entity file into dst, refusing a duplicate: the later
// one would win, so a role redefined in the wrong file would quietly change who
// holds what.
//
// declaredIn records where each uid was first seen, and the check runs over the
// file's raw list rather than over the parsed entity map. That is the whole
// difference between catching a duplicate and not: an entity map is a map, so
// json.Unmarshal collapses two entries with the same uid to the last one before
// anything here could compare them. A roles.json repeating a role with a
// wider set of parents therefore loaded clean and silently granted the wider
// set — the entity count gate, the schema validator and the conformance matrix
// all stayed green, because by then there was only one entity.
// Returns the raw bytes read, alongside the usual error, so a caller
// accumulating a content hash over every file this load touched (the
// hot-reload path's change-detection gauge, design-authz-hot-reload.md §5)
// does not have to re-open the file a second time.
func loadEntities(path string, dst types.EntityMap, declaredIn map[types.EntityUID]string) ([]byte, error) {
	name := filepath.Base(path)

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}

	var declarations []json.RawMessage
	if err := json.Unmarshal(raw, &declarations); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}
	for i, declaration := range declarations {
		var head struct {
			UID types.EntityUID `json:"uid"`
		}
		if err := json.Unmarshal(declaration, &head); err != nil {
			return nil, fmt.Errorf("parsing %s[%d]: %w", name, i, err)
		}
		if where, duplicate := declaredIn[head.UID]; duplicate {
			return nil, fmt.Errorf("%s[%d] redeclares %s, already declared in %s", name, i, head.UID, where)
		}
		declaredIn[head.UID] = name
	}

	var loaded types.EntityMap
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}
	for uid, entity := range loaded {
		dst[uid] = entity
	}
	return raw, nil
}

const (
	// mappingCountUnknown tells checkEntityStore that roleMappings.json failed
	// to read or decode, so it does not add a misleading "has no entries" on
	// top of the error already reported.
	mappingCountUnknown = -1
)

// checkEntityStore refuses an entity store that loaded and cannot do its job.
//
// "Not empty" was the whole of the gate, and it cannot detect either state it
// was there for. The generated action entities alone satisfy it, so a roles.json
// edited to [] loads with 28 entities, passes Ready, logs "API authorization is
// enabled mode=enforcing" and then refuses every request as no_policy_match —
// reproduced. Nobody holds a role, and nothing says so.
//
// The action check is the other half, and it is about a bundle and a binary that
// were built at different times. The bundle is mounted from a ConfigMap; the
// operations table is compiled in. An image that adds an endpoint ships a table
// naming an action the mounted actions.json has no entity for, so that endpoint
// is refused for everyone while the pod reports itself ready — the mirror of the
// case above, and the reason the two are checked together here rather than left
// to whoever notices first.
//
// What this does not catch, by design — it is a shape check, not a semantic
// one, and the scope is deliberately narrower than exhaustive: a
// roleMappings.json whose entries match no caller that ever arrives; a policy
// set that is present and non-empty per hasAny but is all-forbid, or is shaped
// so no request can ever match a permit, so Ready passes and every call is
// still refused; and an action entity whose UID is present, satisfying this
// loop, but whose "read"/"write" action-group parent was stripped, so the
// entity exists and the coarse-role grant that depends on that parent link
// does not fire.
// None of these are bugs in this gate — they are outside what a startup-time
// shape check can see without evaluating policies against sample requests —
// but they are worth naming so a future reader does not read "Ready" as a
// stronger guarantee than it is.
//
// One thing this loop does check, deliberately, is every entity's stored
// Parents set: every UID an entity names as a parent must itself be a key in
// entities. cedar-go's ancestry walk (cedar-go
// internal/eval/evalers.go, entityInOne/entityInSet) checks
// fe.Parents.Contains directly against an entity's own stored parent set and
// never calls Get to confirm a parent is itself loaded — overlayPrincipal
// (local.go) explains this for the principal, and filters on it there. A role's
// parents in roles.json never go through that filter, so a role whose parent
// names a role nothing declares would make its holders members of a role that
// was never loaded, with nothing at request time to catch it. Catching that
// here, once, at load, is this project's own standing convention for anything
// that is a property of the static bundle rather than of one request -- the
// same reason the role and mapping counts and the action-vs-route check run in
// this same function rather than being re-derived per call.
//
// Every grant lives in roleMappings.json, where each entry names an issuer, so
// the store holds no NKE::Principal. A principal entity would hold its roles
// for its subject string from every issuer. declaredIn maps each entity to the
// file that declared it, for the error; nil names no file. mappingEntries is
// how many roleMappings.json entries the same bundle holds, and it must be at
// least one. mappingCountUnknown skips that rule, for a file whose failure to
// load is already reported.
func checkEntityStore(entities types.EntityMap, declaredIn map[types.EntityUID]string, mappingEntries int) error {
	var roles int
	principals := map[string][]string{}
	for uid := range entities {
		switch string(uid.Type) {
		case RoleType:
			roles++
		case PrincipalType:
			file := declaredIn[uid]
			if file == "" {
				file = "an entity file"
			}
			principals[file] = append(principals[file], uid.String())
		}
	}
	if roles == 0 {
		return fmt.Errorf("%s declares no %s: every request would be refused", rolesFile, RoleType)
	}
	if len(principals) > 0 {
		var errs []error
		for _, file := range slices.Sorted(maps.Keys(principals)) {
			errs = append(errs, fmt.Errorf("%s declares %s, which would hold its roles for that subject from every "+
				"issuer; remove it and grant its roles with a %s entry naming the caller's issuer",
				file, capList(principals[file], maxListed), roleMappingsFile))
		}
		return errors.Join(errs...)
	}
	if mappingEntries == 0 {
		return fmt.Errorf("%s has no entries: no caller would hold a role. Add an entry granting a role to a "+
			"caller from an issuer in AuthenticationConfiguration, in %s", roleMappingsFile, roleMappingsSource)
	}

	for uid, entity := range entities {
		for parent := range entity.Parents.All() {
			if _, declared := entities.Get(parent); !declared {
				return fmt.Errorf(
					"%s names parent %s, which no entity file (%s) declares: cedar-go's ancestry walk "+
						"checks an entity's stored Parents directly and never confirms a parent is itself "+
						"loaded, so this would silently grant membership in a role that does not exist",
					uid, parent, strings.Join(entityFiles, ", "),
				)
			}
		}
	}

	// This loop is also the action/operations-table skew check: it asserts the
	// generated actions.json agrees with the generated operations table. It
	// overlaps checkActionVocabulary deliberately and runs unconditionally, not
	// gated behind the vocabulary checks.
	for pattern, op := range operations {
		if _, ok := entities.Get(types.NewEntityUID(ActionType, types.String(op.Action))); !ok {
			return fmt.Errorf(
				"%s declares no %s::%q, which this build authorizes %q with: the mounted bundle is older than "+
					"the image, and every call to that endpoint would be refused",
				actionsFile, ActionType, op.Action, pattern,
			)
		}
	}
	return nil
}

const (
	// maxListed is how many items capList names before summarizing the rest.
	maxListed = 5
)

// capList sorts items and joins at most limit of them, then "and N more".
func capList(items []string, limit int) string {
	slices.Sort(items)
	if len(items) <= limit {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:limit], ", "), len(items)-limit)
}
