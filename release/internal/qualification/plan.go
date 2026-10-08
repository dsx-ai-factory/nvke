// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

// Plan is what `qualify plan` writes and every later verb reads.
type Plan struct {
	Subject      string        `json:"subject"`
	Version      string        `json:"version"`
	Scope        string        `json:"scope"`
	Combinations []Combination `json:"combinations"`
}

// Combination is one resolved set of components, to be qualified against every
// stream in the scope.
type Combination struct {
	// ID names the combination by the versions of its components, so the id in
	// a plan and the id in a published record are the same string without
	// either side having to look the other up.
	ID string `json:"id"`

	// Components are the ones the combination is keyed by.
	Components []ComponentVersion `json:"components"`

	// Dependencies are the ones that do not depend on the stream.
	Dependencies []ComponentVersion `json:"dependencies,omitempty"`

	Streams []Stream `json:"streams"`
}

// ComponentVersion is a component with the version it resolved to.
type ComponentVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Stream is one stream of a combination, with what resolved for it.
type Stream struct {
	Name string `json:"name"`

	// Capabilities is what the stream declared in the policy, copied here so
	// the run that executes this stream is configured from the plan rather than
	// by re-reading the policy. Resolution happens once, and a policy edited
	// between the plan and the last stream must not change what the last stream
	// runs.
	Capabilities map[string]string `json:"capabilities"`

	// Dependencies are the ones that depend on this stream.
	Dependencies []ComponentVersion `json:"dependencies,omitempty"`
}
