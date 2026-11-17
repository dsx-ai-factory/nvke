// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/pkg/resolve"
)

// gitIn runs git in dir and returns what it printed, failing the test if it
// fails. Signing is off so a developer's own git configuration cannot reach it.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{
		"-c", "user.name=test", "-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
	}, args...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// newTaggedRepo makes a repository with one commit that the tag names, and
// returns its directory and that commit's full hash.
func newTaggedRepo(t *testing.T, tag string) (dir, commit string) {
	t.Helper()
	dir = t.TempDir()
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	gitIn(t, dir, "tag", tag)
	return dir, gitIn(t, dir, "rev-parse", "HEAD")
}

// writeSubjectApart writes a subject spec outside any tree a planner resolves
// against, and returns its path.
func writeSubjectApart(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qualification.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// subjectAt resolves the subject to a version and, when ref is not empty, the
// ref that version describes components at.
func subjectAt(ref string) *fake {
	return &fake{answer: func(map[string]string) (map[string]string, error) {
		out := map[string]string{resolve.ResultVersionKey: "v0.22.0-beta.1"}
		if ref != "" {
			out[resolve.ResultRefKey] = ref
		}
		return out, nil
	}}
}

// Planning at a commit takes it from the ref the subject's resolver returns,
// with the subject's spec kept apart from the tree that ref is looked up in.
// What a resolver that returns no ref, or a ref outside that tree, causes is
// named, and a plan that does not describe at a commit needs neither.
func TestPlanAtCommit(t *testing.T) {
	root, head := newTaggedRepo(t, "v0.22.0-beta.1")

	tests := []struct {
		name       string
		atCommit   bool
		ref        string
		wantCommit string
		wantErr    string
	}{
		{"records the commit the subject's ref names", true, "v0.22.0-beta.1", head, ""},
		{"names a resolver that returns no ref", true, "", "", "returned no ref to describe it at"},
		{"names a ref that is not in the tree at Root", true, "v9.9.9", "", "v9.9.9 is not a commit in this repository"},
		{"plans without a commit when not describing at one", false, "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newResolvers()
			r["nvke"] = subjectAt(tc.ref)
			p := newPlanner(t, testSubject, r)
			p.Root = root
			p.SubjectFile = writeSubjectApart(t, testSubject)
			p.ResolveAtCommit = tc.atCommit

			got, err := p.Plan("nvke", "", "full")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Commit != tc.wantCommit {
				t.Errorf("the plan records commit %q, want %q", got.Commit, tc.wantCommit)
			}
			if tc.wantCommit != "" {
				// The commit is offered to the resolvers, which is how a
				// component is described at it.
				if len(r["cli"].calls) == 0 || r["cli"].calls[0][resolve.InputCommitKey] != tc.wantCommit {
					t.Errorf("the cli resolver was not offered commit %q: %v", tc.wantCommit, r["cli"].calls)
				}
			}
		})
	}
}

// A Planner used again starts from nothing. The commit one plan resolved at is
// neither recorded in the next nor offered to its resolvers.
func TestPlanDoesNotCarryACommitToTheNextPlan(t *testing.T) {
	root, head := newTaggedRepo(t, "v0.22.0-beta.1")
	r := newResolvers()
	r["nvke"] = subjectAt("v0.22.0-beta.1")
	p := newPlanner(t, testSubject, r)
	p.Root = root
	p.SubjectFile = writeSubjectApart(t, testSubject)

	p.ResolveAtCommit = true
	first, err := p.Plan("nvke", "", "full")
	if err != nil {
		t.Fatal(err)
	}
	if first.Commit != head {
		t.Fatalf("the first plan records commit %q, want %q", first.Commit, head)
	}

	r["cli"].calls = nil
	p.ResolveAtCommit = false
	second, err := p.Plan("nvke", "", "full")
	if err != nil {
		t.Fatal(err)
	}
	if second.Commit != "" {
		t.Errorf("the second plan records commit %q, want none", second.Commit)
	}
	if len(r["cli"].calls) == 0 {
		t.Fatal("the cli resolver was never asked, so nothing was checked")
	}
	for _, in := range r["cli"].calls {
		if c := in[resolve.InputCommitKey]; c != "" {
			t.Errorf("the cli resolver was offered the last plan's commit %q", c)
		}
	}
}
