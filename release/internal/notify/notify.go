// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
)

// Poster sends a message and returns the timestamp Slack gave it.
type Poster interface {
	Post(ctx context.Context, m Message) (ts string, err error)
}

// State remembers the thread a conversation lives in, per channel.
type State interface {
	// Get returns the thread timestamp, or "" when the conversation has none.
	Get(key, channel string) (string, error)
	Set(key, channel, ts string) error
}

// Notifier turns an event into messages.
type Notifier struct {
	Config *Config
	Poster Poster
	State  State
	// Secrets are values that must never leave in a message, such as the bot
	// token. They are removed from every message before it is posted.
	Secrets []string
}

// Result says what Trigger did.
type Result struct {
	// Matched is how many messages the config called for.
	Matched int
	// Posted is how many were sent.
	Posted int
	// StateCreated is true when a message started a thread and its timestamp
	// was recorded. A caller that persists state between jobs saves it only
	// then.
	StateCreated bool
}

// Trigger sends what the config calls for on an event.
//
// All messages are attempted. A failure to render or post one is returned with
// the rest and does not stop the others, so one broken notification does not
// silence its neighbours.
//
// The first message of a conversation in a channel posts as the thread's root
// and records its timestamp under stateKey. Every later one replies under it. A
// notification with thread "none" is never part of a thread.
func (n *Notifier) Trigger(ctx context.Context, event string, data Data, stateKey string) (Result, error) {
	matches := n.Config.Match(event)
	res := Result{Matched: len(matches)}

	var (
		errs    []error
		threads = map[string]string{}
		loaded  = map[string]bool{}
	)
	for _, m := range matches {
		channel := m.Subscriber.Channels.Slack.ChannelID

		msg, err := Render(m.Notification, data)
		if err != nil {
			errs = append(errs, fmt.Errorf("rendering %q: %w", m.Notification.Name, err))
			continue
		}
		msg.Channel = channel
		msg.Mentions = m.Mentions
		n.scrub(&msg)

		threaded := m.Notification.Thread != ThreadNone
		if threaded {
			if !loaded[channel] {
				ts, err := n.State.Get(stateKey, channel)
				if err != nil {
					// Without the thread this posts as a new root: noisier than
					// it should be, and better than silence.
					errs = append(errs, fmt.Errorf("reading the thread for %s: %w", channel, err))
				}
				threads[channel], loaded[channel] = ts, true
			}
			msg.ThreadTS = threads[channel]
		}

		ts, err := n.Poster.Post(ctx, msg)
		if err != nil {
			errs = append(errs, fmt.Errorf("posting %q to %s: %w", m.Notification.Name, channel, err))
			continue
		}
		res.Posted++

		if threaded && msg.ThreadTS == "" && ts != "" {
			threads[channel] = ts
			if err := n.State.Set(stateKey, channel, ts); err != nil {
				errs = append(errs, fmt.Errorf("recording the thread for %s: %w", channel, err))
				continue
			}
			res.StateCreated = true
		}
	}
	return res, errors.Join(errs...)
}

// slackToken is the shape of a Slack token, so one that is not the bot's own is
// removed as well: the xox families (bot, user, workspace, refresh, legacy), the
// xapp- app-level token, and the rotating forms Slack prefixes with xoxe.
var slackToken = regexp.MustCompile(`(?:xoxe\.)?(?:xox[a-z]|xapp)-[A-Za-z0-9-]+`)

const redacted = "[redacted]"

// scrub removes secrets from a message. The templates and the context are built
// not to carry one; this is the second line, for a value that arrives in an
// event a workflow passed.
func (n *Notifier) scrub(m *Message) {
	clean := func(s string) string {
		for _, secret := range n.Secrets {
			if secret != "" {
				s = strings.ReplaceAll(s, secret, redacted)
			}
		}
		return slackToken.ReplaceAllString(s, redacted)
	}
	m.Title = clean(m.Title)
	m.Body = clean(m.Body)
	// Copied, because the slice is the config's and a message must not edit it.
	mentions := make([]string, len(m.Mentions))
	for i, mention := range m.Mentions {
		mentions[i] = clean(mention)
	}
	m.Mentions = mentions
}

// MemoryState keeps thread state in memory. A dry run uses it so that nothing
// is written, and tests use it.
type MemoryState struct {
	mu sync.Mutex
	m  map[string]string
}

// Get implements State.
func (s *MemoryState) Get(key, channel string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[key+"~"+channel], nil
}

// Set implements State.
func (s *MemoryState) Set(key, channel, ts string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]string{}
	}
	s.m[key+"~"+channel] = ts
	return nil
}

// WriterPoster prints each message instead of sending it, for a dry run.
type WriterPoster struct {
	W io.Writer
}

// Post implements Poster.
func (p WriterPoster) Post(_ context.Context, m Message) (string, error) {
	thread := "new thread"
	if m.ThreadTS != "" {
		thread = "reply in " + m.ThreadTS
	}
	fmt.Fprintf(p.W, "[dry run] %s (%s, %s)\n  %s\n", m.Channel, m.Severity, thread, m.Title)
	if len(m.Mentions) > 0 {
		fmt.Fprintf(p.W, "  mentions: %s\n", strings.Join(m.Mentions, " "))
	}
	for _, line := range strings.Split(m.Body, "\n") {
		fmt.Fprintf(p.W, "  %s\n", line)
	}
	return "0000000000.000001", nil
}
