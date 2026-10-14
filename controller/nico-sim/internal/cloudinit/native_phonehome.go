// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudinit

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
)

const legacyMetadataPhoneHomeURL = "http://169.254.169.254:7777/latest/meta-data/phone_home"

// Native metadata binds the admitted vmnet gateway. Relocate the source
// profile's exact link-local callback to that same existing metadata handler;
// retain the post fields, retry policy and independently authored callback URLs.
func projectNativePhoneHome(cfg map[string]interface{}, gateway string) error {
	current, exists := cfg["phone_home"]
	if !exists {
		return nil
	}
	phoneHome, ok := current.(map[string]interface{})
	if !ok {
		return fmt.Errorf("native phone_home must be a mapping")
	}
	currentURL, exists := phoneHome["url"]
	if !exists {
		return nil // Cloud-init owns callbacks without a declared URL.
	}
	callback, ok := currentURL.(string)
	if !ok {
		return fmt.Errorf("native phone_home URL must be a string")
	}
	if callback != legacyMetadataPhoneHomeURL {
		return nil
	}
	address, err := netip.ParseAddr(gateway)
	if err != nil || !address.Is4() || !address.IsPrivate() {
		return fmt.Errorf("native metadata callback requires its declared private IPv4 gateway")
	}
	endpoint := url.URL{Scheme: "http", Host: net.JoinHostPort(address.String(), "7777"), Path: "/latest/meta-data/phone_home"}
	phoneHome["url"] = endpoint.String()
	return nil
}
