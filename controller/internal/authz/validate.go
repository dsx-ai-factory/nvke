// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"crypto/sha256"
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
// but narrower for the entity-store checks (checkEntityStore, entitystore.go):
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
// issuer is configured: it has no authentication config. newLocalFromSource
// makes it at startup and on every reload.
func ValidateBundle(dir string) []error {
	_, errs := loadBundle(dir)
	return errs
}

// Bundle is what loadBundle hands back to a caller that needs the parsed,
// usable form: NewLocal, via DirBundleSource.Load (bundle.go). ValidateBundle
// discards it and keeps only the errors. It is also BundleSource's result
// type: a source other than DirBundleSource would hand back the same shape,
// however it got its bytes.
type Bundle struct {
	policies *cedar.PolicySet
	entities types.EntityMap
	names    map[cedar.PolicyID]string

	// mappings is roleMappings.json, compiled against this load's own
	// roles.json. Its issuer names are not checked here; see
	// newLocalFromSource.
	mappings *roleMappings

	// groupOnlyWrites is every write operation the policy leaves to
	// NKE::Action::"write" (groupOnlyWrites, write_group.go). Not an error: a
	// policy may grant every write on purpose. Reported at startup and on each
	// published reload, because an upgrade that adds a write operation widens
	// such a grant without any change to the bundle.
	groupOnlyWrites []string

	// hash is sha256(policies.cedar || actions.json || roles.json ||
	// roleMappings.json), in that fixed order, over the raw bytes read for
	// this one load -- change detection for the hot-reload path's
	// nke_authz_bundle_loaded_timestamp_seconds gauge
	// (design-authz-hot-reload.md §5), not tamper-proofing. Set only when
	// every file that contributes to it was read successfully; left
	// zero-valued on any load that returned an error, where nothing
	// downstream reads it.
	hash [sha256.Size]byte
}

// resolvedBundleDir pins the directory one load attempt reads all four
// bundle files through, so a ConfigMap volume's atomic "..data" rename cannot
// land between two of the four reads and produce a bundle that mixes two
// generations (design-authz-hot-reload.md §3). kubelet's atomic writer
// publishes an update by writing a full new generation into a fresh,
// timestamped directory and then atomically re-pointing "..data" at it;
// resolving "..data" once, up front, and reading through that resolved
// directory for the rest of this load pins every read to one generation
// regardless of a rename landing mid-load.
//
// Falls back to dir itself only when dir is positively not a ConfigMap mount
// -- os.Readlink on "<dir>/..data" reporting fs.ErrNotExist, the case for a
// plain directory (tests, or a deployment that mounts one directly). Any
// other Readlink error (permission, I/O, ...) is returned as-is and treated
// as a load failure by the caller: silently downgrading to the unpinned path
// on any error would reopen exactly the torn-read race this function exists
// to close, on every reload attempt that happened to hit it.
//
// That fallback path carries none of this function's own torn-read
// protection -- it exists to serve a plain directory at all, not to pin one.
// A real Kubernetes ConfigMap or projected volume, chart-rendered or
// bring-your-own, is always a "..data"-symlinked volume and never reaches
// this branch; nothing in this codebase's supported deployment shapes edits
// a plain directory's files non-atomically out from under a running pod. If
// that ever became a real deployment path, this function would need its own
// protection for it -- do not assume the fallback inherits the ConfigMap
// case's guarantee.
func resolvedBundleDir(dir string) (string, error) {
	target, err := os.Readlink(filepath.Join(dir, "..data"))
	switch {
	case err == nil:
		if filepath.IsAbs(target) {
			return target, nil
		}
		return filepath.Join(dir, target), nil
	case errors.Is(err, fs.ErrNotExist):
		return dir, nil
	default:
		return "", fmt.Errorf("resolving the ConfigMap generation for %s: %w", dir, err)
	}
}

// loadBundle is the single implementation behind both NewLocal (via
// DirBundleSource.Load, bundle.go) and ValidateBundle: parse policies.cedar,
// check its shape, load and merge the entity files (refusing a duplicate
// uid, loadEntities, entitystore.go), and check the resulting entity store
// (roles present, no principal entity, at least one mapping, the
// static-parent referential-integrity check, the action-vs-operations-table
// check -- all checkEntityStore, entitystore.go), plus the entity-instance
// checks ValidateBundle exists to consolidate: an action or role a policy
// names that no entity file declares, and a declared role that no policy ever
// names (directly or through nesting).
//
// Every finding is collected rather than returned on the first one, so a
// caller building one Go error (NewLocal) or a slice (ValidateBundle) can
// choose how to present them without loadBundle knowing which.
func loadBundle(dir string) (*Bundle, []error) {
	if dir == "" {
		return nil, []error{errors.New("local authorizer requires a bundle directory")}
	}

	// Pinned once, up front, for this one load attempt -- see
	// resolvedBundleDir's doc comment. Every read below goes through readDir,
	// never dir directly, so all four files come from the same generation
	// even on a mount whose ConfigMap is being updated concurrently.
	readDir, err := resolvedBundleDir(dir)
	if err != nil {
		return nil, []error{err}
	}

	source, err := os.ReadFile(filepath.Join(readDir, policiesFile))
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
	hasher := sha256.New()
	hasher.Write(source)
	for _, name := range entityFiles {
		raw, err := loadEntities(filepath.Join(readDir, name), entities, declaredIn)
		if err != nil {
			errs = append(errs, err)
			entitiesLoaded = false
			continue
		}
		hasher.Write(raw)
	}

	// Hashed after the entity files, in fixed order, so a mapping-only edit
	// counts as a changed bundle for the reload gauge.
	mappingsRaw, err := os.ReadFile(filepath.Join(readDir, roleMappingsFile))
	mappingsRead := err == nil
	if mappingsRead {
		hasher.Write(mappingsRaw)
	} else {
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
		errs = append(errs, checkActionVocabulary(readDir, uncommented)...)
		errs = append(errs, checkRoleVocabulary(readDir, uncommented)...)
		errs = append(errs, checkRoleReachability(readDir, uncommented)...)
	}

	names := map[cedar.PolicyID]string{}
	for id, policy := range policies.All() {
		if annotated, ok := policy.Annotations()["id"]; ok && annotated != "" {
			names[id] = string(annotated)
		}
	}

	b := &Bundle{
		policies: policies, entities: entities, names: names, mappings: mappings,
		groupOnlyWrites: groupOnlyWrites(policies),
	}
	// Only meaningful, and only computed, once every contributing file was
	// actually read -- a hash over a partial set of files would compare
	// unequal to itself across two failed loads of the identical bundle for
	// no reason a caller could act on.
	if entitiesLoaded && mappingsRead {
		copy(b.hash[:], hasher.Sum(nil))
	}
	return b, errs
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

// stripCedarComments lives in cedar_lex.go: a ~70-line lexer with no
// dependency on load context, independently testable (cedar_lex_test.go),
// pulled out of this file about bundle loading.

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
