// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

// Plan is what `qualify plan` writes and the e2e qualification workflow reads.
type Plan struct {
	Subject string `json:"subject"`
	Version string `json:"version"`
	Scope   string `json:"scope"`

	// Commit is the commit the components built from this repository were
	// resolved at, when the plan was made that way. Empty means every component
	// resolved from its tags, which is how a release is qualified.
	//
	// Version stays the version a person asked for either way, so a beta is
	// still reported as the beta it is and not as a build identity.
	Commit string `json:"commit,omitempty"`

	// Overrides names the components and dependencies whose version a person
	// chose, in order. Everything else was resolved by its strategy, so a record
	// never credits a strategy with a human's choice.
	Overrides []string `json:"overrides,omitempty"`

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

// ComponentVersion is a component with what it resolved to. A version is always
// a version and a URL is always a location. An image has both, the release it
// was built from and where it is, and a bootloader has only a location.
type ComponentVersion struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	URL     string `json:"url,omitempty"`

	// VersionStrategy and LocationStrategy are what answered each half, as the
	// registry names them. A plan read later says how each value was arrived
	// at, which matters most where one entry can resolve two ways and the
	// version alone does not say which.
	//
	// An entry that takes another entry's version has no VersionStrategy,
	// because nothing resolved one for it.
	VersionStrategy  string `json:"versionStrategy,omitempty"`
	LocationStrategy string `json:"locationStrategy,omitempty"`
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
