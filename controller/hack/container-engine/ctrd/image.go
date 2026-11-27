// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
)

// normalizeRef gives an image reference its full name, as docker does and ctr
// does not: ubuntu is docker.io/library/ubuntu:latest.
func normalizeRef(ref string) string {
	repoTag, digest, _ := strings.Cut(ref, "@")
	if digest != "" {
		digest = "@" + digest
	}
	repo, tag := repoTag, ""
	if i := strings.LastIndex(repoTag, ":"); i > strings.LastIndex(repoTag, "/") {
		repo, tag = repoTag[:i], repoTag[i:]
	}
	domain, path, _ := strings.Cut(repo, "/")
	if !strings.Contains(repo, "/") || (!strings.ContainsAny(domain, ".:") && domain != "localhost") {
		domain, path = dockerHub, repo
	}
	if domain == "index.docker.io" {
		domain = dockerHub
	}
	if domain == dockerHub && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	if tag == "" && digest == "" {
		tag = ":latest"
	}
	return domain + "/" + path + tag + digest
}

func hostPlatform() string { return "linux/" + runtime.GOARCH }

// findImage returns the image stored under the reference.
func (e *engine) findImage(ref string) (*ctrImage, error) {
	images, err := e.ctrImages()
	if err != nil {
		return nil, err
	}
	for _, image := range images {
		if image.Ref == ref {
			return &image, nil
		}
	}
	return nil, fmt.Errorf("no such image: %s", ref)
}

// ensureImage pulls the image unless it is stored already, as docker run does.
func (e *engine) ensureImage(ref string) error {
	if _, err := e.findImage(ref); err == nil {
		return nil
	}
	return e.pullImage(ref, hostPlatform())
}

func (e *engine) pullImage(ref, platform string) error {
	return e.ctrPull(ref, platform)
}

func (e *engine) pull(args []string) error {
	flags, err := parseFlags("pull", flagSet{flagPlatform: {Kind: stringFlag}, flagQuiet: {Kind: boolFlag, Short: "q"}}, args, false)
	if err != nil {
		return err
	}
	if len(flags.args) != 1 {
		return errors.New("pull: one image is required")
	}
	platform := flags.str(flagPlatform)
	if platform == "" {
		platform = hostPlatform()
	}
	return e.pullImage(normalizeRef(flags.args[0]), platform)
}

func (e *engine) imageDocument(ref string) (map[string]any, error) {
	name := normalizeRef(ref)
	image, err := e.findImage(name)
	if err != nil {
		return nil, err
	}
	return map[string]any{"Id": image.Digest, "RepoTags": []string{name}, "Architecture": runtime.GOARCH, "Os": runtime.GOOS}, nil
}

// save writes the images to an archive that a node's `ctr images import
// --all-platforms` reads. An image stored as a multi-platform index is written
// as the manifest of the platform alone: the import would look for the content
// of every platform the index lists.
func (e *engine) save(args []string) error {
	flags, err := parseFlags("save", flagSet{"output": {Kind: stringFlag, Short: "o"}, flagPlatform: {Kind: stringFlag}}, args, false)
	if err != nil {
		return err
	}
	output := flags.str("output")
	if output == "" || len(flags.args) == 0 {
		return errors.New("save: -o FILE and at least one image are required")
	}
	platform := flags.str(flagPlatform)
	if platform == "" {
		platform = hostPlatform()
	}
	refs := make([]string, len(flags.args))
	for i, ref := range flags.args {
		refs[i] = normalizeRef(ref)
		if _, err := e.findImage(refs[i]); err != nil {
			return err
		}
	}
	exported := output + ".export"
	defer os.Remove(exported)
	if err := e.ctrExport(exported, platform, refs); err != nil {
		return err
	}
	return flattenArchive(exported, output, platform)
}

// load imports an archive of images, as `save` or docker wrote it.
func (e *engine) load(args []string) error {
	flags, err := parseFlags("load", flagSet{"input": {Kind: stringFlag, Short: "i"}, flagQuiet: {Kind: boolFlag, Short: "q"}}, args, false)
	if err != nil {
		return err
	}
	if flags.str("input") == "" {
		return notSupportedError("load from standard input")
	}
	return e.ctrImport(flags.str("input"), hostPlatform())
}
