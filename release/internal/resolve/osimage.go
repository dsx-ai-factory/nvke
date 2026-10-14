// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"fmt"
	"strings"
)

// OSImage names the node image for one architecture.
//
// An image is published once per release, architecture and Ubuntu point
// release, so its URL is built from those and never browsed for. The bucket does
// not list, so building is the only way to name it. The built URL is handed to
// URL, which checks it exists and returns it.
type OSImage struct {
	// Base is the location the images are published under.
	Base string

	// Path is the directory under Base the release directories sit in. It may be
	// empty.
	Path string

	// From names the component whose version is the release tag.
	From string

	// Arch is the architecture of the image.
	Arch string

	// Variant is the image flavour in the file name, such as non-fde.
	Variant string

	// CheckExists is passed to the URL this builds, so an image is held to the
	// same opt-in check as a declared one.
	CheckExists bool
}

// Resolve returns the image's URL and the release tag it was built from.
//
// The point release is part of the file name and cannot be derived from the
// Ubuntu series, so it is the highest of InputReleasesKey that matches
// InputConstraintKey, which is the series. With InputOverrideKey the URL a person
// gave is returned in place of the built one, and the image's release is then
// not known.
func (o OSImage) Resolve(inputs map[string]string) (map[string]string, error) {
	url := URL{CheckExists: o.CheckExists}
	if inputs[InputOverrideKey] != "" {
		return url.Resolve(inputs)
	}

	tag := inputs[o.From]
	if tag == "" {
		return nil, fmt.Errorf("no %s given, so there is no release to build an image URL for", o.From)
	}
	series := inputs[InputConstraintKey]
	if series == "" {
		return nil, fmt.Errorf("no %s given, which is the Ubuntu series of the %s image", InputConstraintKey, tag)
	}
	releases := strings.Split(inputs[InputReleasesKey], ",")
	if inputs[InputReleasesKey] == "" {
		return nil, fmt.Errorf("no %s given, so there is no Ubuntu point release to name the %s image with", InputReleasesKey, tag)
	}
	release, ok := HighestMatching(releases, series)
	if !ok {
		return nil, fmt.Errorf("none of the %s %s matches Ubuntu %s", InputReleasesKey, strings.Join(releases, ", "), series)
	}

	under := strings.TrimSuffix(o.Base, "/")
	if o.Path != "" {
		under += "/" + strings.Trim(o.Path, "/")
	}
	// Every image published since the rename carries the nvke filename. A
	// release cut before it is still in the bucket under the old one, and the
	// bucket does not list, so the name that answers is the one that was
	// published. This asks with its own check rather than the caller's, because
	// here the answer selects the filename rather than confirming it.
	//
	// Transitional, and it goes when the first release published since the
	// rename is the highest one a run resolves. A run describing a commit
	// already skips it: anything built now carries the new name, and its image
	// may still be baking, so asking would fail a plan for a reason the wait
	// exists to absorb.
	name := o.imageObjectURL(under, tag, release, "nvke")
	if inputs[InputCommitKey] == "" {
		probe := URL{CheckExists: true}
		if _, err := probe.Resolve(map[string]string{InputValueKey: name}); err != nil {
			legacy := o.imageObjectURL(under, tag, release, "nke")
			if _, err := probe.Resolve(map[string]string{InputValueKey: legacy}); err == nil {
				name = legacy
			}
		}
	}

	got, err := url.Resolve(map[string]string{InputValueKey: name})
	if err != nil {
		return nil, err
	}
	got[ResultVersionKey] = tag
	return got, nil
}

// imageObjectURL is the qcow2 published for one release, architecture and point
// release. prefix is nvke for an image built after the rename and nke for one
// published before it.
func (o OSImage) imageObjectURL(under, tag, release, prefix string) string {
	return fmt.Sprintf("%s/os-image/releases/%s/%s/%s-node-%s-ubuntu-%s-%s.qcow2",
		under, tag, o.Arch, prefix, o.Variant, release, o.Arch)
}

// Validate requires the series and the releases, and that one of the releases
// matches the series.
func (OSImage) Validate(inputs map[string]string) error {
	if err := takesOnly(inputs, InputConstraintKey, InputReleasesKey); err != nil {
		return err
	}
	if err := needs(inputs, InputConstraintKey, InputReleasesKey); err != nil {
		return err
	}
	releases := strings.Split(inputs[InputReleasesKey], ",")
	if _, ok := HighestMatching(releases, inputs[InputConstraintKey]); !ok {
		return fmt.Errorf("none of the releases %s matches Ubuntu %s", strings.Join(releases, ", "), inputs[InputConstraintKey])
	}
	return nil
}
