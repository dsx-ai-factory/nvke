// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"slices"
	"strings"
	"testing"
)

// valid is a config every test below breaks in one way.
const valid = `
subscribers:
  - id: team
    channels:
      slack:
        channel_id: C0123456789
notifications:
  - name: failed
    title: "Failed {{.Event.stream}}"
    message_template: "<{{.Context.run_url}}|run>"
    severity: error
subscriptions:
  - subscriber: team
    events:
      - name: "qualification:*"
        notification: failed
`

func TestParseValid(t *testing.T) {
	if _, err := Parse([]byte(valid)); err != nil {
		t.Fatal(err)
	}
}

func TestParseRejects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace [2]string
		want    string
	}{
		{"unknown field", [2]string{"severity: error", "severty: error"}, "severty"},
		{"unknown subscriber", [2]string{"subscriber: team", "subscriber: nobody"}, `subscriber "nobody"`},
		{"unknown notification", [2]string{"notification: failed", "notification: missing"}, `notification "missing"`},
		{"bad severity", [2]string{"severity: error", "severity: loud"}, "severity"},
		{"bad thread", [2]string{"severity: error", "thread: sideways"}, "thread"},
		{"channel name, not ID", [2]string{"C0123456789", "general"}, "channel ID"},
		{"template does not parse", [2]string{`"Failed {{.Event.stream}}"`, `"Failed {{.Event.stream"`}, "does not parse"},
		{"context key outside the allowlist", [2]string{".Context.run_url", ".Context.github_token"}, "not an available key"},
		{"whole context", [2]string{`"<{{.Context.run_url}}|run>"`, `"{{printf \"%v\" .Context}}"`}, "whole"},
		{"whole event", [2]string{`"Failed {{.Event.stream}}"`, `"{{.Event}}"`}, "whole"},
		{"unknown top-level field", [2]string{".Event.stream", ".Env.stream"}, "only .Context and .Event"},
		{"pattern with a mid-string wildcard", [2]string{`"qualification:*"`, `"qual*:x"`}, "single trailing *"},
		{"empty title", [2]string{`"Failed {{.Event.stream}}"`, `""`}, "title is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(valid, tc.replace[0], tc.replace[1], 1)
			if src == valid {
				t.Fatal("the replacement changed nothing")
			}
			_, err := Parse([]byte(src))
			if err == nil {
				t.Fatal("Parse accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestParseRejectsBadMention(t *testing.T) {
	src := strings.Replace(valid, "    events:", "    additional_mentions: [\"@oncall\"]\n    events:", 1)
	if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "mention") {
		t.Errorf("err = %v, want one about the mention", err)
	}
}

func TestParseReportsEveryProblem(t *testing.T) {
	src := strings.NewReplacer("severity: error", "severity: loud", "subscriber: team", "subscriber: nobody").Replace(valid)
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "severity") || !strings.Contains(err.Error(), "nobody") {
		t.Errorf("err = %v, want both problems", err)
	}
}

func TestMatchEvent(t *testing.T) {
	for _, tc := range []struct {
		pattern, event string
		want           bool
	}{
		{"qualification:stream-failed", "qualification:stream-failed", true},
		{"qualification:stream-failed", "qualification:stream-failed-late", false},
		{"qualification:*", "qualification:stream-failed", true},
		{"qualification:*", "release:stream-failed", false},
		{"*", "anything", true},
		{"qualification:stream-*", "qualification:completed", false},
	} {
		if got := matchEvent(tc.pattern, tc.event); got != tc.want {
			t.Errorf("matchEvent(%q, %q) = %v, want %v", tc.pattern, tc.event, got, tc.want)
		}
	}
}

// One subscription sends one message for one event, even when two of its
// patterns match. Two subscriptions each send their own.
func TestMatchFirstPatternPerSubscription(t *testing.T) {
	cfg, err := Parse([]byte(`
subscribers:
  - id: team
    channels: {slack: {channel_id: C0123456789}}
notifications:
  - {name: a, title: A, message_template: a}
  - {name: b, title: B, message_template: b}
subscriptions:
  - subscriber: team
    events:
      - {name: "q:failed", notification: a}
      - {name: "q:*", notification: b}
  - subscriber: team
    additional_mentions: ["<!here>"]
    events:
      - {name: "q:*", notification: b}
`))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Match("q:failed")
	if len(got) != 2 || got[0].Notification.Name != "a" || got[1].Notification.Name != "b" {
		t.Fatalf("Match = %+v, want a then b", got)
	}
	if len(got[0].Mentions) != 0 || !slices.Equal(got[1].Mentions, []string{"<!here>"}) {
		t.Errorf("mentions = %v, %v", got[0].Mentions, got[1].Mentions)
	}
	if len(cfg.Match("other:event")) != 0 {
		t.Error("an event no pattern covers matched")
	}
}

// The config this repository ships has to load, and every template in it has to
// render from the keys it declares, with nothing left unset.
func TestShippedConfig(t *testing.T) {
	cfg, err := Load("../../notify.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range cfg.Notifications {
		event := map[string]string{}
		for _, text := range []string{n.Title, n.MessageTemplate} {
			refs, err := References(text)
			if err != nil {
				t.Fatalf("%s: %v", n.Name, err)
			}
			for _, k := range refs.Event {
				event[k] = "value"
			}
		}
		ctx := map[string]string{}
		for _, k := range []string{"run_url", "run_number", "commit_url", "sha_short", "ref_name", "event_name", "actor"} {
			ctx[k] = "value"
		}
		if _, err := Render(n, Data{Context: ctx, Event: event}); err != nil {
			t.Errorf("%s does not render from the keys it reads: %v", n.Name, err)
		}
	}

	// Every event the ticket names has a notification behind it.
	for _, event := range []string{
		"qualification:tag-cut", "qualification:started", "qualification:plan-resolved",
		"qualification:awaiting-approval", "qualification:artifacts-ready", "qualification:artifacts-timeout",
		"qualification:stream-started", "qualification:stream-passed", "qualification:stream-failed",
		"qualification:completed",
	} {
		if len(cfg.Match(event)) == 0 {
			t.Errorf("no subscription for %s", event)
		}
	}
}
