// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

// GitTagOrCommitDescribe resolves a component from the repository's tags, or by describing
// it at a named commit.
//
// Both are live questions. A qualification of a release asks for the highest
// tag, and a qualification of one commit asks what that commit is, which no tag
// answers. Keeping them in two resolvers leaves each with the invariants it can
// actually hold, and leaves this the only place that chooses between them.
//
// When Base and Repository are set, the resolved version also names where the
// artifact is, which is what lets a plan be checked against a registry without
// anything reconstructing the location from a workflow variable.
type GitTagOrCommitDescribe struct {
	// Tag resolves from the repository's tags.
	Tag GitTag

	// Describe resolves by describing a commit.
	Describe CommitDescribe

	// Base is the location the component is published under, expanded from the
	// source its registry entry names. Empty means it publishes nowhere this
	// can name, as os-image does by reaching the CDN through its per-architecture
	// components instead.
	Base string

	// Repository is the component's name under Base. The name is the same
	// wherever the artifacts are pushed, because only the prefix differs
	// between registries.
	Repository string
}

// Resolve describes InputCommitKey when one is given, and otherwise reads the
// tags.
//
// An override goes to Tag either way. It names a tag, which is a question about
// the repository's tags however the rest of the run resolves, and sending it
// anywhere else would make the same override mean two things depending on a
// mode the person naming it did not choose.
func (t GitTagOrCommitDescribe) Resolve(inputs map[string]string) (map[string]string, error) {
	resolver, strategy := Resolver(t.Tag), VersionStrategyGitTag
	if commit := inputs[InputCommitKey]; commit != "" && inputs[InputOverrideKey] == "" {
		resolver, strategy = t.Describe.At(commit), VersionStrategyCommitDescribe
	}

	got, err := resolver.Resolve(inputs)
	if err != nil {
		return nil, err
	}
	got[ResultStrategyKey] = strategy
	return got, nil
}

// Validate holds the declared inputs to the tag resolver. Both arms answer the
// same question about the same component, so there is one answer to give.
func (t GitTagOrCommitDescribe) Validate(inputs map[string]string) error {
	return Validate(t.Tag, inputs)
}
