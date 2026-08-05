// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary

package lifecycle

import (
	"os"
	"strings"
)

// readLeaseByMAC reads a dnsmasq lease file and returns the IP currently
// assigned to the given MAC address. Returns ("", false) if no lease is found
// or the file cannot be read.
//
// dnsmasq lease file format (one entry per line):
//
//	epoch mac ip hostname [client-id]
func readLeaseByMAC(leaseFile, mac string) (string, bool) {
	data, err := os.ReadFile(leaseFile)
	if err != nil {
		return "", false
	}
	mac = strings.ToLower(mac)
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && strings.ToLower(fields[1]) == mac {
			return fields[2], true
		}
	}
	return "", false
}
