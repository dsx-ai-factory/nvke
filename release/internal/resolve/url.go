// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"fmt"
	"net/http"
	"strings"
	"time"
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

	// Client is used to check the URL exists. Nil means a client with a short
	// timeout.
	Client *http.Client
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

// finish checks a URL exists and returns it.
func (u URL) finish(url string) (map[string]string, error) {
	if !isURL(url) {
		return nil, fmt.Errorf("%q is not a URL", url)
	}
	if err := u.exists(url); err != nil {
		return nil, err
	}
	return map[string]string{ResultURLKey: url}, nil
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

// exists asks for the URL with HEAD and requires a 200.
//
// The bucket is private and does not list, so a missing object answers 403 as
// well, and so does a request from a network that is not allowed. The check can
// confirm that something exists and cannot say that it does not, so a failure
// names both explanations.
func (u URL) exists(url string) error {
	client := u.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	resp, err := client.Head(url)
	if err != nil {
		return fmt.Errorf("could not confirm %s exists: %w", url, err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("could not confirm %s exists: HTTP %d. A missing artifact and a network that is "+
			"not allowed both answer 403, so check both", url, resp.StatusCode)
	}
	return nil
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
