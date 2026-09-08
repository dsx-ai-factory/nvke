// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbwrapper

import (
	"sync"
	"time"
)

// keyCoalescer delivers a key at most once per window. The first offer is sent
// at once; offers that land inside the window fold into a single trailing send,
// which runs after the last of them so the reconcile reads the newest state.
type keyCoalescer[T comparable] struct {
	mu      sync.Mutex
	window  time.Duration
	pending map[T]*coalesceState
	send    func(T)
}

type coalesceState struct {
	dirty bool
	timer *time.Timer
}

func newKeyCoalescer[T comparable](window time.Duration, send func(T)) *keyCoalescer[T] {
	return &keyCoalescer[T]{window: window, pending: map[T]*coalesceState{}, send: send}
}

func (c *keyCoalescer[T]) offer(key T) {
	c.mu.Lock()
	if st, ok := c.pending[key]; ok {
		st.dirty = true
		c.mu.Unlock()
		return
	}
	st := &coalesceState{}
	c.pending[key] = st
	st.timer = time.AfterFunc(c.window, func() { c.fire(key) })
	c.mu.Unlock()
	c.send(key)
}

func (c *keyCoalescer[T]) fire(key T) {
	c.mu.Lock()
	st, ok := c.pending[key]
	if !ok {
		c.mu.Unlock()
		return
	}
	if !st.dirty {
		delete(c.pending, key)
		c.mu.Unlock()
		return
	}
	st.dirty = false
	st.timer = time.AfterFunc(c.window, func() { c.fire(key) })
	c.mu.Unlock()
	c.send(key)
}
