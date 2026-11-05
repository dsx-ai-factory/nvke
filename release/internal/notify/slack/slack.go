// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package slack posts notify messages through Slack's chat.postMessage.
//
// It speaks to the one endpoint it needs over net/http, so the module takes no
// Slack SDK as a dependency.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/release/internal/notify"
)

const (
	defaultBaseURL     = "https://slack.com/api"
	defaultAttempts    = 3
	defaultTimeout     = 10 * time.Second
	maxRetryAfter      = 30 * time.Second
	maxResponseBytes   = 1 << 20
	postMessageMethod  = "chat.postMessage"
	contentTypeJSONUTF = "application/json; charset=utf-8"
)

// Client posts messages as a Slack bot.
type Client struct {
	// Token is the bot token. It is sent only as the Authorization header and
	// is never part of an error.
	Token string
	// BaseURL is Slack's API root; empty means Slack's own. Tests point it at a
	// local server.
	BaseURL string
	HTTP    *http.Client
	// Attempts is how many times to try a post that fails in a way worth
	// retrying; zero means three.
	Attempts int
	// Sleep waits between attempts; nil waits for real. Tests replace it.
	Sleep func(context.Context, time.Duration) error
}

// colours by severity, as the hex Slack accepts for an attachment bar.
var colours = map[string]string{
	notify.SeverityInfo:    "#36c5f0",
	notify.SeveritySuccess: "#2eb886",
	notify.SeverityWarning: "#ecb22e",
	notify.SeverityError:   "#e01e5a",
}

type attachment struct {
	Color    string   `json:"color"`
	Title    string   `json:"title"`
	Text     string   `json:"text,omitempty"`
	Fallback string   `json:"fallback"`
	MrkdwnIn []string `json:"mrkdwn_in"`
}

type payload struct {
	Channel     string       `json:"channel"`
	Text        string       `json:"text,omitempty"`
	ThreadTS    string       `json:"thread_ts,omitempty"`
	Attachments []attachment `json:"attachments"`
	UnfurlLinks bool         `json:"unfurl_links"`
	UnfurlMedia bool         `json:"unfurl_media"`
}

type response struct {
	OK    bool   `json:"ok"`
	TS    string `json:"ts"`
	Error string `json:"error"`
}

// Post sends a message and returns its timestamp.
//
// Mentions go in the message's own text and not in the attachment, because
// Slack notifies for a mention only there.
//
// A post is retried only when the request certainly was not processed: a rate
// limit (honouring Retry-After), Slack saying it is unavailable, and a failure
// to connect at all. chat.postMessage takes no idempotency key, so anything that
// leaves it unknown whether Slack acted, such as a dropped response, a timeout
// after the request was sent, a 500 or fatal_error, is not retried. A lost
// notification is the lesser harm: a retry would post the message twice, and
// orphan the thread root the first copy started. Anything else Slack refuses is
// final, since asking again gets the same answer.
func (c *Client) Post(ctx context.Context, m notify.Message) (string, error) {
	colour, ok := colours[m.Severity]
	if !ok {
		colour = colours[notify.SeverityInfo]
	}
	body, err := json.Marshal(payload{
		Channel:  m.Channel,
		Text:     strings.Join(m.Mentions, " "),
		ThreadTS: m.ThreadTS,
		Attachments: []attachment{{
			Color:    colour,
			Title:    m.Title,
			Text:     m.Body,
			Fallback: m.Title,
			MrkdwnIn: []string{"text"},
		}},
	})
	if err != nil {
		return "", fmt.Errorf("encoding the message: %w", err)
	}

	attempts := c.Attempts
	if attempts <= 0 {
		attempts = defaultAttempts
	}
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		ts, wait, err := c.once(ctx, body)
		if err == nil {
			return ts, nil
		}
		last = err
		if wait < 0 || attempt == attempts {
			break
		}
		if err := c.sleep(ctx, wait); err != nil {
			return "", err
		}
	}
	return "", last
}

// once makes one attempt. A negative wait means the failure is final; otherwise
// it is how long to wait before the next.
func (c *Client) once(ctx context.Context, body []byte) (string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/"+postMessageMethod, bytes.NewReader(body))
	if err != nil {
		return "", -1, fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", contentTypeJSONUTF)

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// Only a failure to connect proves nothing was sent. Any other error
		// may have come after Slack read the request.
		if neverSent(err) && ctx.Err() == nil {
			return "", time.Second, fmt.Errorf("%s: %w", postMessageMethod, err)
		}
		return "", -1, fmt.Errorf("%s: %w (not retried: the message may have been delivered)", postMessageMethod, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", retryAfter(resp.Header.Get("Retry-After")), fmt.Errorf("%s: rate limited", postMessageMethod)
	case resp.StatusCode == http.StatusServiceUnavailable:
		return "", time.Second, fmt.Errorf("%s: Slack answered %d", postMessageMethod, resp.StatusCode)
	case resp.StatusCode >= http.StatusInternalServerError:
		return "", -1, fmt.Errorf("%s: Slack answered %d (not retried: the message may have been delivered)", postMessageMethod, resp.StatusCode)
	}

	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", -1, fmt.Errorf("%s: Slack answered %d with something that is not JSON", postMessageMethod, resp.StatusCode)
	}
	if out.OK {
		return out.TS, 0, nil
	}
	err = fmt.Errorf("%s: %s%s", postMessageMethod, out.Error, hint(out.Error))
	switch out.Error {
	case "ratelimited":
		return "", retryAfter(resp.Header.Get("Retry-After")), err
	case "service_unavailable", "request_timeout":
		// Slack did not act on the request.
		return "", time.Second, err
	case "internal_error", "fatal_error":
		// Slack documents that fatal_error can follow a message that was posted.
		return "", -1, fmt.Errorf("%w (not retried: the message may have been delivered)", err)
	}
	return "", -1, err
}

// neverSent reports whether a request failed before anything reached Slack: the
// connection was refused, or the name did not resolve.
func neverSent(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
}

// hint explains the refusals people meet most.
func hint(code string) string {
	switch code {
	case "not_in_channel":
		return " (invite the app to the channel)"
	case "channel_not_found":
		return " (check channel_id, and that the app is in the channel)"
	case "invalid_auth", "not_authed", "token_revoked", "account_inactive":
		return " (the bot token is not valid)"
	case "missing_scope":
		return " (the app needs the chat:write scope)"
	}
	return ""
}

// retryAfter reads a Retry-After header in seconds, bounded so a hostile or
// broken value cannot hold a CI step for minutes.
//
// The bound is applied to the seconds, before they become a Duration: a large
// enough value overflows the multiplication into a negative wait, which the
// caller reads as "do not retry".
func retryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	secs, err := strconv.ParseInt(header, 10, 64)
	switch {
	case errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(header, "-"):
		return maxRetryAfter
	case err != nil || secs < 1:
		return time.Second
	case secs > int64(maxRetryAfter/time.Second):
		return maxRetryAfter
	}
	return time.Duration(secs) * time.Second
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
