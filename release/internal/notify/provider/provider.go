// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package provider is the interface a delivery channel implements.
//
// It has the same shape as the provider interface of the notify tool the
// qualification pipeline runs in GitLab, so the two tools take a channel the
// same way. It differs in two places, on purpose. The conversation a message
// belongs to is a plain string, since a Slack thread_ts, an email In-Reply-To
// and a PagerDuty incident are all identifiers, where the reference passes an
// untyped map. And a provider is handed to the notifier and not looked up in a
// package-level registry, which would need a way to deregister it in tests.
package provider

import "context"

// Severity of a message, which a provider maps to whatever it has for emphasis,
// such as a colour.
const (
	SeverityInfo    = "info"
	SeveritySuccess = "success"
	SeverityWarning = "warning"
	SeverityError   = "error"
)

// Response is what a provider returns for a message it sent.
type Response struct {
	// ConversationID names the conversation the message started, or joined. A
	// later message passes it back to reply in it. Empty when the provider has
	// no such thing.
	ConversationID string
}

// Provider delivers messages to one kind of destination.
type Provider interface {
	// Name identifies the provider in errors and logs.
	Name() string
	// Send delivers one message to a recipient, such as a Slack channel ID. A
	// non-empty conversationID asks to reply in that conversation; empty starts a
	// new one. Mentions are provider-specific text that notifies someone.
	Send(ctx context.Context, recipientID, title, message, severity, conversationID string, mentions []string) (*Response, error)
}
