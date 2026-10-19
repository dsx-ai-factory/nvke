// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package deploypath

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/dsx-ai-factory/nvke/cli/internal/stackartifacts"
)

// Binding states.
const (
	// StateBound: the artifact exists for the architecture.
	StateBound = "bound"
	// StateMissing: a required artifact the file lacks; the render refuses.
	StateMissing = "missing"
	// StateCommitted: a chart the file lacks; it comes from the deploy path
	// or its repository.
	StateCommitted = "committed"
	// StateOtherArch: the artifact exists only for other architectures; the
	// render refuses. The state carries them: other-arch:<platforms>.
	StateOtherArch = "other-arch"
	// StateInvalid: the artifact is in the file and cannot be bound, for the
	// reason the state carries: invalid:<reason>. An artifact of another
	// kind than the role takes, or one without a reference.
	StateInvalid = "invalid"
)

// Binding is one artifact a deploy-path item names, resolved against a
// stack artifacts file for one architecture.
type Binding struct {
	// Item is the deploy-path item; Role is what the artifact is to it:
	// chart, image or profiles-image.
	Item, Role string
	// Artifact is the name the item binds; Resolved is the artifact Select
	// chose for it, which may be a per-architecture sibling, or "" when none.
	Artifact, Resolved string
	// State is one of the State constants, other-arch with its platforms and
	// invalid with its reason.
	State string
}

// Bindings resolves every artifact the deploy path under dir binds against
// r for arch, in item order, and returns with them the names of the
// artifacts no item binds. The render makes the same selections. A
// per-architecture sibling of a bound name is bound, not unused.
func Bindings(dir string, r *stackartifacts.Set, arch string) ([]Binding, []string, error) {
	items, err := Items(dir)
	if err != nil {
		return nil, nil, err
	}
	var bindings []Binding
	used := map[string]bool{}
	bind := func(item, role, name string, required bool, kind string) {
		b := Binding{Item: item, Role: role, Artifact: name}
		for _, c := range r.Candidates(name) {
			used[c.Name] = true
		}
		a, err := r.SelectKind(name, kind, arch)
		switch {
		case err == nil:
			b.Resolved, b.State = a.Name, StateBound
		case errors.Is(err, stackartifacts.ErrNotProvided) && required:
			b.State = StateMissing
		case errors.Is(err, stackartifacts.ErrNotProvided):
			b.State = StateCommitted
		case errors.Is(err, stackartifacts.ErrOtherArch):
			var platforms []string
			for _, c := range r.Candidates(name) {
				platforms = append(platforms, c.PublishedPlatforms()...)
			}
			b.State = StateOtherArch + ":" + strings.Join(platforms, ",")
		default:
			b.State = StateInvalid + ":" + err.Error()
		}
		bindings = append(bindings, b)
	}
	for _, item := range items {
		switch item.Type {
		case TypeHelm:
			rel, err := ReadRelease(filepath.Join(dir, item.Name))
			if err != nil {
				return nil, nil, err
			}
			if rel.Chart == nil {
				continue
			}
			bind(item.Name, "chart", firstNonEmpty(rel.Chart.Artifact, item.Name), false, stackartifacts.KindChart)
			if rel.Chart.ImageArtifact != "" {
				bind(item.Name, "image", rel.Chart.ImageArtifact, true, stackartifacts.KindImage)
			}
			if rel.Chart.ProfilesArtifact != "" {
				bind(item.Name, "profiles-image", rel.Chart.ProfilesArtifact, true, stackartifacts.KindImage)
			}
		case TypeManifest:
			if _, err := os.Stat(filepath.Join(dir, item.Name, "release.yaml")); err != nil {
				continue
			}
			rel, err := ReadRelease(filepath.Join(dir, item.Name))
			if err != nil {
				return nil, nil, err
			}
			if rel.Artifact != "" {
				bind(item.Name, "image", rel.Artifact, true, stackartifacts.KindImage)
			}
		}
	}
	var unused []string
	for _, a := range r.Artifacts {
		if !used[a.Name] {
			unused = append(unused, a.Name)
		}
	}
	return bindings, unused, nil
}
