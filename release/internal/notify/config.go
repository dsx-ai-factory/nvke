// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package notify routes events to Slack from configuration.
//
// Three things are kept apart so that routing is configuration and not
// workflow code. Subscribers say who receives and where. Notifications say what
// is sent: a title, a severity and a template. Subscriptions connect event
// patterns to notifications for a subscriber. A workflow only names the event;
// who hears about it, and with what words, is the config's decision.
package notify

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"text/template"
	"text/template/parse"

	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/release/internal/notify/githubctx"
)

// Severities a notification may carry. They choose the colour of the message.
const (
	SeverityInfo    = "info"
	SeveritySuccess = "success"
	SeverityWarning = "warning"
	SeverityError   = "error"
)

// Thread modes. The default is to reply in the conversation's thread, starting
// it when there is none.
const (
	ThreadReply = "reply"
	ThreadNone  = "none"
)

// Config is the contents of a notify config file.
type Config struct {
	Subscribers   []Subscriber   `json:"subscribers"`
	Notifications []Notification `json:"notifications"`
	Subscriptions []Subscription `json:"subscriptions"`
}

// Subscriber is someone who receives notifications.
type Subscriber struct {
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	Channels Channels `json:"channels"`
}

// Channels holds a subscriber's contact details per transport.
type Channels struct {
	Slack *SlackChannel `json:"slack,omitempty"`
}

// SlackChannel is a Slack conversation, by ID. The ID and not the name, because
// a name changes when a channel is renamed and an ID does not.
type SlackChannel struct {
	ChannelID string `json:"channel_id"`
}

// Notification is what is sent for an event.
type Notification struct {
	Name string `json:"name"`
	// Title and MessageTemplate are text/template over .Context and .Event.
	Title           string `json:"title"`
	MessageTemplate string `json:"message_template"`
	// Severity is one of the Severity constants; empty means info.
	Severity string `json:"severity,omitempty"`
	// Thread is a Thread constant; empty means reply.
	Thread string `json:"thread,omitempty"`
}

// Subscription connects event patterns to notifications for a subscriber.
type Subscription struct {
	Subscriber string `json:"subscriber"`
	// AdditionalMentions are Slack mentions, such as <!subteam^S123|@oncall>,
	// added to the message so that it notifies the people they name.
	AdditionalMentions []string   `json:"additional_mentions,omitempty"`
	Events             []EventRef `json:"events"`
}

// EventRef maps an event pattern to the notification sent for it.
type EventRef struct {
	// Name is an event name, or a pattern ending in "*": "qualification:*".
	Name         string `json:"name"`
	Notification string `json:"notification"`
}

// Load reads and validates the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the notify config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse reads and validates a config. A field it does not know is an error,
// because the usual cause is a misspelt one, which would otherwise be ignored
// and leave a notification that never fires.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing the notify config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var (
	channelID  = regexp.MustCompile(`^[A-Z][A-Z0-9]{8,}$`)
	mentionRe  = regexp.MustCompile(`^<[@!][^<>\s]+(\|[^<>]*)?>$`)
	patternRe  = regexp.MustCompile(`^(\*|[^\s*]+\*?)$`)
	severities = map[string]bool{SeverityInfo: true, SeveritySuccess: true, SeverityWarning: true, SeverityError: true}
)

// Validate reports every problem in the config, not just the first, so an
// author fixes them in one pass.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	subscribers := map[string]bool{}
	for i, s := range c.Subscribers {
		switch {
		case s.ID == "":
			fail("subscribers[%d]: id is empty", i)
		case subscribers[s.ID]:
			fail("subscribers[%d]: id %q is used twice", i, s.ID)
		}
		subscribers[s.ID] = true
		if s.Channels.Slack == nil {
			fail("subscriber %q: has no slack channel", s.ID)
		} else if !channelID.MatchString(s.Channels.Slack.ChannelID) {
			fail("subscriber %q: slack channel_id %q is not a channel ID (such as C0123456789)", s.ID, s.Channels.Slack.ChannelID)
		}
	}

	notifications := map[string]bool{}
	for i, n := range c.Notifications {
		switch {
		case n.Name == "":
			fail("notifications[%d]: name is empty", i)
		case notifications[n.Name]:
			fail("notifications[%d]: name %q is used twice", i, n.Name)
		}
		notifications[n.Name] = true
		if n.Severity != "" && !severities[n.Severity] {
			fail("notification %q: severity %q is not info, success, warning or error", n.Name, n.Severity)
		}
		if n.Thread != "" && n.Thread != ThreadReply && n.Thread != ThreadNone {
			fail("notification %q: thread %q is not %q or %q", n.Name, n.Thread, ThreadReply, ThreadNone)
		}
		if strings.TrimSpace(n.Title) == "" {
			fail("notification %q: title is empty", n.Name)
		}
		for field, text := range map[string]string{"title": n.Title, "message_template": n.MessageTemplate} {
			if _, err := References(text); err != nil {
				fail("notification %q: %s: %w", n.Name, field, err)
			}
		}
	}

	for i, sub := range c.Subscriptions {
		if !subscribers[sub.Subscriber] {
			fail("subscriptions[%d]: subscriber %q is not defined", i, sub.Subscriber)
		}
		for _, m := range sub.AdditionalMentions {
			if !mentionRe.MatchString(m) {
				fail("subscriptions[%d]: mention %q is not a Slack mention such as <!subteam^S123|@oncall>", i, m)
			}
		}
		if len(sub.Events) == 0 {
			fail("subscriptions[%d]: has no events", i)
		}
		for _, e := range sub.Events {
			if !patternRe.MatchString(e.Name) {
				fail("subscriptions[%d]: event %q is not a name, a pattern with a single trailing *, or *", i, e.Name)
			}
			if !notifications[e.Notification] {
				fail("subscriptions[%d]: event %q names notification %q, which is not defined", i, e.Name, e.Notification)
			}
		}
	}
	return errors.Join(errs...)
}

// Match is one notification to send for an event.
type Match struct {
	Subscriber   Subscriber
	Notification Notification
	Mentions     []string
}

// Match returns what to send for an event: for each subscription, the
// notification of the first event pattern that matches. First, so one
// subscription sends one message for one event however many patterns it holds.
func (c *Config) Match(event string) []Match {
	var matches []Match
	for _, sub := range c.Subscriptions {
		for _, ref := range sub.Events {
			if !matchEvent(ref.Name, event) {
				continue
			}
			matches = append(matches, Match{
				Subscriber:   c.subscriber(sub.Subscriber),
				Notification: c.notification(ref.Notification),
				Mentions:     sub.AdditionalMentions,
			})
			break
		}
	}
	return matches
}

func (c *Config) subscriber(id string) Subscriber {
	for _, s := range c.Subscribers {
		if s.ID == id {
			return s
		}
	}
	return Subscriber{}
}

func (c *Config) notification(name string) Notification {
	for _, n := range c.Notifications {
		if n.Name == name {
			return n
		}
	}
	return Notification{}
}

// matchEvent reports whether an event name matches a pattern: exactly, or by
// prefix when the pattern ends in "*". A bare "*" matches everything.
func matchEvent(pattern, event string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(event, prefix)
	}
	return pattern == event
}

// Refs are the keys a template reads.
type Refs struct {
	Context []string
	Event   []string
}

// References parses a template and returns the keys it reads, or an error when
// it does not parse or reaches past a single key.
//
// A template may only name one key at a time, as .Context.run_url. It may not
// pass .Context, .Event, or the dot or $ that holds both to anything whole,
// because that is how a template would print every value it holds, and it may
// not read a Context key outside the allowlist.
func References(text string) (Refs, error) {
	tmpl, err := template.New("t").Option("missingkey=error").Parse(text)
	if err != nil {
		return Refs{}, fmt.Errorf("does not parse: %w", err)
	}

	allowed := map[string]bool{}
	for _, k := range githubctx.Keys() {
		allowed[k] = true
	}

	var (
		refs Refs
		errs []error
	)
	seen := map[string]bool{}
	record := func(ident []string) {
		if len(ident) == 0 {
			return
		}
		switch ident[0] {
		case "Context", "Event":
		default:
			errs = append(errs, fmt.Errorf("reads .%s: only .Context and .Event exist", strings.Join(ident, ".")))
			return
		}
		if len(ident) != 2 {
			errs = append(errs, fmt.Errorf("uses .%s whole; name one key, such as .%s.run_url", strings.Join(ident, "."), ident[0]))
			return
		}
		if ident[0] == "Context" && !allowed[ident[1]] {
			errs = append(errs, fmt.Errorf("reads .Context.%s, which is not an available key", ident[1]))
			return
		}
		if seen[ident[0]+"."+ident[1]] {
			return
		}
		seen[ident[0]+"."+ident[1]] = true
		if ident[0] == "Context" {
			refs.Context = append(refs.Context, ident[1])
		} else {
			refs.Event = append(refs.Event, ident[1])
		}
	}

	for _, t := range tmpl.Templates() {
		if t.Tree == nil {
			continue
		}
		walk(t.Root, func(n parse.Node) {
			switch n := n.(type) {
			case *parse.FieldNode:
				record(n.Ident)
			case *parse.DotNode:
				// {{.}} is the whole Data: both maps, with whatever the caller
				// put in the event.
				errs = append(errs, errors.New("uses . whole; name one key, such as .Event.stream"))
			case *parse.VariableNode:
				switch {
				case len(n.Ident) == 1 && n.Ident[0] == "$":
					// $ is the same whole Data as a bare dot.
					errs = append(errs, errors.New("uses $ whole; name one key, such as $.Event.stream"))
				case len(n.Ident) > 1 && n.Ident[0] == "$":
					// $.Context.x reaches the data the same way .Context.x does.
					record(n.Ident[1:])
				}
			}
		})
	}
	return refs, errors.Join(errs...)
}

// walk calls visit for every node under n.
func walk(n parse.Node, visit func(parse.Node)) {
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			walk(c, visit)
		}
	case *parse.ActionNode:
		walk(n.Pipe, visit)
	case *parse.PipeNode:
		if n == nil {
			return
		}
		for _, c := range n.Cmds {
			walk(c, visit)
		}
	case *parse.CommandNode:
		for _, a := range n.Args {
			walk(a, visit)
		}
	case *parse.IfNode:
		walkBranch(&n.BranchNode, visit)
	case *parse.RangeNode:
		walkBranch(&n.BranchNode, visit)
	case *parse.WithNode:
		walkBranch(&n.BranchNode, visit)
	case *parse.TemplateNode:
		walk(n.Pipe, visit)
	case *parse.ChainNode:
		walk(n.Node, visit)
	case nil:
		return
	}
	visit(n)
}

func walkBranch(b *parse.BranchNode, visit func(parse.Node)) {
	walk(b.Pipe, visit)
	walk(b.List, visit)
	walk(b.ElseList, visit)
}
