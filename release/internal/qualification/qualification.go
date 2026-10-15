// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package qualification reads the files that say what is qualified and turns
// them into a plan.
//
// Three files under release/ each carry one concern. The registry says how each
// component resolves to a version. The policy says which streams exist and how
// much of them a run covers. A subject says what one qualification moves
// together and what is resolved around it. The Planner reads all three and
// resolves a subject over a scope into a Plan, and the same inputs always
// produce the same plan in the same order.
package qualification

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// APIVersion is the group and version every kind carries. There is one, and a
// file naming another is rejected rather than coerced: the alternative is
// reading a future schema with today's structs and silently dropping the
// fields that were the reason for the bump.
const APIVersion = "nvke.nvidia.com/v1alpha1"

// Kinds. Each names qualification, because the group does not: these are the
// qualification tool's files rather than Kubernetes objects, and a bare
// "Registry" in a product group says nothing about which registry it is.
const (
	KindRegistry            = "QualificationRegistry"
	KindQualificationPolicy = "QualificationPolicy"
	KindQualification       = "Qualification"
)

// Metadata names a file and says what it is for.
type Metadata struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// readStrict unmarshals YAML and rejects fields the structs do not declare. A
// misspelled key would otherwise read as an omitted one, and every field here
// is optional-looking enough that the result would be a plan quietly missing
// whatever the typo was meant to say.
func readStrict(path string, into any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if b, err = expand(b); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := yaml.UnmarshalStrict(b, into); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}

// reference matches ${NAME}. Only the braced form is recognized: a bare $NAME
// would also match prose in a comment, and expansion happens before the YAML
// is parsed, so a comment is not yet a comment when this runs.
var reference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expand substitutes ${NAME} from the process environment.
//
// What a component is and how it resolves is engineering content and lives in
// the file. The identifiers of the account its artifacts sit in are not: they
// name one organization's registry, and this repository is the staging ground
// for a public one. So they are supplied at run time, which is already how the
// lane reaches that registry at all.
//
// An unset variable is an error rather than an empty string. Substituting
// nothing would produce a file that parses, and a plan would then name an
// artifact under no org, which fails later against a registry rather than here
// against the file that asked for it. Every missing name is collected, so one
// run reports the whole list.
//
// Set but empty counts as unset, because that is the shape a caller produces:
// an unset repository variable reaches a workflow as the empty string, not as
// absent.
//
// This mirrors the convention the e2e environments already use, so one file
// does not state an identifier a different file asks for.
func expand(data []byte) ([]byte, error) {
	missing := map[string]struct{}{}

	expanded := reference.ReplaceAllFunc(data, func(match []byte) []byte {
		name := string(reference.FindSubmatch(match)[1])
		if value := os.Getenv(name); value != "" {
			return []byte(value)
		}
		missing[name] = struct{}{}
		return match
	})

	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for name := range missing {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("unset or empty: %s", strings.Join(names, ", "))
	}
	return expanded, nil
}

func checkTypeMeta(path, apiVersion, kind, wantKind string) error {
	if apiVersion != APIVersion {
		return fmt.Errorf("%s: apiVersion is %q, want %q", path, apiVersion, APIVersion)
	}
	if kind != wantKind {
		return fmt.Errorf("%s: kind is %q, want %q", path, kind, wantKind)
	}
	return nil
}
