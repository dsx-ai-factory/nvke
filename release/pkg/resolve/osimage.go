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
// not list, so building is the only way to name it.
type OSImage struct {
	// Base is the location the images are published under.
	Base string

	// Path is the directory under Base the release directories sit in. It may be
	// empty.
	Path string

	// Arch is the architecture of the image.
	Arch string

	// Variant is the image flavour in the file name, such as non-fde.
	Variant string

	// CheckExists is passed to the URL this builds, so an image is held to the
	// same opt-in check as a declared one.
	CheckExists bool
}

// Resolve returns the image's URL.
//
// The point release is part of the file name and cannot be derived from the
// Ubuntu series, so it is the highest of InputReleasesKey that matches
// InputConstraintKey, which is the series. With InputOverrideKey the URL a person
// gave is returned in place of the built one.
func (o OSImage) Resolve(inputs map[string]string) (map[string]string, error) {
	url := URL{CheckExists: o.CheckExists}
	if inputs[InputOverrideKey] != "" {
		return url.Resolve(inputs)
	}

	tag := inputs[InputVersionKey]
	if tag == "" {
		return nil, fmt.Errorf("no %s given, so there is no release to build an image URL for", InputVersionKey)
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
	return url.Resolve(map[string]string{InputValueKey: o.imageObjectURL(under, tag, release)})
}

// imageObjectURL is the qcow2 published for one release, architecture and point
// release.
func (o OSImage) imageObjectURL(under, tag, release string) string {
	return fmt.Sprintf("%s/os-image/releases/%s/%s/nvke-node-%s-ubuntu-%s-%s.qcow2",
		under, tag, o.Arch, o.Variant, release, o.Arch)
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
