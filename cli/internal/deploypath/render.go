// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package deploypath

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
)

// RenderOptions drive Render.
type RenderOptions struct {
	// DeployPath is the committed deploy path; it is never written.
	DeployPath string
	// Site is the site spec whose controllerStack section describes the
	// site the stack is installed for.
	Site *sitespec.SiteSpec
	// Out is the directory to write; it must not exist and must not be
	// under a shared temp directory.
	Out string
	// AllowTempOut accepts an Out under a temp directory. Tests only: their
	// scratch directories live there.
	AllowTempOut bool
	// Log receives one line per decision.
	Log io.Writer
}

// sharedTempDirs are the temp directories every user of a host can read.
var sharedTempDirs = []string{"/tmp", "/var/tmp", "/private/tmp", "/private/var/tmp"}

// underSharedTemp reports whether path is in a shared temp directory or in
// the process's temp directory.
func underSharedTemp(path string) bool {
	dirs := sharedTempDirs
	if tmp, err := filepath.Abs(os.TempDir()); err == nil {
		dirs = append(dirs, tmp)
	}
	for _, dir := range dirs {
		if path == dir || strings.HasPrefix(path, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Rendered reports what Render wrote.
type Rendered struct {
	// Notes are the decisions worth telling the operator.
	Notes []string
	// Credentials lists the files under Out that carry a site credential,
	// relative to Out, in the order they were written.
	Credentials []string
}

// Render writes a copy of the deploy path under opts.Out, resolved for the
// site: every item that takes site values gets a site-values.yaml appended
// to its values, and every template item is rendered against the site and
// becomes a manifest item. The committed files are copied, never edited.
// The result holds the site's secrets, so it is written 0700 and is meant
// for one install. Every problem is reported at once and nothing is left
// under Out on failure.
func Render(opts RenderOptions) (*Rendered, error) {
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if opts.Site == nil {
		return nil, errors.New("a site spec is required")
	}
	deployPath, err := filepath.Abs(opts.DeployPath)
	if err != nil {
		return nil, err
	}
	// A symlinked checkout is walked and joined through its target.
	if resolved, err := filepath.EvalSymlinks(deployPath); err == nil {
		deployPath = resolved
	}
	opts.DeployPath = deployPath
	out, err := filepath.Abs(opts.Out)
	if err != nil {
		return nil, err
	}
	opts.Out = out
	if !opts.AllowTempOut && underSharedTemp(out) {
		return nil, fmt.Errorf("%s is under a shared temp directory; render a site's credentials under a directory only you can read, for example ~/.local/state/nvke/renders/<site>", out)
	}
	if rel, err := filepath.Rel(realPath(deployPath), realPath(out)); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s is inside the deploy path %s; render writes beside it, not into it", out, deployPath)
	}
	items, err := Items(deployPath)
	if err != nil {
		return nil, err
	}
	// Mkdir, not MkdirAll: an existing directory is refused, and the
	// cleanup below removes only what this run created.
	if err := os.Mkdir(out, 0o700); err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("%s exists; render writes a new directory, so pass another --out or remove it", out)
		}
		return nil, err
	}
	if err := copyTree(deployPath, out); err != nil {
		_ = os.RemoveAll(out)
		return nil, err
	}
	r := &renderer{opts: opts, items: items, out: &Rendered{}}
	r.absoluteChartPaths()
	r.site()
	if len(r.problems) > 0 {
		_ = os.RemoveAll(opts.Out)
		return nil, fmt.Errorf("nothing was rendered:\n  %s", strings.Join(r.problems, "\n  "))
	}
	if err := r.finish(); err != nil {
		_ = os.RemoveAll(opts.Out)
		return nil, err
	}
	return r.out, nil
}

type renderer struct {
	opts     RenderOptions
	items    []Item
	out      *Rendered
	problems []string
	// itemsChanged is set when an item's type changed, so finish rewrites
	// the item list.
	itemsChanged bool
}

func (r *renderer) problem(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

// credential records a file under Out that carries a site credential.
func (r *renderer) credential(rel string) {
	r.out.Credentials = append(r.out.Credentials, filepath.ToSlash(rel))
}

func (r *renderer) note(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.out.Notes = append(r.out.Notes, line)
	fmt.Fprintln(r.opts.Log, line)
}

// realPath resolves symlinks in path through its nearest existing ancestor,
// so two spellings of one directory compare equal.
func realPath(path string) string {
	rest := ""
	for p := path; ; {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent, base := filepath.Dir(p), filepath.Base(p)
		if parent == p {
			return path
		}
		rest = filepath.Join(base, rest)
		p = parent
	}
}

// absoluteChartPaths rewrites a committed chart.path, relative to the
// deploy path, as an absolute path in the rendered copy: the executor
// resolves chart.path against the deploy path it installs, and the rendered
// tree lives elsewhere.
func (r *renderer) absoluteChartPaths() {
	for _, item := range r.items {
		if item.Type != TypeHelm {
			continue
		}
		rel, err := ReadRelease(filepath.Join(r.opts.DeployPath, item.Name))
		if err != nil {
			r.problem("%s: %v", item.Name, err)
			continue
		}
		if rel.Chart == nil || rel.Chart.Path == "" {
			continue
		}
		path := rel.Chart.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(r.opts.DeployPath, path)
		}
		// The deploy path reader takes one unquoted token per value.
		if strings.ContainsFunc(path, unicode.IsSpace) {
			r.problem("%s: the chart path %q holds whitespace, which the deploy path reader cannot carry; render from a path without spaces", item.Name, path)
			continue
		}
		if path == rel.Chart.Path {
			continue
		}
		rel.Chart.Path = path
		if err := writePrivate(filepath.Join(r.opts.Out, item.Name, "release.yaml"), rel.Marshal()); err != nil {
			r.problem("%s: %v", item.Name, err)
		}
	}
}

func (r *renderer) hasItem(name, kind string) bool {
	for _, item := range r.items {
		if item.Name == name && item.Type == kind {
			return true
		}
	}
	return false
}

// appendValues writes a values file into the rendered item and appends it to
// the item's values list, after the committed ones so it wins.
func (r *renderer) appendValues(item, file string, data []byte) {
	// The rendered copy carries what earlier passes wrote, so the list is
	// appended there.
	dst := filepath.Join(r.opts.Out, item)
	rel, err := ReadRelease(dst)
	if err != nil {
		r.problem("%s: %v", item, err)
		return
	}
	if rel.Chart == nil {
		r.problem("%s: release.yaml has no chart section", item)
		return
	}
	if err := writePrivate(filepath.Join(dst, file), data); err != nil {
		r.problem("%s: %v", item, err)
		return
	}
	rel.Values = append(rel.Values, file)
	if err := writePrivate(filepath.Join(dst, "release.yaml"), rel.Marshal()); err != nil {
		r.problem("%s: %v", item, err)
	}
}

// appendSetFile adds <helm key>=<absolute path> entries to an item's
// chart.setFile.
func (r *renderer) appendSetFile(item string, entries []string) {
	dst := filepath.Join(r.opts.Out, item)
	rel, err := ReadRelease(dst)
	if err != nil {
		r.problem("%s: %v", item, err)
		return
	}
	if rel.Chart == nil {
		r.problem("%s: release.yaml has no chart section", item)
		return
	}
	rel.Chart.SetFile = append(rel.Chart.SetFile, entries...)
	if err := writePrivate(filepath.Join(dst, "release.yaml"), rel.Marshal()); err != nil {
		r.problem("%s: %v", item, err)
	}
}

func (r *renderer) finish() error {
	// The tree holds credentials; a repository must not pick it up by accident.
	if err := writePrivate(filepath.Join(r.opts.Out, ".gitignore"), []byte("*\n")); err != nil {
		return err
	}
	if r.itemsChanged {
		header := "Rendered by nvkectl render deploy-path from " + r.opts.DeployPath + ". Template items are rendered and apply as manifests."
		if err := writePrivate(filepath.Join(r.opts.Out, "nvke-deploy.yaml"), MarshalItems(r.items, header)); err != nil {
			return err
		}
	}
	return nil
}

// writePrivate writes a file the install reads and nobody else should: the
// rendered tree holds credentials.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	// Private, but a helper script stays executable for its owner.
	mode := os.FileMode(0o600)
	if info, err := in.Stat(); err == nil {
		mode |= info.Mode().Perm() & 0o100
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
