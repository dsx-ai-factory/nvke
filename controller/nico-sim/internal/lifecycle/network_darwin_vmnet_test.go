// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package lifecycle

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func mustSubnet(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, subnet, err := net.ParseCIDR(cidr)
	require.NoError(t, err)
	return subnet
}

func TestDarwinLease(t *testing.T) {
	subnet := mustSubnet(t, "192.168.64.0/24")

	data := "{\n ip_address=192.168.64.7\n hw_address=1,52:54:0:a:b:c\n lease=0x7fffffff\n}\n{\n ip_address=192.168.64.8\n hw_address=1,52:54:0:d:e:f\n lease=0x7fffffff\n}"
	ip, ok := darwinLease(data, "52:54:00:0a:0b:0c", subnet)
	require.True(t, ok)
	require.Equal(t, "192.168.64.7", ip)
	_, ok = darwinLease(data, "52:54:00:00:00:00", subnet)
	require.False(t, ok)
	_, ok = darwinLease("{\nip_address=invalid\nhw_address=1,52:54:0:a:b:c\n lease=0x7fffffff\n}", "52:54:00:0a:0b:0c", subnet)
	require.False(t, ok)
	_, ok = darwinLease("{\nip_address=192.168.64.3\nhw_address=1,52:54:0:a:b:c\nlease=1\n}", "52:54:00:0a:0b:0c", subnet)
	require.False(t, ok)
}

// TestDarwinLeaseFiltersOutOfSubnetBeforeComparingExpiry guards against
// picking the latest-expiry record for a MAC across the whole lease file
// before subnet-filtering: a newer lease that belongs to a different vmnet
// bridge subnet must not mask a valid, in-subnet lease that expires sooner.
func TestDarwinLeaseFiltersOutOfSubnetBeforeComparingExpiry(t *testing.T) {
	subnet := mustSubnet(t, "192.168.64.0/24")
	data := "{\n ip_address=192.168.64.9\n hw_address=1,52:54:0:a:b:c\n lease=0x7fffffff\n}\n" +
		"{\n ip_address=10.0.0.9\n hw_address=1,52:54:0:a:b:c\n lease=0x7fffffff0\n}"

	ip, ok := darwinLease(data, "52:54:00:0a:0b:0c", subnet)
	require.True(t, ok)
	require.Equal(t, "192.168.64.9", ip, "the out-of-subnet, later-expiring lease must not mask the in-subnet one")
}

func TestDarwinUserData(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			name: "rewrites phone_home to the vmnet gateway",
			in:   "phone_home:\n    url: http://169.254.169.254:7777/latest/meta-data/phone_home\n",
			want: "phone_home:\n    url: http://192.168.64.1:7777/latest/meta-data/phone_home\n",
		},
		{
			name: "leaves other link-local URLs alone",
			in:   "url: http://169.254.169.254/latest/\n",
			want: "url: http://169.254.169.254/latest/\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, darwinUserData(tt.in, "192.168.64.1"))
		})
	}
}
