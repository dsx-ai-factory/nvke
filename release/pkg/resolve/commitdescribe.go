// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"bytes"
	"errors"
	"fmt"
	"math/bits"
	"os/exec"
	"strings"
	"sync"
)

// CommitDescribe names a component at one commit, as git describe reports it.
//
// A commit between releases has no tag of its own, so there is no version to
// look up. Describe answers with the nearest reachable tag, the number of
// commits since it, and the commit itself: "v0.2.0-rc.1-7-g21afa494". That
// string is what controller/Makefile stamps into BUILD_VERSION, and what every
// publisher tags the artifact it builds from that commit, so it is the one
// identity a deployed component reports and a registry can be asked for.
//
// This is a different question from GitTag's, which picks the highest tag in
// the repository and never looks at a commit. Neither subsumes the other, and
// GitTagOrCommitDescribe is where the choice between them is made.
type CommitDescribe struct {
	// Dir is the repository to read. Empty means the working directory.
	Dir string

	// TagPrefix is everything in a tag before the version, and is also what
	// restricts which tags the description is measured against.
	TagPrefix string

	// Commit is what to describe. At sets it.
	Commit string
}

// At returns a copy that describes commit, so one configured resolver can serve
// a run that names a commit and one that does not.
func (c CommitDescribe) At(commit string) CommitDescribe {
	c.Commit = commit
	return c
}

// Resolve returns the description of Commit and the commit it was taken at.
//
// The ref is the commit and not TagPrefix + version, which is what GitTag
// returns, because a description is not a tag and no ref by that name exists.
// A dependency reading "<component>.ref" has to reach the tree the version was
// built from, and the commit is that tree without relying on git parsing a
// description back into one.
//
// A commit before the component's first tag has nothing to be described
// against, and --always then answers with the abbreviated commit alone. That is
// what the publishers fall back to as well, so the identity still matches the
// artifact they pushed.
//
// It reads no input. The commit is on the resolver, and an override names a tag,
// which GitTagOrCommitDescribe sends to the tag resolver instead.
func (c CommitDescribe) Resolve(map[string]string) (map[string]string, error) {
	if c.Commit == "" {
		return nil, fmt.Errorf("%s has no commit to describe", c.name())
	}

	described, err := c.describe()
	if err != nil {
		return nil, err
	}
	commit, err := CommitOf(c.Dir, c.Commit)
	if err != nil {
		return nil, err
	}
	return map[string]string{ResultVersionKey: described, ResultRefKey: commit}, nil
}

// describe runs git describe the way the publishers do, so the version this
// returns is the one they tagged the artifact with.
func (c CommitDescribe) describe() (string, error) {
	args := []string{"describe", "--tags", "--always", "--match", c.TagPrefix + "v*"}
	n, err := originAbbrev(c.Dir)
	if err != nil {
		return "", err
	}
	if n > 0 {
		args = append(args, fmt.Sprintf("--abbrev=%d", n))
	}
	cmd := exec.Command("git", append(args, c.Commit)...)
	cmd.Dir = c.Dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("describing %s at %s: %w", c.name(), c.Commit, err)
	}

	described := strings.TrimSpace(string(out))
	if described == "" {
		return "", fmt.Errorf("describing %s at %s returned nothing", c.name(), c.Commit)
	}
	return strings.TrimPrefix(described, c.TagPrefix), nil
}

// originAbbrevs holds originAbbrev's answer per repository: counting every
// object is the slow part of a plan, and the answer is the same for each
// component described.
var originAbbrevs sync.Map

// originAbbrev is the hash length git describe picks in a fresh clone of
// upstream, which is where every publisher describes. Git sizes it from the
// clone's object count, so a clone that has fetched forks as well describes
// the same commit a digit longer and names an artifact nobody published.
// Zero, with no upstream refs, leaves the length to git.
func originAbbrev(dir string) (int, error) {
	if n, ok := originAbbrevs.Load(dir); ok {
		return n.(int), nil
	}
	remote, err := UpstreamRemote(dir)
	if errors.Is(err, ErrNoUpstreamRemote) {
		remote = "origin"
	} else if err != nil {
		return 0, err
	}
	cmd := exec.Command("git", "rev-list", "--objects", "--remotes="+remote, "--tags")
	cmd.Dir = dir
	var lines lineCounter
	cmd.Stdout = &lines
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("counting the objects a clone of %s holds: %w", remote, err)
	}
	n := abbrevFor(int(lines))
	originAbbrevs.Store(dir, n)
	return n, nil
}

// lineCounter counts the lines written to it and keeps none of them: a clone
// lists millions of objects, and only their number matters.
type lineCounter int

func (c *lineCounter) Write(p []byte) (int, error) {
	*c += lineCounter(bytes.Count(p, []byte("\n")))
	return len(p), nil
}

// UpstreamRepository is the path every publisher clones.
const UpstreamRepository = "dsx-ai-factory/nvke"

// ErrNoUpstreamRemote is UpstreamRemote's error when no remote's URL names
// UpstreamRepository. The planner then takes origin to be upstream, as it is
// in CI; a run from a fork stops instead, since its origin is the fork.
var ErrNoUpstreamRemote = errors.New("no remote's URL names " + UpstreamRepository +
	"; add one: git remote add upstream https://github.com/" + UpstreamRepository + ".git")

// UpstreamRemote is the remote whose URL names UpstreamRepository. A clone
// made to work from a fork has the fork as origin, so the name alone says
// nothing.
func UpstreamRemote(dir string) (string, error) {
	cmd := exec.Command("git", "config", "--get-regexp", `^remote\..*\.url$`)
	cmd.Dir = dir
	out, err := cmd.Output()
	// git config exits 1 when nothing matches, which is a clone with no
	// remotes; anything else is a failure to read the config at all.
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		return "", fmt.Errorf("listing the remotes: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, url, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		url = strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git")
		if strings.HasSuffix(url, "/"+UpstreamRepository) || strings.HasSuffix(url, ":"+UpstreamRepository) {
			return strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".url"), nil
		}
	}
	return "", ErrNoUpstreamRemote
}

// abbrevFor is git's own default for a repository of count objects: half the
// count's bit length, rounded up, and never under seven.
func abbrevFor(count int) int {
	if count == 0 {
		return 0
	}
	return max((bits.Len(uint(count))+1)/2, 7)
}

// CommitOf expands any commit-ish to the full hash, so a plan records the same
// value whether it was named by a tag, a branch or an abbreviation.
//
// A tag resolves to the commit it points at, which is how the commit a run
// describes its components at is derived from the version it is qualifying.
func CommitOf(dir, ref string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--verify", ref+"^{commit}")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s is not a commit in this repository: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// name is how an error refers to what is being described.
func (c CommitDescribe) name() string {
	if c.TagPrefix == "" {
		return "the repository"
	}
	return strings.TrimSuffix(c.TagPrefix, "/")
}

// Validate accepts no declared input, because a description is found from the
// prefix and the commit alone.
func (CommitDescribe) Validate(inputs map[string]string) error { return takesOnly(inputs) }
