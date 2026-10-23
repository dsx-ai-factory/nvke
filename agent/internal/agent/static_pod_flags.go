// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

// Convergent flag manipulation for kubeadm static-pod manifests.
//
// A container's flags live in the keyless command/args string lists, which
// strategic-merge cannot reconcile: merging a list of bare strings either
// replaces it wholesale or appends duplicates. These helpers express the
// mutations as operations on flag NAMES instead, so repeating a call converges
// rather than accumulating.
//
// Everything here is container-agnostic and takes the target container name,
// because both the kube-apiserver and etcd manifests need the same treatment.
// The manifest is handled as JSON maps throughout so fields the vendored
// Kubernetes types do not model survive the round-trip.

// validateFlagName enforces that a flag name is a single bare token: non-empty,
// no whitespace, no leading dashes, no "=". Values are unconstrained.
func validateFlagName(name string) error {
	if name == "" {
		return errors.New("flag name is required")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("flag name must not include leading dashes: %q", name)
	}
	if strings.Contains(name, "=") {
		return fmt.Errorf("flag name must not contain '=': %q", name)
	}
	if len(strings.Fields(name)) != 1 {
		return fmt.Errorf("flag name must be a single token with no whitespace: %q", name)
	}
	return nil
}

// applyFlagOps applies the flag operations to the named container's
// command/args string lists, working on the manifest as a map so unknown fields
// survive. Set operations reconcile the flag where it already lives (command or
// args), else add it to command when command already carries flags (kubeadm's
// canonical layout) and otherwise to args. Remove operations strip the flag from
// both lists.
func applyFlagOps(manifestJSON []byte, containerName string, ops []*pb.FlagOp) ([]byte, error) {
	var doc map[string]interface{}
	if err := json.Unmarshal(manifestJSON, &doc); err != nil {
		return nil, fmt.Errorf("unmarshal manifest: %w", err)
	}
	container, err := findContainerMap(doc, containerName)
	if err != nil {
		return nil, err
	}

	command, err := toStringSlice(container["command"])
	if err != nil {
		return nil, fmt.Errorf("container command: %w", err)
	}
	args, err := toStringSlice(container["args"])
	if err != nil {
		return nil, fmt.Errorf("container args: %w", err)
	}
	_, argsExisted := container["args"]

	for _, op := range ops {
		switch {
		case op.GetSet() != nil:
			command, args = applySetFlag(command, args, op.GetSet().GetName(), op.GetSet().GetValue())
		case op.GetRemove() != nil:
			command = removeFlag(command, op.GetRemove().GetName())
			args = removeFlag(args, op.GetRemove().GetName())
		}
	}

	container["command"] = fromStringSlice(command)
	if len(args) > 0 || argsExisted {
		container["args"] = fromStringSlice(args)
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	return out, nil
}

// readFlagValue returns the value of --<name> on the named container, looking in
// command then args and accepting both the token form (--name=value) and the
// split form (--name value). found=false means the flag is absent, which callers
// must distinguish from an empty value: for a sizing flag it means etcd's
// built-in default applies, not zero.
func readFlagValue(manifestJSON []byte, containerName, flagName string) (string, bool, error) {
	var doc map[string]interface{}
	if err := json.Unmarshal(manifestJSON, &doc); err != nil {
		return "", false, fmt.Errorf("unmarshal manifest: %w", err)
	}
	container, err := findContainerMap(doc, containerName)
	if err != nil {
		return "", false, err
	}
	for _, key := range []string{"command", "args"} {
		list, err := toStringSlice(container[key])
		if err != nil {
			return "", false, fmt.Errorf("container %s: %w", key, err)
		}
		if value, found := flagValueIn(list, flagName); found {
			return value, true, nil
		}
	}
	return "", false, nil
}

// flagValueIn finds --<name> in list and returns its value. A bare boolean flag
// (no value token, or followed by another flag) yields an empty value with
// found=true.
func flagValueIn(list []string, name string) (string, bool) {
	flag := "--" + name
	pfx := flag + "="
	for i, e := range list {
		switch {
		case strings.HasPrefix(e, pfx):
			return strings.TrimPrefix(e, pfx), true
		case e == flag:
			if i+1 < len(list) && !strings.HasPrefix(list[i+1], "-") {
				return list[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

// applySetFlag ensures --<name>=<value> is present exactly once, reconciling the
// flag wherever it currently lives (command preferred over args), or adding it
// to command when command already carries flags, else to args.
func applySetFlag(command, args []string, name, value string) ([]string, []string) {
	// One scan per list: setFlagIfPresent updates in place and reports whether
	// the flag was there, so a present flag is not scanned twice (once to test,
	// once to update).
	if updated, found := setFlagIfPresent(command, name, value); found {
		return updated, args
	}
	if updated, found := setFlagIfPresent(args, name, value); found {
		return command, updated
	}
	// Absent from both lists: append once, preferring command when it already
	// carries flags (kubeadm's canonical layout), else args.
	want := "--" + name + "=" + value
	if commandHasFlags(command) {
		return append(command, want), args
	}
	return command, append(args, want)
}

// setFlagIfPresent updates an existing --<name> occurrence in list — the token
// form (--name=old) or the split form (--name, old) — to --<name>=<value> and
// reports found=true. When the flag is absent it leaves list untouched and
// returns found=false, so the caller decides where to append. Single pass;
// mutates the caller-owned copy that toStringSlice allocated.
func setFlagIfPresent(list []string, name, value string) ([]string, bool) {
	flag := "--" + name
	want := flag + "=" + value
	pfx := flag + "="
	for i, e := range list {
		switch {
		case e == want:
			return list, true
		case strings.HasPrefix(e, pfx):
			list[i] = want
			return list, true
		case e == flag:
			// Split form "--name value": overwrite the following value token only
			// when it is an actual value, not another flag. A bare boolean flag
			// (--name with no value token, or followed by the next flag) is
			// normalized to token form in place so we neither clobber the next
			// flag nor append a duplicate.
			if i+1 < len(list) && !strings.HasPrefix(list[i+1], "-") {
				list[i+1] = value
			} else {
				list[i] = want
			}
			return list, true
		}
	}
	return list, false
}

// ensureFlag makes --<name>=<value> present exactly once in cmd: it updates an
// existing occurrence in place, else appends. Idempotent; operates on the
// caller-owned copy toStringSlice allocated.
func ensureFlag(cmd []string, name, value string) []string {
	if updated, found := setFlagIfPresent(cmd, name, value); found {
		return updated
	}
	return append(cmd, "--"+name+"="+value)
}

// removeFlag strips every occurrence of --<name> from cmd: the token form
// (--name=...), and the split form (--name followed by its value token). It is
// idempotent and compacts cmd in place (callers pass the owned copy
// toStringSlice allocated), reusing the backing array rather than allocating.
func removeFlag(cmd []string, name string) []string {
	flag := "--" + name
	pfx := flag + "="
	out := cmd[:0]
	for i := 0; i < len(cmd); i++ {
		e := cmd[i]
		if e == flag {
			// Split form: also skip a following value token that is not itself
			// a flag (a bare boolean flag has no value token to consume).
			if i+1 < len(cmd) && !strings.HasPrefix(cmd[i+1], "-") {
				i++
			}
			continue
		}
		if strings.HasPrefix(e, pfx) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// commandHasFlags reports whether command has at least one element after the
// bare executable that starts with "--". kubeadm-rendered apiserver and etcd
// static pods satisfy this; split-shape (command: [exe], args: [--flags...])
// does not.
func commandHasFlags(command []string) bool {
	if len(command) < 2 {
		return false
	}
	for _, a := range command[1:] {
		if strings.HasPrefix(a, "--") {
			return true
		}
	}
	return false
}

// findContainerMap locates the named container's map in the manifest doc. It
// prefers an exact name match; failing that it tolerates a single-container
// manifest, but refuses a multi-container manifest where the target cannot be
// chosen deterministically.
func findContainerMap(doc map[string]interface{}, containerName string) (map[string]interface{}, error) {
	containers, err := containerList(doc)
	if err != nil {
		return nil, err
	}
	for _, cm := range containers {
		if name, _ := cm["name"].(string); name == containerName {
			return cm, nil
		}
	}
	if len(containers) == 1 {
		return containers[0], nil
	}
	return nil, fmt.Errorf(
		"manifest has %d containers and none is named %q",
		len(containers), containerName,
	)
}

// containerList returns .spec.containers as a slice of maps.
func containerList(doc map[string]interface{}) ([]map[string]interface{}, error) {
	spec, ok := doc["spec"].(map[string]interface{})
	if !ok {
		return nil, errors.New("manifest has no spec object")
	}
	raw, ok := spec["containers"].([]interface{})
	if !ok || len(raw) == 0 {
		return nil, errors.New("manifest has no containers")
	}
	out := make([]map[string]interface{}, 0, len(raw))
	for i, rc := range raw {
		cm, ok := rc.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("container %d is not an object", i)
		}
		out = append(out, cm)
	}
	return out, nil
}

// jsonSemanticEqual reports whether two JSON documents are structurally equal,
// ignoring key ordering and whitespace.
func jsonSemanticEqual(a, b []byte) bool {
	var av, bv interface{}
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// toStringSlice converts a JSON array of strings to []string. A nil/absent value
// yields (nil, nil).
func toStringSlice(v interface{}) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected a list, got %T", v)
	}
	out := make([]string, 0, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("element %d is not a string (%T)", i, e)
		}
		out = append(out, s)
	}
	return out, nil
}

// fromStringSlice converts []string to a JSON-marshalable []interface{}.
func fromStringSlice(s []string) []interface{} {
	out := make([]interface{}, len(s))
	for i, e := range s {
		out[i] = e
	}
	return out
}
