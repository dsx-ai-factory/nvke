// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"os/exec"
	"strings"
	"testing"
)

// commit adds an empty commit, so a description has a distance to report.
func commit(t *testing.T, dir, message string) {
	t.Helper()
	git(t, dir, "commit", "-q", "--allow-empty", "-m", message)
}

// head returns the full hash of the repository's current commit.
func head(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestCommitDescribeResolve(t *testing.T) {
	// One tagged commit, then two more, so the same repository holds a commit
	// that is a tag, a commit past one, and a commit before the first tag.
	dir := newRepo(t)
	beforeAnyTag := head(t, dir)

	commit(t, dir, "the release")
	tag(t, dir, "controller/v0.2.0-rc.1", "cli/v0.1.0")
	atTheTag := head(t, dir)

	commit(t, dir, "after the release")
	commit(t, dir, "after that")
	pastTheTag := head(t, dir)

	for _, tc := range []struct {
		name   string
		prefix string
		commit string
		want   string
		wantEr string
	}{
		{
			name:   "a commit that is the tag describes as the version alone",
			prefix: "controller/",
			commit: atTheTag,
			want:   "v0.2.0-rc.1",
		},
		{
			name:   "a commit past the tag carries the distance and the commit",
			prefix: "controller/",
			commit: pastTheTag,
			want:   "v0.2.0-rc.1-2-g",
		},
		{
			name:   "the prefix chooses which tags are measured against",
			prefix: "cli/",
			commit: pastTheTag,
			want:   "v0.1.0-2-g",
		},
		{
			name:   "a commit before the first tag describes as the commit alone",
			prefix: "controller/",
			commit: beforeAnyTag,
			want:   "",
		},
		{
			name:   "a commit that is not in the repository is an error",
			prefix: "controller/",
			commit: "0000000000000000000000000000000000000000",
			wantEr: "describing controller at",
		},
		{
			name:   "no commit is an error rather than a guess",
			prefix: "controller/",
			commit: "",
			wantEr: "has no commit to describe",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CommitDescribe{Dir: dir, TagPrefix: tc.prefix, Commit: tc.commit}.Resolve(nil)
			if tc.wantEr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantEr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantEr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}

			version := got[ResultVersionKey]
			if !strings.HasPrefix(version, tc.want) {
				t.Errorf("version = %q, want it to start with %q", version, tc.want)
			}
			// The prefix is stripped, as controller/Makefile strips it for
			// BUILD_VERSION, so the version is what the binary reports.
			if strings.HasPrefix(version, tc.prefix) && tc.prefix != "" {
				t.Errorf("version = %q, want the %q prefix stripped", version, tc.prefix)
			}
			// The ref is the commit and not a tag, because a description names
			// no ref, and a dependency reading it has to reach the tree.
			if got[ResultRefKey] != tc.commit {
				t.Errorf("ref = %q, want the commit %q", got[ResultRefKey], tc.commit)
			}
		})
	}
}

// A description ends in the abbreviated commit, which is what makes it both an
// image tag and something git resolves back to the tree it was built from.
func TestCommitDescribeNamesTheCommit(t *testing.T) {
	dir := newRepo(t)
	tag(t, dir, "controller/v0.2.0-rc.1")
	commit(t, dir, "after the release")
	sha := head(t, dir)

	got, err := CommitDescribe{Dir: dir, TagPrefix: "controller/", Commit: sha}.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}

	version := got[ResultVersionKey]
	short, ok := strings.CutPrefix(version, "v0.2.0-rc.1-1-g")
	if !ok {
		t.Fatalf("version = %q, want v0.2.0-rc.1-1-g<short sha>", version)
	}
	if !strings.HasPrefix(sha, short) {
		t.Errorf("version names %q, which does not abbreviate the commit %q", short, sha)
	}
}

// An abbreviation resolves to the same plan as the full hash, so a run records
// one value whichever the caller named.
func TestCommitDescribeExpandsTheCommit(t *testing.T) {
	dir := newRepo(t)
	tag(t, dir, "controller/v0.2.0-rc.1")
	commit(t, dir, "after the release")
	sha := head(t, dir)

	got, err := CommitDescribe{Dir: dir, TagPrefix: "controller/", Commit: sha[:8]}.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got[ResultRefKey] != sha {
		t.Errorf("ref = %q, want the full hash %q", got[ResultRefKey], sha)
	}
}
