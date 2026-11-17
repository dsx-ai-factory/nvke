// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"context"
	"fmt"
	"strings"

	"github.com/dsx-ai-factory/nvke/release/internal/artifact"
)

// URL returns a location, which is what the e2e suite takes for an image or a
// bootloader. It is the primitive the artifact strategies end in: a URL is
// checked to be one, checked to exist, and returned.
//
// It also resolves a URL that is declared and not derived, such as a bootloader
// whose path carries a date and a build number that nothing computes.
type URL struct {
	// Base is the location the value is a path under. Empty means the value is
	// a whole URL.
	Base string

	// CheckExists asks whether the URL holds something before returning it.
	//
	// It is off by default so that planning needs nothing but git. Resolution
	// answers what a run qualifies, and whether each artifact is published is
	// asked once, later, for every artifact in the plan.
	CheckExists bool
}

// Resolve returns the URL under ResultURLKey.
//
// With InputOverrideKey the URL a person gave is returned, and it is always a
// whole URL. Otherwise InputValueKey is the URL, or its path under Base when
// there is one. A whole URL given where a path is expected is an error, because
// it would be joined to Base and name something nobody wrote.
func (u URL) Resolve(inputs map[string]string) (map[string]string, error) {
	if given := inputs[InputOverrideKey]; given != "" {
		return u.finish(given)
	}

	value := inputs[InputValueKey]
	if value == "" {
		return nil, fmt.Errorf("no %s given, so there is no URL to return", InputValueKey)
	}
	if u.Base == "" {
		return u.finish(value)
	}
	if isURL(value) {
		return nil, fmt.Errorf("%q is a whole URL, but this entry takes a path under %s", value, u.Base)
	}
	return u.finish(strings.TrimSuffix(u.Base, "/") + "/" + strings.TrimPrefix(value, "/"))
}

// finish returns a URL, having checked it exists when asked to.
func (u URL) finish(url string) (map[string]string, error) {
	if !isURL(url) {
		return nil, fmt.Errorf("%q is not a URL", url)
	}
	if u.CheckExists {
		if err := artifact.Exists(context.Background(), url); err != nil {
			return nil, err
		}
	}
	return map[string]string{ResultURLKey: url}, nil
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

// Validate requires a value and takes nothing else. A value is a path under the
// base when there is one and a whole URL when there is not, and a value of the
// other kind could never resolve.
func (u URL) Validate(inputs map[string]string) error {
	if err := takesOnly(inputs, InputValueKey); err != nil {
		return err
	}
	if err := needs(inputs, InputValueKey); err != nil {
		return err
	}
	value := inputs[InputValueKey]
	switch {
	case u.Base != "" && isURL(value):
		return fmt.Errorf("value %q is a whole URL, but this entry takes a path under %s", value, u.Base)
	case u.Base == "" && !isURL(value):
		return fmt.Errorf("value %q is not a URL", value)
	}
	return nil
}
