// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"fmt"
	"net/http"
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

	// Client is used to check the URL exists. Nil means a client with a short
	// timeout.
	Client *http.Client
}

// Resolve returns the image's URL and the release tag it was built from.
//
// The point release is part of the file name and cannot be derived from the
// Ubuntu series, so it is the highest of InputReleasesKey that matches
// InputConstraintKey, which is the series. With InputOverrideKey the URL a person
// gave is returned in place of the built one, and the image's release is then
// not known.
func (o OSImage) Resolve(inputs map[string]string) (map[string]string, error) {
	url := URL{Client: o.Client}
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
	// New images publish under the nvke filename. Releases already in the bucket
	// keep the previous filename, and the bucket does not list, so the name that
	// answers is the one that was published. A release that has neither reports
	// the new name, which is what a new image is published as.
	got, err := url.Resolve(map[string]string{
		InputValueKey: o.imageObjectURL(under, tag, release, "nvke"),
	})
	if err != nil {
		legacy, legacyErr := url.Resolve(map[string]string{
			InputValueKey: o.imageObjectURL(under, tag, release, "nke"),
		})
		if legacyErr != nil {
			return nil, err
		}
		got = legacy
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
