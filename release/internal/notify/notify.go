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

	"github.com/dsx-ai-factory/nvke/release/internal/notify/provider"
)

// State remembers the thread a conversation lives in, per recipient.
type State interface {
	// Get returns the conversation ID, or "" when the conversation has none.
	Get(key, recipient string) (string, error)
	Set(key, recipient, id string) error
}

// Notifier turns an event into messages.
type Notifier struct {
	Config *Config
	// Providers deliver the messages, by the kind of destination a subscriber
	// names: "slack". They are passed in, and not looked up in a registry, so a
	// test or a dry run supplies its own.
	Providers map[string]provider.Provider
	State     State
	// Secrets are values that must never leave in a message, such as the bot
	// token. They are removed from every message before it is sent.
	Secrets []string
	// ReplyOnly never starts a thread. A threaded message with no thread to
	// reply in is not sent and is reported as an error. Jobs that run in
	// parallel set it, so only the job that is meant to open the thread can.
	ReplyOnly bool
}

// kindSlack is the destination kind of a subscriber with a Slack channel.
const kindSlack = "slack"

// Result says what Trigger did.
type Result struct {
	// Matched is how many messages the config called for.
	Matched int
	// Posted is how many were sent.
	Posted int
	// StateCreated is true when a message started a thread and its ID was
	// recorded. A caller that persists state between jobs saves it only then.
	StateCreated bool
}

// Trigger sends what the config calls for on an event: it matches the event and
// fans out over the notifications that match, each of which sends itself.
//
// All of them are attempted. A failure of one is returned with the rest and
// does not stop the others, so one broken notification does not silence its
// neighbours.
func (n *Notifier) Trigger(ctx context.Context, event string, data Data, key string) (Result, error) {
	matches := n.Config.Match(event)
	res := Result{Matched: len(matches)}

	// The conversation is kept for the rest of this trigger, whether or not
	// saving it worked: two subscriptions can match one event for the same
	// channel, and the second message belongs in the thread the first started.
	state := &retainedState{base: n.State}

	var errs []error
	for _, m := range matches {
		p, ok := n.Providers[kindSlack]
		if !ok {
			errs = append(errs, fmt.Errorf("sending %q: no %s provider", m.Notification.Name, kindSlack))
			continue
		}
		sent, err := m.Notification.Send(ctx, Delivery{
			Provider:  p,
			Kind:      kindSlack,
			Recipient: m.Subscriber.Channels.Slack.ChannelID,
			Mentions:  m.Mentions,
			Data:      data,
			State:     state,
			Key:       key,
			ReplyOnly: n.ReplyOnly,
			Secrets:   n.Secrets,
		})
		if sent.Posted {
			res.Posted++
		}
		if sent.StateCreated {
			res.StateCreated = true
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

// retainedState keeps every thread it is given in memory, then saves it to the
// base. A failed save is reported, and the thread is still remembered for the
// messages that follow in the same trigger.
type retainedState struct {
	base State
	mem  MemoryState
}

// Get implements State.
func (s *retainedState) Get(key, recipient string) (string, error) {
	if id, _ := s.mem.Get(key, recipient); id != "" {
		return id, nil
	}
	return s.base.Get(key, recipient)
}

// Set implements State.
func (s *retainedState) Set(key, recipient, id string) error {
	_ = s.mem.Set(key, recipient, id)
	return s.base.Set(key, recipient, id)
}

// Delivery is where a notification goes, and the conversation it belongs to.
type Delivery struct {
	Provider provider.Provider
	// Kind is the kind of destination, such as "slack". With Recipient it keys
	// the conversation's saved state, since an ID means nothing across kinds.
	Kind      string
	Recipient string
	Mentions  []string
	Data      Data
	// State and Key hold the conversation: the thread the first message of a
	// Key started, for each Kind and Recipient.
	State State
	Key   string
	// ReplyOnly never starts a conversation; see Notifier.ReplyOnly.
	ReplyOnly bool
	// Secrets are removed from the message before it is sent.
	Secrets []string
}

// Sent says what a notification did.
type Sent struct {
	Posted       bool
	StateCreated bool
}

// Send renders the notification, removes secrets from it, and delivers it
// through the provider: as a reply when the conversation already has a thread,
// and as the first message of one when it does not, unless the notification's
// thread policy is "none" or the delivery is reply-only.
//
// A failure to read the thread is reported and the message still goes, as a
// new conversation: noisier than it should be, and better than silence. A
// failure to record a new thread is reported after the message was sent.
func (n Notification) Send(ctx context.Context, d Delivery) (Sent, error) {
	msg, err := Render(n, d.Data)
	if err != nil {
		return Sent{}, fmt.Errorf("rendering %q: %w", n.Name, err)
	}
	msg.Mentions = d.Mentions
	scrub(&msg, d.Secrets)

	var errs []error
	where := d.Kind + ":" + d.Recipient
	threaded := n.Thread != ThreadNone
	conversation := ""
	if threaded {
		id, err := d.State.Get(d.Key, where)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading the thread for %s: %w", d.Recipient, err))
		}
		conversation = id
		if conversation == "" && d.ReplyOnly {
			errs = append(errs, fmt.Errorf("not posting %q to %s: there is no thread to reply in and reply-only is set", n.Name, d.Recipient))
			return Sent{}, errors.Join(errs...)
		}
	}

	resp, err := d.Provider.Send(ctx, d.Recipient, msg.Title, msg.Body, msg.Severity, conversation, msg.Mentions)
	if err != nil {
		errs = append(errs, fmt.Errorf("posting %q to %s: %w", n.Name, d.Recipient, err))
		return Sent{}, errors.Join(errs...)
	}
	sent := Sent{Posted: true}

	if threaded && conversation == "" && resp != nil && resp.ConversationID != "" {
		if err := d.State.Set(d.Key, where, resp.ConversationID); err != nil {
			errs = append(errs, fmt.Errorf("recording the thread for %s: %w", d.Recipient, err))
		} else {
			sent.StateCreated = true
		}
	}
	return sent, errors.Join(errs...)
}

// slackToken is the shape of a Slack token, so one that is not the bot's own is
// removed as well: the xox families (bot, user, workspace, refresh, legacy), the
// xapp- app-level token, and the rotating forms Slack prefixes with xoxe.
var slackToken = regexp.MustCompile(`(?:xoxe\.)?(?:xox[a-z]|xapp)-[A-Za-z0-9-]+`)

const redacted = "[redacted]"

// scrub removes secrets from a message. The templates and the context are built
// not to carry one; this is the second line, for a value that arrives in an
// event a workflow passed.
func scrub(m *Message, secrets []string) {
	clean := func(s string) string {
		for _, secret := range secrets {
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

// OverlayState reads existing thread state from Base and keeps what Set records
// in memory, so a dry run sees the threads that exist and writes nothing.
type OverlayState struct {
	Base State
	mem  MemoryState
}

// Get implements State. A thread recorded in this run wins over the base.
func (s *OverlayState) Get(key, channel string) (string, error) {
	if ts, _ := s.mem.Get(key, channel); ts != "" {
		return ts, nil
	}
	return s.Base.Get(key, channel)
}

// Set implements State. It never touches Base.
func (s *OverlayState) Set(key, channel, ts string) error { return s.mem.Set(key, channel, ts) }

// WriterProvider prints each message instead of sending it, for a dry run.
type WriterProvider struct {
	W io.Writer
}

// Name implements provider.Provider.
func (p WriterProvider) Name() string { return "dry run" }

// Send implements provider.Provider.
func (p WriterProvider) Send(_ context.Context, recipientID, title, message, severity, conversationID string, mentions []string) (*provider.Response, error) {
	thread := "new thread"
	if conversationID != "" {
		thread = "reply in " + conversationID
	}
	fmt.Fprintf(p.W, "[dry run] %s (%s, %s)\n  %s\n", recipientID, severity, thread, title)
	if len(mentions) > 0 {
		fmt.Fprintf(p.W, "  mentions: %s\n", strings.Join(mentions, " "))
	}
	for _, line := range strings.Split(message, "\n") {
		fmt.Fprintf(p.W, "  %s\n", line)
	}
	return &provider.Response{ConversationID: "0000000000.000001"}, nil
}
