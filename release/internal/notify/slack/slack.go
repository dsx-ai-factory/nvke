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
// Slack notifies for a mention only there. A rate limit and a server error are
// retried, honouring Retry-After; anything else Slack refuses is final, since
// asking again gets the same answer.
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
		// A cancelled run is not worth retrying; a dropped connection is.
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
			return "", -1, err
		}
		return "", time.Second, fmt.Errorf("%s: %w", postMessageMethod, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", retryAfter(resp.Header.Get("Retry-After")), fmt.Errorf("%s: rate limited", postMessageMethod)
	case resp.StatusCode >= http.StatusInternalServerError:
		return "", time.Second, fmt.Errorf("%s: Slack answered %d", postMessageMethod, resp.StatusCode)
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
	case "internal_error", "fatal_error", "service_unavailable", "request_timeout":
		return "", time.Second, err
	}
	return "", -1, err
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
func retryAfter(header string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || secs < 1 {
		return time.Second
	}
	return min(time.Duration(secs)*time.Second, maxRetryAfter)
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
