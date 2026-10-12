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

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
)

// RenderOptions drive Render.
type RenderOptions struct {
	// DeployPath is the committed deploy path; it is never written.
	DeployPath string
	// Site is the site spec whose controllerStack section describes the
	// site the stack is installed for.
	Site *sitespec.SiteSpec
	// Out is the directory to write; it must not exist.
	Out string
	// Log receives one line per decision.
	Log io.Writer
}

// Rendered reports what Render wrote.
type Rendered struct {
	// Notes are the decisions worth telling the operator.
	Notes []string
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
	if _, err := os.Stat(opts.Out); err == nil {
		return nil, fmt.Errorf("%s exists; render writes a new directory", opts.Out)
	}
	items, err := Items(opts.DeployPath)
	if err != nil {
		return nil, err
	}
	if err := copyTree(opts.DeployPath, opts.Out); err != nil {
		_ = os.RemoveAll(opts.Out)
		return nil, err
	}
	if err := os.Chmod(opts.Out, 0o700); err != nil {
		_ = os.RemoveAll(opts.Out)
		return nil, err
	}
	r := &renderer{opts: opts, items: items, out: &Rendered{}}
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

func (r *renderer) note(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.out.Notes = append(r.out.Notes, line)
	fmt.Fprintln(r.opts.Log, line)
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
	dst := filepath.Join(r.opts.Out, item)
	rel, err := ReadRelease(filepath.Join(r.opts.DeployPath, item))
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

func (r *renderer) finish() error {
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
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
