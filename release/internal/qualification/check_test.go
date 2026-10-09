// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		subject string
		want    string
	}{
		{name: "a consistent set passes", subject: testSubject},
		{
			name:    "the subject is not registered",
			subject: strings.NewReplacer("  name: nke", "  name: agent", "subject: nke", "subject: agent").Replace(testSubject),
			want:    `subject "agent" is not a component in the registry`,
		},
		{
			name:    "a component is not registered",
			subject: strings.Replace(testSubject, "{name: cli, selectors", "{name: agent, selectors", 1),
			want:    `lists "agent", which is not a component in the registry`,
		},
		{
			name:    "a dependency is not registered",
			subject: strings.Replace(testSubject, "{name: kind}", "{name: helm}", 1),
			want:    `depends on "helm", which is not a component in the registry`,
		},
		{
			name:    "an unknown selector",
			subject: strings.Replace(testSubject, "selectors: [latest]", "selectors: [tip]", 1),
			want:    "the only selector is",
		},
		{
			name:    "a capability a stream lacks",
			subject: strings.Replace(testSubject, "{capability: kube}", "{capability: ubuntu}", 1),
			want:    `stream amd64-1.34 does not declare capability "ubuntu"`,
		},
		{
			name:    "a stream the policy does not define",
			subject: testSubject + "  streams: [amd64-9.99]\n",
			want:    "which the policy does not define",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := LoadSubject(write(t, "qualification.yaml", tc.subject))
			if err != nil {
				t.Fatal(err)
			}
			err = Check(testPolicy(), testRegistry(), s, nil)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantError(t, err, tc.want)
		})
	}
}
