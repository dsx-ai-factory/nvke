// SPDX-License-Identifier: Apache-2.0

package projection

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSON(t *testing.T) {
	type public struct {
		Name string `json:"name"`
	}
	for _, tt := range []struct {
		name string
		from any
		want public
		err  bool
	}{
		{
			name: "copies matching fields", from: map[string]any{"name": "cluster", "private": true},
			want: public{Name: "cluster"},
		},
		{name: "reports marshal failure", from: make(chan int), err: true},
		{name: "reports unmarshal failure", from: map[string]any{"name": 42}, err: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := JSON[public](tt.from)
			if tt.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
