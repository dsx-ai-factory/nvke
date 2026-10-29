// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package envalias reads nvkectl's settings from the environment and, for one
// release, accepts each setting under the name it had while the binary was
// called nkectl: NVKECTL_X is read first, then NKECTL_X. The command that
// reads a setting under its previous name warns once, naming the replacement,
// so the old names can be dropped in the release after this one. A setting
// named NVKECTL_NVKE_* was added after the rename and has no previous name.
package envalias

import (
	"os"
	"sort"
	"strings"
)

const (
	prefix     = "NVKECTL_"
	oldPrefix  = "NKECTL_"
	prefixLen  = len(prefix)
	newPrefix  = "NVKECTL_NVKE_" // settings added after the rename
	deprecated = " is deprecated and will be removed in a future release; set "
)

// Old returns the name a setting had before the rename, or "" when the name
// does not carry the prefix the rename changed or the setting is NVKECTL_NVKE_*.
func Old(name string) string {
	if !strings.HasPrefix(name, prefix) || strings.HasPrefix(name, newPrefix) {
		return ""
	}
	return oldPrefix + name[prefixLen:]
}

// Get returns the setting's value, or, when it is unset or empty, the value
// under its previous name.
func Get(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	if old := Old(name); old != "" {
		return os.Getenv(old)
	}
	return ""
}

// Renamed is a setting that is set only under its previous name.
type Renamed struct {
	Old, New string
}

// Warning is the text a command logs for a Renamed setting.
func (r Renamed) Warning() string {
	return r.Old + deprecated + r.New + " instead"
}

// Deprecated returns, sorted by old name, every given setting that is set
// under its previous name while the current name is unset or empty.
func Deprecated(names ...string) []Renamed {
	var found []Renamed
	seen := map[string]bool{}
	for _, name := range names {
		old := Old(name)
		if old == "" || seen[name] {
			continue
		}
		seen[name] = true
		if os.Getenv(name) == "" && os.Getenv(old) != "" {
			found = append(found, Renamed{Old: old, New: name})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Old < found[j].Old })
	return found
}
