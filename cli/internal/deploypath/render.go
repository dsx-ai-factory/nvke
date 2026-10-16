// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package deploypath

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/nvkrrelease"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/deploypath/stackconfig"
)

// RenderOptions drive Render.
type RenderOptions struct {
	// DeployPath is the committed deploy path; it is never written.
	DeployPath string
	// Site is the site spec of the site the stack is installed for: the
	// cluster name, the control-plane VIP, the SPIRE trust domain and CA.
	// Optional; Stack goes with it.
	Site *sitespec.SiteSpec
	// Stack is the stack config, loaded by stackconfig.Load: the NICo API
	// and the optional image overrides and mirror. Required with Site,
	// refused without it. The credentials come from the environment.
	Stack *stackconfig.Config
	// Release is the NVKR release description nvkr release export writes,
	// and ReleasePath where it was read from, copied into the output as
	// nvkr-release.json. Optional.
	Release     *nvkrrelease.Release
	ReleasePath string
	// Arch is the nodes' architecture, linux/amd64 or linux/arm64. It
	// selects the release artifacts and the SPIRE TPM attestor checksum, and
	// is required with Site.
	Arch string
	// RunDir, when set, is a completed NVKR Run (or a directory of OCI
	// layouts): charts and images are read from it and nothing is pulled.
	RunDir string
	// Out is the directory to write; it must not exist and must not be
	// under a shared temp directory.
	Out string
	// AllowTempOut accepts an Out under a temp directory. Tests only: their
	// scratch directories live there.
	AllowTempOut bool
	// Registry opens a repository named by the release when RunDir is
	// empty; Registries is the default.
	Registry func(repository string) (oras.ReadOnlyTarget, error)
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
	// Images lists the image references the rendered deploy path runs from
	// the release, with the layout each came from when RunDir was set.
	Images []ImageSource
	// Notes are the decisions worth telling the operator.
	Notes []string
	// Credentials lists the files under Out that carry a site credential,
	// relative to Out, in the order they were written.
	Credentials []string
}

// ImageSource is one image the rendered deploy path runs.
type ImageSource struct {
	Reference string
	Layout    string
}

// Render writes a copy of the deploy path under opts.Out; ctx cancels its
// registry requests. It writes the copy, resolved for its
// inputs. For the site: every item that takes site values gets a
// site-values.yaml appended to its values, and every template item is
// rendered against the site and becomes a manifest item. For the release: a
// helm item's chart comes from the chart archive pulled by digest into
// charts/<item>/ when the release carries the artifact (chart.artifact,
// default the item name), else stays as committed; an item bound to image
// artifacts gets a release-images.yaml values file; a manifest item bound
// to an image artifact gets a kustomization.yaml that rewrites its image to
// the artifact's repository and digest; images.txt lists the images the
// result runs. The committed files are copied, never edited. The result
// holds the site's secrets, so it is written 0700 and is meant for one
// install. Every problem is reported at once and nothing is left under Out
// on failure.
func Render(ctx context.Context, opts RenderOptions) (*Rendered, error) {
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if opts.Site != nil && opts.Stack == nil {
		return nil, errors.New("--sitespec needs --stack-config: the stack's input is the stack config file")
	}
	if opts.Stack != nil && opts.Site == nil {
		return nil, errors.New("--stack-config needs --sitespec: the site name, VIP and trust domain come from the site spec")
	}
	if opts.Site == nil && opts.Release == nil {
		return nil, errors.New("a site spec or a release description is required")
	}
	if opts.Stack != nil && opts.Stack.Path() == "" {
		return nil, errors.New("the stack config must be loaded by stackconfig.Load, which validates it")
	}
	if opts.Site != nil && !contains(nvkrrelease.Archs, opts.Arch) {
		return nil, fmt.Errorf("the architecture %q is not one of %s", opts.Arch, strings.Join(nvkrrelease.Archs, ", "))
	}
	if opts.Release != nil && opts.RunDir == "" && opts.Registry == nil {
		opts.Registry = Registries(nil)
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
	if opts.RunDir != "" {
		// images.txt names each image's layout; the install may run from
		// another directory, so the paths are absolute.
		runDir, err := filepath.Abs(opts.RunDir)
		if err != nil {
			return nil, err
		}
		opts.RunDir = runDir
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
	r := &renderer{ctx: ctx, opts: opts, items: items, out: &Rendered{}, failed: map[string]bool{}}
	// Committed chart paths first, so a chart the release carries replaces
	// them and a chart it lacks resolves from anywhere; then the release, then
	// the site, whose values are appended last and win.
	r.absoluteChartPaths()
	if opts.Release != nil {
		for _, item := range items {
			switch item.Type {
			case TypeHelm:
				r.helm(item.Name)
			case TypeManifest:
				r.manifest(item.Name)
			}
		}
	}
	if opts.Site != nil {
		r.site()
	}
	if len(r.problems) > 0 {
		_ = os.RemoveAll(opts.Out)
		return nil, fmt.Errorf("nothing was rendered:\n  %s", strings.Join(r.problems, "\n  "))
	}
	if err := r.finish(); err != nil {
		_ = os.RemoveAll(opts.Out)
		return nil, err
	}
	for _, line := range r.out.Notes {
		fmt.Fprintln(opts.Log, line)
	}
	return r.out, nil
}

type renderer struct {
	// failed names the items whose release.yaml did not parse; later passes
	// skip them so the problem is reported once.
	failed map[string]bool
	// ctx bounds the registry requests; a cancelled command unwinds through
	// the normal cleanup.
	ctx      context.Context
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

// note records a decision for the operator. Notes reach Log only when the
// render succeeds, so a later failure does not leave a note about a tree
// that was removed.
func (r *renderer) note(format string, args ...any) {
	r.out.Notes = append(r.out.Notes, fmt.Sprintf(format, args...))
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
			r.failed[item.Name] = true
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
	if r.failed[item] {
		return
	}
	// The rendered copy carries what earlier passes wrote (the absolute or
	// archive chart path, release-images.yaml), so the list is appended there.
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
	if r.failed[item] {
		return
	}
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

// selectRequired resolves an artifact an item needs; absence is a problem.
func (r *renderer) selectRequired(item, role, name string) (nvkrrelease.Artifact, bool) {
	a, err := r.opts.Release.Select(name, r.opts.Arch)
	if errors.Is(err, nvkrrelease.ErrNotPublished) {
		r.problem("%s: %s %s is not in the release", item, role, name)
		return nvkrrelease.Artifact{}, false
	}
	if err != nil {
		r.problem("%s: %s: %v", item, role, err)
		return nvkrrelease.Artifact{}, false
	}
	return a, true
}

func (r *renderer) helm(item string) {
	if r.failed[item] {
		return
	}
	// The rendered copy, which already carries an absolute committed chart path.
	rel, err := ReadRelease(filepath.Join(r.opts.Out, item))
	if err != nil {
		r.problem("%s: %v", item, err)
		return
	}
	if rel.Chart == nil {
		r.problem("%s: release.yaml has no chart section", item)
		return
	}
	dst := filepath.Join(r.opts.Out, item)
	changed := false

	artifact := rel.Chart.Artifact
	if artifact == "" {
		artifact = item
	}
	a, err := r.opts.Release.Select(artifact, r.opts.Arch)
	switch {
	case errors.Is(err, nvkrrelease.ErrNotPublished):
		r.note("%s: %s is not in the release; the chart comes from the deploy path or its repository", item, artifact)
	case err != nil:
		r.problem("%s: chart: %v", item, err)
	default:
		// The release decides the version of every chart it carries; the pin
		// in release.yaml decides only for a repository chart the release
		// lacks, and a path chart names none. A difference is said at once,
		// not noted, because it points at values written for another chart.
		if a.ChartVersion != "" && rel.Chart.Version != "" && a.ChartVersion != rel.Chart.Version {
			fmt.Fprintf(r.opts.Log, "warning: %s: the release carries %s %s, release.yaml pins %s; the release decides, check the item's values against %s\n",
				item, artifact, a.ChartVersion, rel.Chart.Version, a.ChartVersion)
		}
		file, err := r.chartArchive(item, rel.Chart.Name, a)
		if err != nil {
			r.problem("%s: chart %s: %v", item, a.Reference, err)
		} else {
			rel.Chart.Path = file
			rel.Chart.Repository = ""
			if a.ChartVersion != "" {
				rel.Chart.Version = a.ChartVersion
			}
			changed = true
			r.note("%s: chart %s from %s", item, firstNonEmpty(a.ChartVersion, rel.Chart.Version, "(unversioned)"), a.Reference)
		}
	}

	values := map[string]string{}
	if rel.Chart.ImageArtifact != "" {
		if img, ok := r.selectRequired(item, "image", rel.Chart.ImageArtifact); ok {
			values["image.repository"] = img.Repository()
			values["image.digest"] = img.Digest
			r.image(img)
		}
	}
	if rel.Chart.ProfilesArtifact != "" {
		if img, ok := r.selectRequired(item, "profiles image", rel.Chart.ProfilesArtifact); ok {
			values["profiles.image.repository"] = img.Repository()
			values["profiles.image.digest"] = img.Digest
			r.image(img)
		}
	}
	if len(values) > 0 {
		if err := writePrivate(filepath.Join(dst, "release-images.yaml"), releaseImageValues(values)); err != nil {
			r.problem("%s: %v", item, err)
			return
		}
		rel.Values = append(rel.Values, "release-images.yaml")
		changed = true
	}
	if changed {
		if err := writePrivate(filepath.Join(dst, "release.yaml"), rel.Marshal()); err != nil {
			r.problem("%s: %v", item, err)
		}
	}
}

func (r *renderer) manifest(item string) {
	src := filepath.Join(r.opts.DeployPath, item)
	if _, err := os.Stat(filepath.Join(src, "release.yaml")); err != nil {
		return
	}
	rel, err := ReadRelease(src)
	if err != nil {
		r.problem("%s: %v", item, err)
		return
	}
	if rel.Artifact == "" {
		return
	}
	if rel.Image == "" {
		r.problem("%s: release.yaml binds artifact %s but does not say which image the manifest runs (image: <repository>:<tag>)", item, rel.Artifact)
		return
	}
	img, ok := r.selectRequired(item, "image", rel.Artifact)
	if !ok {
		return
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		r.problem("%s: %v", item, err)
		return
	}
	var resources []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".yaml") || name == "release.yaml" || name == "kustomization.yaml" {
			continue
		}
		resources = append(resources, name)
	}
	sort.Strings(resources)
	var b strings.Builder
	b.WriteString("resources:\n")
	for _, res := range resources {
		fmt.Fprintf(&b, "  - %s\n", res)
	}
	fmt.Fprintf(&b, "images:\n  - name: %q\n    newName: %q\n    digest: %q\n", rel.Image[:strings.LastIndex(rel.Image, ":")], img.Repository(), img.Digest)
	if err := writePrivate(filepath.Join(r.opts.Out, item, "kustomization.yaml"), []byte(b.String())); err != nil {
		r.problem("%s: %v", item, err)
		return
	}
	r.image(img)
	r.note("%s: image %s -> %s", item, rel.Image, img.Reference)
}

// image records an image the rendered tree runs, as repository@digest: the
// name the values and the kustomization use, and the one containerd is asked
// for. A tag in the release's reference is dropped.
func (r *renderer) image(a nvkrrelease.Artifact) {
	ref := a.Repository() + "@" + a.Digest
	for _, have := range r.out.Images {
		if have.Reference == ref {
			return
		}
	}
	src := ImageSource{Reference: ref}
	if r.opts.RunDir != "" {
		layout, err := nvkrrelease.FindLayout(r.opts.RunDir, a.Digest)
		if err != nil {
			r.problem("image %s: %v", a.Reference, err)
			return
		}
		src.Layout = layout
	}
	r.out.Images = append(r.out.Images, src)
}

// chartArchive puts the chart archive of a into charts/<item>/ under Out and
// returns its path relative to Out. The store verifies the manifest and the
// archive against their digests as it reads them.
func (r *renderer) chartArchive(item, chartName string, a nvkrrelease.Artifact) (string, error) {
	ctx := r.ctx
	store, ref, err := r.store(ctx, a)
	if err != nil {
		return "", err
	}
	desc, err := store.Resolve(ctx, ref)
	if err != nil {
		return "", err
	}
	if got := desc.Digest.String(); got != a.Digest {
		return "", fmt.Errorf("the manifest is %s, not the reference's %s", got, a.Digest)
	}
	manifest, err := content.FetchAll(ctx, store, desc)
	if err != nil {
		return "", err
	}
	layer, file, err := nvkrrelease.ChartLayer(manifest, chartName+"-"+strings.TrimPrefix(a.Digest, "sha256:")+".tgz")
	if err != nil {
		return "", err
	}
	data, err := content.FetchAll(ctx, store, layer)
	if err != nil {
		return "", fmt.Errorf("chart layer %s: %w", layer.Digest, err)
	}
	relDir := filepath.Join("charts", item)
	if err := writePrivate(filepath.Join(r.opts.Out, relDir, file), data); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join(relDir, file)), nil
}

// store returns where a is read from, the OCI layout in RunDir that holds it
// or its registry, and the reference to resolve there.
func (r *renderer) store(ctx context.Context, a nvkrrelease.Artifact) (oras.ReadOnlyTarget, string, error) {
	if r.opts.RunDir == "" {
		store, err := r.opts.Registry(a.Repository())
		return store, a.Reference, err
	}
	layout, err := nvkrrelease.FindLayout(r.opts.RunDir, a.Digest)
	if err != nil {
		return nil, "", err
	}
	store, err := oci.NewFromFS(ctx, os.DirFS(layout))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", layout, err)
	}
	return store, a.Digest, nil
}

func (r *renderer) finish() error {
	// The tree holds credentials; a repository must not pick it up by accident.
	if err := writePrivate(filepath.Join(r.opts.Out, ".gitignore"), []byte("*\n")); err != nil {
		return err
	}
	if len(r.out.Images) > 0 {
		var b strings.Builder
		for _, img := range r.out.Images {
			fmt.Fprintf(&b, "%s\t%s\n", img.Reference, img.Layout)
		}
		if err := writePrivate(filepath.Join(r.opts.Out, "images.txt"), []byte(b.String())); err != nil {
			return err
		}
	}
	if r.opts.ReleasePath != "" {
		if err := copyFile(r.opts.ReleasePath, filepath.Join(r.opts.Out, "nvkr-release.json")); err != nil {
			return err
		}
	}
	if r.itemsChanged {
		header := "Rendered by nvkectl render deploy-path from " + r.opts.DeployPath + ". Template items are rendered and apply as manifests."
		if err := writePrivate(filepath.Join(r.opts.Out, "nvke-deploy.yaml"), MarshalItems(r.items, header)); err != nil {
			return err
		}
	}
	return nil
}

// releaseImageValues renders the generated values file: nested keys from dotted ones.
func releaseImageValues(values map[string]string) []byte {
	var keys []string
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Written by nvkectl render deploy-path from the release description.\n")
	var open []string
	for _, k := range keys {
		parts := strings.Split(k, ".")
		common := 0
		for common < len(open) && common < len(parts)-1 && open[common] == parts[common] {
			common++
		}
		open = open[:common]
		for _, p := range parts[common : len(parts)-1] {
			fmt.Fprintf(&b, "%s%s:\n", strings.Repeat("  ", len(open)), p)
			open = append(open, p)
		}
		fmt.Fprintf(&b, "%s%s: %q\n", strings.Repeat("  ", len(open)), parts[len(parts)-1], values[k])
	}
	return []byte(b.String())
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

// archOf returns the CPU architecture of a platform: linux/arm64 is arm64.
func archOf(platform string) string {
	return strings.TrimPrefix(platform, "linux/")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
