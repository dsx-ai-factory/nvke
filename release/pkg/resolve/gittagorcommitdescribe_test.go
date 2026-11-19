// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"strings"
	"testing"
)

// newGitTagOrCommitDescribe builds the resolver New builds for a registry entry, so the
// tests exercise the composition the registry actually produces.
func newGitTagOrCommitDescribe(dir, prefix string) GitTagOrCommitDescribe {
	return GitTagOrCommitDescribe{
		Tag:      GitTag{Dir: dir, TagPrefix: prefix},
		Describe: CommitDescribe{Dir: dir, TagPrefix: prefix},
	}
}

func TestTagOrCommitChoosesAnArm(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, "the release")
	tag(t, dir, "controller/v0.2.0-rc.1")
	commit(t, dir, "after the release")
	sha := head(t, dir)

	// A newer tag on a later commit, which sha cannot reach. Without this the
	// two arms would agree and no case would show which one answered.
	commit(t, dir, "the next release")
	tag(t, dir, "controller/v0.3.0")

	for _, tc := range []struct {
		name   string
		inputs map[string]string
		want   string
	}{
		{
			name:   "no commit reads the tags",
			inputs: nil,
			want:   "v0.3.0",
		},
		{
			name:   "a commit describes it",
			inputs: map[string]string{InputCommitKey: sha},
			want:   "v0.2.0-rc.1-1-g",
		},
		{
			name:   "an override reads the tags even with a commit",
			inputs: map[string]string{InputCommitKey: sha, InputOverrideKey: "v0.2.0-rc.1"},
			want:   "v0.2.0-rc.1",
		},
		{
			name:   "an override reads the tags without a commit",
			inputs: map[string]string{InputOverrideKey: "v0.2.0-rc.1"},
			want:   "v0.2.0-rc.1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newGitTagOrCommitDescribe(dir, "controller/").Resolve(tc.inputs)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(got[ResultVersionKey], tc.want) {
				t.Errorf("version = %q, want it to start with %q", got[ResultVersionKey], tc.want)
			}
		})
	}
}

// An override names a tag, so it has to fail the same way in both modes. A
// person who mistypes a version should not be told something different because
// the run happens to name a commit.
func TestTagOrCommitOverrideFailsTheSameWay(t *testing.T) {
	dir := newRepo(t)
	tag(t, dir, "controller/v0.2.0-rc.1")
	sha := head(t, dir)

	resolver := newGitTagOrCommitDescribe(dir, "controller/")
	want := "has no version v0.9.9"

	for _, inputs := range []map[string]string{
		{InputOverrideKey: "v0.9.9"},
		{InputOverrideKey: "v0.9.9", InputCommitKey: sha},
	} {
		_, err := resolver.Resolve(inputs)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("with commit %q: err = %v, want it to contain %q", inputs[InputCommitKey], err, want)
		}
	}
}
