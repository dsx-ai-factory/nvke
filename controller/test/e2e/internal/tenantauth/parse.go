// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantauth

import (
	"bufio"
	"strings"
)

// parseOutput turns the verifier pod's termination message into a VerifyResult.
// Lines:
//   - "WHOAMI <username?>"           -> Username (may be empty)
//   - "CANI <verb> <res> <nsout> <yes|no>" -> CanI[Check{verb,res,ns}] = yes
//   - "LISTNODES <ok|fail>"                 -> ListNodesOK
//
// where ns is "" if nsout=="-", "*" if nsout=="*", else nsout. The parser is
// total: short or unexpected lines are ignored, never panicking.
func parseOutput(output string) VerifyResult {
	result := VerifyResult{CanI: map[Check]bool{}}

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "WHOAMI":
			if len(fields) >= 2 {
				result.Username = fields[1]
			}
		case "CANI":
			if len(fields) >= 5 {
				check := Check{Verb: fields[1], Resource: fields[2], Namespace: decodeNsout(fields[3])}
				result.CanI[check] = fields[4] == "yes"
			}
		case "LISTNODES":
			if len(fields) >= 2 {
				result.ListNodesOK = fields[1] == "ok"
			}
		}
	}

	return result
}

func decodeNsout(nsout string) string {
	switch nsout {
	case "-":
		return ""
	default:
		return nsout
	}
}
