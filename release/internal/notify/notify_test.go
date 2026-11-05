// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRenderEscapesValuesNotTemplate(t *testing.T) {
	msg, err := Render(Notification{
		Name:            "n",
		Title:           "Failed {{.Event.stream}}",
		MessageTemplate: "<{{.Context.run_url}}|run> {{.Event.line}}\n",
		Severity:        SeverityError,
	}, Data{
		Context: map[string]string{"run_url": "https://x/runs/1?a=1&b=2"},
		Event:   map[string]string{"stream": "a<b", "line": "ping <!channel> & <@U1>"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "Failed a&lt;b"; msg.Title != want {
		t.Errorf("Title = %q, want %q", msg.Title, want)
	}
	want := "<https://x/runs/1?a=1&amp;b=2|run> ping &lt;!channel&gt; &amp; &lt;@U1&gt;"
	if msg.Body != want {
		t.Errorf("Body = %q, want %q", msg.Body, want)
	}
	if msg.Severity != SeverityError {
		t.Errorf("Severity = %q", msg.Severity)
	}
}

func TestRenderMissingKeyIsAnError(t *testing.T) {
	_, err := Render(Notification{Name: "n", Title: "t", MessageTemplate: "{{.Event.stream}}"}, Data{Event: map[string]string{}})
	if err == nil {
		t.Fatal("a template read a key that was not passed and rendered anyway")
	}
}

func TestRenderDefaultsSeverityToInfo(t *testing.T) {
	msg, err := Render(Notification{Name: "n", Title: "t"}, Data{})
	if err != nil || msg.Severity != SeverityInfo {
		t.Errorf("Severity = %q, %v", msg.Severity, err)
	}
}

// fakePoster records what it is asked to post and hands out timestamps.
type fakePoster struct {
	posted []Message
	fail   map[string]error // by title
	next   int
}

func (p *fakePoster) Post(_ context.Context, m Message) (string, error) {
	if err := p.fail[m.Title]; err != nil {
		return "", err
	}
	p.posted = append(p.posted, m)
	p.next++
	return "1700000000.00000" + string(rune('0'+p.next)), nil
}

const twoChannels = `
subscribers:
  - id: one
    channels: {slack: {channel_id: C0000000001}}
  - id: two
    channels: {slack: {channel_id: C0000000002}}
notifications:
  - {name: start, title: Start, message_template: "s"}
  - {name: fail, title: "Fail {{.Event.stream}}", message_template: "f", severity: error}
  - {name: loose, title: Loose, message_template: "l", thread: none}
subscriptions:
  - subscriber: one
    events:
      - {name: "q:start", notification: start}
      - {name: "q:loose", notification: loose}
  - subscriber: one
    additional_mentions: ["<!subteam^S1|@oncall>"]
    events:
      - {name: "q:fail", notification: fail}
  - subscriber: two
    events:
      - {name: "q:start", notification: start}
`

func notifier(t *testing.T, src string, p Poster, s State, secrets ...string) *Notifier {
	t.Helper()
	cfg, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return &Notifier{Config: cfg, Poster: p, State: s, Secrets: secrets}
}

func TestTriggerStartsThenRepliesInTheThread(t *testing.T) {
	p, s := &fakePoster{}, &MemoryState{}
	n := notifier(t, twoChannels, p, s)
	data := Data{Event: map[string]string{"stream": "gb200"}}

	res, err := n.Trigger(context.Background(), "q:start", data, "v1")
	if err != nil || res.Posted != 2 || !res.StateCreated {
		t.Fatalf("first trigger = %+v, %v; want two posts and a new thread", res, err)
	}
	// Two channels, two roots, neither a reply.
	for _, m := range p.posted {
		if m.ThreadTS != "" {
			t.Errorf("first message in %s replied in %q", m.Channel, m.ThreadTS)
		}
	}

	p.posted = nil
	res, err = n.Trigger(context.Background(), "q:fail", data, "v1")
	if err != nil || res.Posted != 1 || res.StateCreated {
		t.Fatalf("second trigger = %+v, %v; want one reply and no new thread", res, err)
	}
	root, _ := s.Get("v1", "C0000000001")
	if m := p.posted[0]; m.Channel != "C0000000001" || m.ThreadTS != root || root == "" {
		t.Errorf("reply = %+v, want a reply under %q in channel one", m, root)
	}
	if m := p.posted[0]; m.Severity != SeverityError || len(m.Mentions) != 1 {
		t.Errorf("failure = %+v, want error severity and the on-call mention", m)
	}
}

// Each channel has its own thread: a ts means nothing in another channel.
func TestTriggerKeepsThreadsPerChannel(t *testing.T) {
	s := &MemoryState{}
	n := notifier(t, twoChannels, &fakePoster{}, s)
	if _, err := n.Trigger(context.Background(), "q:start", Data{}, "v1"); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Get("v1", "C0000000001")
	b, _ := s.Get("v1", "C0000000002")
	if a == "" || b == "" || a == b {
		t.Errorf("threads = %q and %q, want one distinct thread per channel", a, b)
	}
}

func TestTriggerThreadNoneStaysOutOfTheThread(t *testing.T) {
	p, s := &fakePoster{}, &MemoryState{}
	n := notifier(t, twoChannels, p, s)
	res, err := n.Trigger(context.Background(), "q:loose", Data{}, "v1")
	if err != nil || res.StateCreated {
		t.Fatalf("= %+v, %v; a message outside the thread must not start one", res, err)
	}
	if got, _ := s.Get("v1", "C0000000001"); got != "" {
		t.Errorf("state = %q, want none", got)
	}
}

// A failed post must not record a thread that was never created, and must not
// stop the other messages.
func TestTriggerFailureDoesNotStopTheRest(t *testing.T) {
	p := &fakePoster{fail: map[string]error{"Start": errors.New("boom")}}
	s := &MemoryState{}
	n := notifier(t, twoChannels, p, s)

	res, err := n.Trigger(context.Background(), "q:start", Data{}, "v1")
	if err == nil || res.Posted != 0 || res.StateCreated {
		t.Fatalf("= %+v, %v; want errors, nothing posted, no state", res, err)
	}
	if got, _ := s.Get("v1", "C0000000001"); got != "" {
		t.Errorf("state = %q after a failed post", got)
	}

	p = &fakePoster{fail: map[string]error{"Fail a": errors.New("boom")}}
	n = notifier(t, twoChannels, p, &MemoryState{})
	if _, err := n.Trigger(context.Background(), "q:start", Data{}, "v1"); err != nil {
		t.Fatal(err)
	}
}

func TestTriggerWithNoMatchSendsNothing(t *testing.T) {
	p := &fakePoster{}
	res, err := notifier(t, twoChannels, p, &MemoryState{}).Trigger(context.Background(), "q:unknown", Data{}, "v1")
	if err != nil || res.Matched != 0 || len(p.posted) != 0 {
		t.Errorf("= %+v, %v, %d posts", res, err, len(p.posted))
	}
}

// The token reaches a message only through a value a workflow passed. It is
// removed, and so is any other token shaped like Slack's.
func TestTriggerScrubsSecrets(t *testing.T) {
	p := &fakePoster{}
	n := notifier(t, twoChannels, p, &MemoryState{}, "super-secret-value")
	data := Data{Event: map[string]string{"stream": "a super-secret-value xoxb-123-abc xoxp-9-z"}}

	if _, err := n.Trigger(context.Background(), "q:fail", data, "v1"); err != nil {
		t.Fatal(err)
	}
	m := p.posted[0]
	for _, field := range []string{m.Title, m.Body, strings.Join(m.Mentions, " ")} {
		for _, leaked := range []string{"super-secret-value", "xoxb-", "xoxp-"} {
			if strings.Contains(field, leaked) {
				t.Errorf("%q leaked into %q", leaked, field)
			}
		}
	}
	if !strings.Contains(m.Title, redacted) {
		t.Errorf("Title = %q, want the secret replaced", m.Title)
	}
}

// Scrubbing a message must not edit the config's own mention list.
func TestScrubDoesNotEditConfig(t *testing.T) {
	n := notifier(t, twoChannels, &fakePoster{}, &MemoryState{}, "S1")
	before := append([]string(nil), n.Config.Subscriptions[1].AdditionalMentions...)
	if _, err := n.Trigger(context.Background(), "q:fail", Data{Event: map[string]string{"stream": "x"}}, "v1"); err != nil {
		t.Fatal(err)
	}
	if got := n.Config.Subscriptions[1].AdditionalMentions; got[0] != before[0] {
		t.Errorf("mentions changed from %v to %v", before, got)
	}
}
