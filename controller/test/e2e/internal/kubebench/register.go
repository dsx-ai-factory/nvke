// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"sigs.k8s.io/yaml"
)

var (
	// defaultRegister is the exception register the CI lanes judge scans
	// against.
	//
	//go:embed exceptions.yaml
	defaultRegister []byte

	// checkID is a kube-bench check ID: dotted numbers, such as 1.2.11.
	checkID = regexp.MustCompile(`^\d+(\.\d+)+$`)
)

const (
	// reviewByLayout is the format of an exception's review date.
	reviewByLayout = "2006-01-02"
)

// Register is the set of kube-bench findings NVKE accepts, with the reason for
// each.
type Register struct {
	Exceptions []Exception `json:"exceptions"`
}

// Exception accepts one check's FAIL or WARN where its scope applies.
type Exception struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Scope    Scope  `json:"scope"`
	Reason   string `json:"reason"`
	Approver string `json:"approver"`
	ReviewBy string `json:"reviewBy"`
}

// Scope is where an exception applies. An empty list applies to every value.
type Scope struct {
	Clusters []Cluster `json:"clusters,omitempty"`
	Roles    []Role    `json:"roles,omitempty"`
}

// Covers reports whether the scope applies to a node of role r in a cluster of
// kind c.
func (s Scope) Covers(c Cluster, r Role) bool {
	return (len(s.Clusters) == 0 || slices.Contains(s.Clusters, c)) &&
		(len(s.Roles) == 0 || slices.Contains(s.Roles, r))
}

// overlaps reports whether some node is in both scopes.
func (s Scope) overlaps(o Scope) bool {
	return listsMeet(s.Clusters, o.Clusters) && listsMeet(s.Roles, o.Roles)
}

func listsMeet[T comparable](a, b []T) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, v := range a {
		if slices.Contains(b, v) {
			return true
		}
	}
	return false
}

// LoadRegister decodes a register, rejecting unknown fields, and validates it.
func LoadRegister(data []byte) (Register, error) {
	if err := checkIDsAreStrings(data); err != nil {
		return Register{}, err
	}
	var reg Register
	if err := yaml.UnmarshalStrict(data, &reg); err != nil {
		return Register{}, fmt.Errorf("decoding the exception register: %w", err)
	}
	for i := range reg.Exceptions {
		reg.Exceptions[i].ReviewBy = normaliseDate(reg.Exceptions[i].ReviewBy)
	}
	if err := reg.Validate(); err != nil {
		return Register{}, err
	}
	return reg, nil
}

// DefaultRegister is the register embedded from exceptions.yaml.
func DefaultRegister() (Register, error) {
	return LoadRegister(defaultRegister)
}

// checkIDsAreStrings rejects an unquoted id that YAML reads as a number, such
// as 5.1. It is the only guard against one: decoding the register converts
// the number into a string without complaint, and 5.10 becomes "5.1", which
// silently names a different check.
func checkIDsAreStrings(data []byte) error {
	var raw struct {
		Exceptions []map[string]any `json:"exceptions"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decoding the exception register: %w", err)
	}
	var errs []error
	for i, entry := range raw.Exceptions {
		if id, ok := entry["id"]; ok {
			if _, isString := id.(string); !isString {
				errs = append(errs, fmt.Errorf("exceptions[%d]: YAML read id as the number %v, which may not be the check meant; quote the ID exactly as written, as in id: \"1.2.11\"", i, id))
			}
		}
	}
	return errors.Join(errs...)
}

// normaliseDate turns an unquoted YAML date, which may arrive as an RFC 3339
// timestamp at midnight UTC, back into the date it was written as.
func normaliseDate(value string) string {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil || !t.Equal(t.Truncate(24*time.Hour)) {
		return value
	}
	return t.UTC().Format(reviewByLayout)
}

// Validate reports every problem in the register, each naming its entry.
func (r Register) Validate() error {
	var errs []error
	for i, e := range r.Exceptions {
		name := e.ID
		if name == "" {
			name = fmt.Sprintf("exceptions[%d]", i)
		}
		if !checkID.MatchString(e.ID) {
			errs = append(errs, fmt.Errorf("exception %q: id is not dotted numbers such as 1.2.11", e.ID))
		}
		if e.Title == "" {
			errs = append(errs, fmt.Errorf("exception %s: title is empty", name))
		}
		if e.Reason == "" {
			errs = append(errs, fmt.Errorf("exception %s: reason is empty", name))
		}
		if _, err := time.Parse(reviewByLayout, e.ReviewBy); err != nil {
			errs = append(errs, fmt.Errorf("exception %s: reviewBy %q is not a date such as 2027-05-04", name, e.ReviewBy))
		}
		for _, c := range e.Scope.Clusters {
			if c != ClusterSite && c != ClusterTenant {
				errs = append(errs, fmt.Errorf("exception %s: cluster %q is not %s or %s", name, c, ClusterSite, ClusterTenant))
			}
		}
		for _, role := range e.Scope.Roles {
			if role != RoleControlPlane && role != RoleWorker {
				errs = append(errs, fmt.Errorf("exception %s: role %q is not %s or %s", name, role, RoleControlPlane, RoleWorker))
			}
		}
		for j := i + 1; j < len(r.Exceptions); j++ {
			if o := r.Exceptions[j]; o.ID == e.ID && e.Scope.overlaps(o.Scope) {
				errs = append(errs, fmt.Errorf("exception %s: entries %d and %d overlap in scope", name, i, j))
			}
		}
	}
	return errors.Join(errs...)
}
