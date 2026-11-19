// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	_ "embed"
	"errors"
	"fmt"
	"regexp"

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

// Register is the set of kube-bench findings NVKE accepts, with the reason for
// each.
type Register struct {
	Exceptions []Exception `json:"exceptions"`
}

// Exception accepts one check's FAIL or WARN, on every node of every cluster.
type Exception struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
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
		for j := i + 1; j < len(r.Exceptions); j++ {
			if r.Exceptions[j].ID == e.ID {
				errs = append(errs, fmt.Errorf("exception %s: listed more than once, as entries %d and %d", name, i, j))
			}
		}
	}
	return errors.Join(errs...)
}
