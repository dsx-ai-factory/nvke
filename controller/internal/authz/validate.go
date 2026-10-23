// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"
)

// ValidateBundle checks the bundle at dir the way NewLocal does, including the
// entity-instance checks (design-authz-chart-validation.md §6).
//
// It is the one home for these checks. NewLocal, CI's tests, and any other
// tooling that wants "is this bundle good" call it, so a bundle hand-edited
// and applied out of band (a `kubectl edit` on a live ConfigMap, a kustomize
// apply) gets the same checks at startup as the shipped bundle gets in CI.
//
// Returns one error per problem found rather than stopping at the first, the
// same choice policy_validation_test.go's TestEveryPolicyTypeChecks makes:
// an operator fixing a bundle by hand wants the whole list, not one failure
// at a time. A nil (or empty) slice means the bundle is valid.
//
// That "every problem" guarantee is real for the vocabulary/reachability
// checks (checkActionVocabulary, checkRoleVocabulary, checkRoleReachability)
// but narrower for the entity-store checks (checkEntityStore, local.go):
// checkEntityStore returns on the first missing-role/missing-principal/
// dangling-parent/missing-compiled-action problem it finds, so if several
// hold at once, only the first is reported. That early return is deliberate
// -- e.g. a dangling parent can make the rest of the entity-store checks
// meaningless to run -- not an oversight, but it does mean a caller cannot
// assume this function always surfaces every entity-store-class problem
// simultaneously the way it does for vocabulary-class ones. See
// TestValidateBundleReportsOnlyFirstEntityStoreProblem.
//
// Deliberately does not include schema (type) validation --
// policy_validation_test.go's checks against schema.json stay CI-only. That
// validator lives behind cedar-go's x/exp/schema/validate, explicitly marked
// experimental; nothing in this repo's release-gating CI runs a live
// `helm install`/`helm upgrade` today (§6/§7), so there is no context yet
// where the release binary would need to pay that dependency's cost. See
// bundle.go's own doc comment on schemaFile.
//
// The one roleMappings.json check this cannot make is that every entry's
// issuer is configured: it has no authentication config. newLocal makes it
// at startup.
func ValidateBundle(dir string) []error {
	_, errs := loadBundle(dir)
	return errs
}

// loadedBundle is what loadBundle hands back to a caller that needs the
// parsed, usable form: NewLocal. ValidateBundle discards it and keeps only the
// errors.
type loadedBundle struct {
	policies *cedar.PolicySet
	entities types.EntityMap
	names    map[cedar.PolicyID]string

	// mappings is roleMappings.json, compiled against this load's own
	// roles.json. Its issuer names are not checked here; see newLocal.
	mappings *roleMappings

	// groupOnlyWrites is every write operation the policy leaves to
	// NKE::Action::"write" (groupOnlyWrites, write_group.go). Not an error: a
	// policy may grant every write on purpose. Reported at startup, because an
	// upgrade that adds a write operation widens such a grant without any
	// change to the bundle.
	groupOnlyWrites []string
}

// loadBundle is the single implementation behind both NewLocal and
// ValidateBundle: parse policies.cedar, check its shape, load and merge the
// entity files (refusing a duplicate uid), and check the resulting entity
// store (roles present, no principal entity, at least one mapping, the
// static-parent referential-integrity check, the action-vs-operations-table
// check -- all checkEntityStore, local.go), plus the entity-instance checks
// ValidateBundle exists to consolidate: an action or role a policy names that
// no entity file declares, and a declared role that no policy ever names
// (directly or through nesting).
//
// Every finding is collected rather than returned on the first one, so a
// caller building one Go error (NewLocal) or a slice (ValidateBundle) can
// choose how to present them without loadBundle knowing which.
func loadBundle(dir string) (*loadedBundle, []error) {
	if dir == "" {
		return nil, []error{errors.New("local authorizer requires a bundle directory")}
	}

	source, err := os.ReadFile(filepath.Join(dir, policiesFile))
	if err != nil {
		return nil, []error{fmt.Errorf("reading %s: %w", policiesFile, err)}
	}
	policies, err := cedar.NewPolicySetFromBytes(policiesFile, source)
	if err != nil {
		return nil, []error{fmt.Errorf("parsing %s: %w", policiesFile, err)}
	}

	var errs []error
	if !hasAny(policies) {
		// An empty policy set refuses every request, which is indistinguishable
		// at request time from a policy nobody wrote.
		errs = append(errs, fmt.Errorf("%s declares no policies", policiesFile))
	}
	if err := checkPolicyShape(policies); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", policiesFile, err))
	}

	entities := types.EntityMap{}
	declaredIn := map[types.EntityUID]string{}
	entitiesLoaded := true
	for _, name := range entityFiles {
		if err := loadEntities(filepath.Join(dir, name), entities, declaredIn); err != nil {
			errs = append(errs, err)
			entitiesLoaded = false
		}
	}

	mappingsRaw, err := os.ReadFile(filepath.Join(dir, roleMappingsFile))
	mappingsRead := err == nil
	if !mappingsRead {
		if errors.Is(err, fs.ErrNotExist) {
			err = fmt.Errorf("%w; the chart mounts it from %s, which must exist and hold a %s key",
				err, roleMappingsSource, roleMappingsFile)
		}
		errs = append(errs, fmt.Errorf("reading %s: %w", roleMappingsFile, err))
	}

	// Comments stripped once, up front, and the stripped text handed to every
	// regex-over-source check below -- not the raw source. A policy file
	// legitimately says things like `// removed NKE::Role::"admin"` when
	// deprecating a role; scanning raw source would match that comment as a
	// real reference and, for checkActionVocabulary/checkRoleVocabulary,
	// reject an otherwise valid bundle over dead comment text. See
	// stripCedarComments.
	uncommented := stripCedarComments(source)

	// checkEntityStore and the vocabulary checks both read the entity files (or
	// the merged map) on the assumption that loading them succeeded; a file that
	// failed to parse would otherwise be reported again, less specifically, by
	// every check downstream of it. The mappings are checked against the
	// declared roles, so they wait for the entities too.
	var mappings *roleMappings
	if entitiesLoaded {
		if mappingsRead {
			var mappingErrs []error
			mappings, mappingErrs = compileRoleMappings(mappingsRaw, entities)
			errs = append(errs, mappingErrs...)
		}
		// A roleMappings.json that failed to read or decode is already
		// reported; pass mappingCountUnknown so checkEntityStore does not add
		// a second finding that it has no entries.
		mappingEntries := mappings.len()
		if mappings == nil {
			mappingEntries = mappingCountUnknown
		}
		if err := checkEntityStore(entities, declaredIn, mappingEntries); err != nil {
			errs = append(errs, err)
		}
		errs = append(errs, checkActionVocabulary(dir, uncommented)...)
		errs = append(errs, checkRoleVocabulary(dir, uncommented)...)
		errs = append(errs, checkRoleReachability(dir, uncommented)...)
	}

	names := map[cedar.PolicyID]string{}
	for id, policy := range policies.All() {
		if annotated, ok := policy.Annotations()["id"]; ok && annotated != "" {
			names[id] = string(annotated)
		}
	}

	return &loadedBundle{
		policies: policies, entities: entities, names: names, mappings: mappings,
		groupOnlyWrites: groupOnlyWrites(policies),
	}, errs
}

// Entity-instance checks. Cedar validation is type-based
// -- NKE::Role::"raeder" is a perfectly well-typed reference to a role nobody
// declared, and a policy naming it validates clean and then matches nothing.
// These check instances, not types: does this role exist, does some policy
// actually name it.

var (
	actionRef = regexp.MustCompile(`NKE::Action::"([^"]+)"`)
	roleRef   = regexp.MustCompile(`NKE::Role::"([^"]+)"`)
)

// stripCedarComments removes Cedar's two comment forms -- `//` to end of
// line, and `/* ... */`, both real Cedar syntax (internal/parser's
// cedar_tokenize.go scanComment, in the cedar-go dependency this package
// already imports) -- from policy source before any regexRef check scans it
// for role/action references. Without this, a line like
// `// removed NKE::Role::"admin"` reads as a real reference to a role that
// may no longer exist, and checkActionVocabulary/checkRoleVocabulary would
// reject an otherwise-valid bundle over dead comment text.
//
// String-literal-aware, not just a blind "//"/"/*" scan: Cedar string
// literals cannot contain a literal newline (scanString errors on one) but
// can contain a literal "//" or "/*" -- an action or role id like
// NKE::Action::"http://example" is valid Cedar. A blind scan would treat
// that "//" as starting a comment and silently swallow the rest of the
// line, hiding a real reference instead of finding a fake one -- trading a
// false positive for a worse false negative. This walks the source
// tracking whether it is inside a double-quoted string (respecting Cedar's
// backslash escaping, the same rule scanEscape implements) and only treats
// "//"/"/*" as a comment opener outside of one.
func stripCedarComments(source []byte) []byte {
	var out []byte
	inString := false
	for i := 0; i < len(source); i++ {
		c := source[i]

		if inString {
			out = append(out, c)
			if c == '\\' && i+1 < len(source) {
				// Escaped character: copy it verbatim and skip past it so an
				// escaped quote (\") does not end the string early.
				out = append(out, source[i+1])
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}

		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}

		if c == '/' && i+1 < len(source) && source[i+1] == '/' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			if i < len(source) {
				out = append(out, '\n')
			}
			continue
		}

		if c == '/' && i+1 < len(source) && source[i+1] == '*' {
			i += 2
			for i+1 < len(source) && (source[i] != '*' || source[i+1] != '/') {
				if source[i] == '\n' {
					out = append(out, '\n')
				}
				i++
			}
			i++ // land on the closing '/'; the loop's i++ advances past it
			continue
		}

		out = append(out, c)
	}
	return out
}

// entityIDs is the shape every bundle entity file (actions.json, roles.json)
// is read into for these checks: just enough to group by type and follow
// NKE::Role parent links, independent of cedar-go's own types.EntityMap so a
// malformed uid.type/uid.id pair is a checkVocabulary finding rather than a
// panic.
type entityID struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

func entityIDsOfType(path, entityType string) (map[string]bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}

	var declared []struct {
		UID entityID `json:"uid"`
	}
	if err := json.Unmarshal(raw, &declared); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", filepath.Base(path), err)
	}

	ids := map[string]bool{}
	for _, e := range declared {
		if e.UID.Type == entityType {
			ids[e.UID.ID] = true
		}
	}
	return ids, nil
}

// checkActionVocabulary reports every NKE::Action a policy names that no
// operation or action group in actionsFile declares. Cedar validation is
// type-based, so a misspelled action name (NKE::Action::"getClustre") is a
// perfectly well-typed reference that loads and type-checks clean, and then
// silently matches nothing at request time -- a rule that looks like it
// grants something and grants nothing.
func checkActionVocabulary(dir string, policySource []byte) []error {
	known, err := entityIDsOfType(filepath.Join(dir, actionsFile), ActionType)
	if err != nil {
		return []error{err}
	}

	var errs []error
	for _, m := range actionRef.FindAllStringSubmatch(string(policySource), -1) {
		if name := m[1]; !known[name] {
			errs = append(errs, fmt.Errorf(
				"%s names NKE::Action::%q, which no operation or action group in %s declares",
				policiesFile, name, actionsFile,
			))
		}
	}
	return errs
}

// checkRoleVocabulary reports every NKE::Role a policy names that rolesFile
// does not declare. The same failure mode as checkActionVocabulary, on the
// other entity type: a misspelled role name (NKE::Role::"raeder") is a
// well-typed reference to a role nobody declared, so the policy naming it
// loads and type-checks clean and then never matches -- a permit that grants
// nothing to anyone, indistinguishable from a working rule until someone
// tries the thing it was supposed to allow.
func checkRoleVocabulary(dir string, policySource []byte) []error {
	known, err := entityIDsOfType(filepath.Join(dir, rolesFile), RoleType)
	if err != nil {
		return []error{err}
	}

	var errs []error
	for _, m := range roleRef.FindAllStringSubmatch(string(policySource), -1) {
		if name := m[1]; !known[name] {
			errs = append(errs, fmt.Errorf(
				"%s names NKE::Role::%q, which %s does not declare", policiesFile, name, rolesFile,
			))
		}
	}
	return errs
}

// checkRoleReachability reports every declared role that no policy names,
// directly or through nesting under a role a policy does name -- the other
// direction from checkRoleVocabulary: a role appended with a holder and no
// matching permit satisfies every other check and grants nothing, which is
// the failure that looks like a working configuration for as long as nobody
// tries the thing it was supposed to allow.
func checkRoleReachability(dir string, policySource []byte) []error {
	raw, err := os.ReadFile(filepath.Join(dir, rolesFile))
	if err != nil {
		return []error{fmt.Errorf("reading %s: %w", rolesFile, err)}
	}

	var declaredEntities []struct {
		UID     entityID   `json:"uid"`
		Parents []entityID `json:"parents"`
	}
	if err := json.Unmarshal(raw, &declaredEntities); err != nil {
		return []error{fmt.Errorf("parsing %s: %w", rolesFile, err)}
	}

	declared := map[string]bool{}
	parents := map[string]map[string]bool{}
	for _, e := range declaredEntities {
		if e.UID.Type != RoleType {
			continue
		}
		declared[e.UID.ID] = true
		parents[e.UID.ID] = map[string]bool{}
		for _, p := range e.Parents {
			if p.Type == RoleType {
				parents[e.UID.ID][p.ID] = true
			}
		}
	}

	// policySource has already had comments stripped (loadBundle, via
	// stripCedarComments) before it reaches this function, so a role name
	// that appears only in a comment -- e.g. `// removed NKE::Role::"x"` --
	// does not count as "named" here. This is a regex over source, not a
	// Cedar parse (string-literal-aware only for comment stripping, not for
	// e.g. distinguishing a role reference from a same-shaped string
	// constant), but it does not credit commented-out references as live.
	named := map[string]bool{}
	for _, m := range roleRef.FindAllStringSubmatch(string(policySource), -1) {
		named[m[1]] = true
	}

	var errs []error
	for role := range declared {
		if !roleIsNamed(role, named, parents) {
			errs = append(errs, fmt.Errorf(
				"%s declares NKE::Role::%q, but no policy names it or any role it nests in, so it grants nothing",
				rolesFile, role,
			))
		}
	}
	return errs
}

// roleIsNamed reports whether role is used: named directly by a policy, or
// nested (through one or more levels of parents) under a role that is.
//
// Reachability flows *up* the parent chain, starting from role itself: role
// is a member of everything in parents[role], and transitively of everything
// those roles are members of, so role is used if any ancestor in that chain
// is named -- not if some other role nested *under* role happens to be
// named, which says nothing about whether role itself grants anything.
func roleIsNamed(role string, named map[string]bool, parents map[string]map[string]bool) bool {
	return roleIsNamedFrom(role, named, parents, map[string]bool{})
}

func roleIsNamedFrom(role string, named map[string]bool, parents map[string]map[string]bool, seen map[string]bool) bool {
	if named[role] {
		return true
	}
	if seen[role] {
		// A cycle in the role hierarchy is a different bug; do not spin on it.
		return false
	}
	seen[role] = true
	for parent := range parents[role] {
		if roleIsNamedFrom(parent, named, parents, seen) {
			return true
		}
	}
	return false
}
