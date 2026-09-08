// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbwrapper

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyCoalescer(t *testing.T) {
	// Wide enough that a loaded runner does not turn scheduling delay into a
	// failure. The test waits on conditions, not on the clock.
	const window = 200 * time.Millisecond
	var sent atomic.Int32
	c := newKeyCoalescer[string](window, func(string) { sent.Add(1) })

	// A storm inside one window: one send now, one trailing send, nothing more.
	for range 100 {
		c.offer("node")
	}
	assert.Equal(t, int32(1), sent.Load(), "the first offer is sent at once")
	require.Eventually(t, func() bool { return sent.Load() == 2 }, 10*window, window/10, "one trailing send for the folded offers")
	assert.Never(t, func() bool { return sent.Load() > 2 }, 3*window, window/10, "a quiet window ends the coalescing")

	// Different keys do not fold into each other.
	c.offer("a")
	c.offer("b")
	assert.Equal(t, int32(4), sent.Load())

	// After the window a fresh offer is immediate again.
	time.Sleep(3 * window)
	c.offer("node")
	assert.Equal(t, int32(5), sent.Load())
}
