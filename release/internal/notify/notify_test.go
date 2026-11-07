// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/internal/notify/provider"
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

// sentMessage is a Message and where it went.
type sentMessage struct {
	Message
	Channel  string
	ThreadTS string
}

// fakeProvider records what it is asked to send and hands out timestamps.
type fakeProvider struct {
	posted []sentMessage
	fail   map[string]error // by recipient
	next   int
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Send(_ context.Context, recipientID, title, message, severity, conversationID string, mentions []string) (*provider.Response, error) {
	if err := p.fail[recipientID]; err != nil {
		return nil, err
	}
	p.posted = append(p.posted, sentMessage{
		Message:  Message{Title: title, Body: message, Severity: severity, Mentions: mentions},
		Channel:  recipientID,
		ThreadTS: conversationID,
	})
	p.next++
	return &provider.Response{ConversationID: "1700000000.00000" + string(rune('0'+p.next))}, nil
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

func notifier(t *testing.T, src string, p provider.Provider, s State, secrets ...string) *Notifier {
	t.Helper()
	cfg, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return &Notifier{Config: cfg, Providers: map[string]provider.Provider{"slack": p}, State: s, Secrets: secrets}
}

func TestTriggerStartsThenRepliesInTheThread(t *testing.T) {
	p, s := &fakeProvider{}, &MemoryState{}
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
	root, _ := s.Get("v1", "slack:C0000000001")
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
	n := notifier(t, twoChannels, &fakeProvider{}, s)
	if _, err := n.Trigger(context.Background(), "q:start", Data{}, "v1"); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Get("v1", "slack:C0000000001")
	b, _ := s.Get("v1", "slack:C0000000002")
	if a == "" || b == "" || a == b {
		t.Errorf("threads = %q and %q, want one distinct thread per channel", a, b)
	}
}

func TestTriggerThreadNoneStaysOutOfTheThread(t *testing.T) {
	p, s := &fakeProvider{}, &MemoryState{}
	n := notifier(t, twoChannels, p, s)
	res, err := n.Trigger(context.Background(), "q:loose", Data{}, "v1")
	if err != nil || res.StateCreated {
		t.Fatalf("= %+v, %v; a message outside the thread must not start one", res, err)
	}
	if got, _ := s.Get("v1", "slack:C0000000001"); got != "" {
		t.Errorf("state = %q, want none", got)
	}
}

// A failed post must not record a thread that was never created, and must not
// stop the other messages.
func TestTriggerFailureDoesNotStopTheRest(t *testing.T) {
	// Both channels fail: nothing posted, nothing recorded.
	p := &fakeProvider{fail: map[string]error{"C0000000001": errors.New("boom"), "C0000000002": errors.New("boom")}}
	s := &MemoryState{}
	res, err := notifier(t, twoChannels, p, s).Trigger(context.Background(), "q:start", Data{}, "v1")
	if err == nil || res.Posted != 0 || res.StateCreated {
		t.Fatalf("= %+v, %v; want errors, nothing posted, no state", res, err)
	}
	for _, ch := range []string{"slack:C0000000001", "slack:C0000000002"} {
		if got, _ := s.Get("v1", ch); got != "" {
			t.Errorf("state for %s = %q after a failed post", ch, got)
		}
	}

	// Only channel one fails: channel two still posts, and gets its thread.
	p = &fakeProvider{fail: map[string]error{"C0000000001": errors.New("boom")}}
	s = &MemoryState{}
	res, err = notifier(t, twoChannels, p, s).Trigger(context.Background(), "q:start", Data{}, "v1")
	if err == nil || res.Posted != 1 || !res.StateCreated || len(p.posted) != 1 || p.posted[0].Channel != "C0000000002" {
		t.Fatalf("= %+v, %v, %+v; want an error and one post, to channel two", res, err, p.posted)
	}
	if got, _ := s.Get("v1", "slack:C0000000001"); got != "" {
		t.Errorf("channel one state = %q, want none", got)
	}
	if got, _ := s.Get("v1", "slack:C0000000002"); got == "" {
		t.Error("channel two has no thread state")
	}
}

// failingSet reads fine and cannot save.
type failingSet struct{ MemoryState }

func (*failingSet) Set(_, _, _ string) error { return errors.New("disk full") }

// Two subscriptions can match one event for the same channel. The second message
// must reply in the thread the first started even when saving that thread
// failed: the ID is kept for the rest of the trigger, and the failure is still
// reported.
func TestTriggerKeepsTheThreadWhenSavingItFails(t *testing.T) {
	const twoSubscriptions = `
subscribers:
  - id: one
    channels: {slack: {channel_id: C0000000001}}
notifications:
  - {name: first, title: First, message_template: "a"}
  - {name: second, title: Second, message_template: "b"}
subscriptions:
  - subscriber: one
    events: [{name: "q:x", notification: first}]
  - subscriber: one
    events: [{name: "q:x", notification: second}]
`
	p := &fakeProvider{}
	n := notifier(t, twoSubscriptions, p, &failingSet{})
	res, err := n.Trigger(context.Background(), "q:x", Data{}, "v1")
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the failed save reported", err)
	}
	if res.Posted != 2 || len(p.posted) != 2 {
		t.Fatalf("posted %d, want 2", res.Posted)
	}
	if p.posted[0].ThreadTS != "" || p.posted[1].ThreadTS == "" {
		t.Errorf("threads = %q then %q, want a root and then a reply in it", p.posted[0].ThreadTS, p.posted[1].ThreadTS)
	}
	if res.StateCreated {
		t.Error("StateCreated is true though nothing was saved")
	}
}

func TestTriggerWithNoMatchSendsNothing(t *testing.T) {
	p := &fakeProvider{}
	res, err := notifier(t, twoChannels, p, &MemoryState{}).Trigger(context.Background(), "q:unknown", Data{}, "v1")
	if err != nil || res.Matched != 0 || len(p.posted) != 0 {
		t.Errorf("= %+v, %v, %d posts", res, err, len(p.posted))
	}
}

// The token reaches a message only through a value a workflow passed. It is
// removed, and so is any other token shaped like Slack's.
func TestTriggerScrubsSecrets(t *testing.T) {
	p := &fakeProvider{}
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
	n := notifier(t, twoChannels, &fakeProvider{}, &MemoryState{}, "S1")
	before := append([]string(nil), n.Config.Subscriptions[1].AdditionalMentions...)
	if _, err := n.Trigger(context.Background(), "q:fail", Data{Event: map[string]string{"stream": "x"}}, "v1"); err != nil {
		t.Fatal(err)
	}
	if got := n.Config.Subscriptions[1].AdditionalMentions; got[0] != before[0] {
		t.Errorf("mentions changed from %v to %v", before, got)
	}
}

// tok builds a fake token from two halves, so that no literal in the source is
// shaped like one and the secrets scan has nothing to flag.
func tok(prefix, rest string) string { return prefix + rest + "abcdef" + "ABCDEF" }

// Every family of Slack token is removed, not only the xox- ones, wherever it
// appears in a message.
func TestScrubCoversEveryTokenFamily(t *testing.T) {
	tokens := map[string]string{
		"bot":                tok("xox", "b-123456789012-"),
		"user":               tok("xox", "p-123456789012-"),
		"workspace":          tok("xox", "a-2-"),
		"refresh":            tok("xox", "r-"),
		"config":             tok("xox", "e-1-"),
		"app-level":          tok("xa", "pp-1-A0123456789-1234567890-"),
		"rotating user":      tok("xox", "e.xoxp-1-"),
		"rotating app-level": tok("xox", "e.xapp-1-A0123456789-1-"),
		"rotating bot":       tok("xox", "e.xoxb-1-"),
	}
	for name, token := range tokens {
		t.Run(name, func(t *testing.T) {
			m := Message{Title: "t " + token, Body: "before " + token + " after", Mentions: []string{"<!here> " + token}}
			scrub(&m, nil)
			for _, field := range []string{m.Title, m.Body, strings.Join(m.Mentions, " ")} {
				if strings.Contains(field, "abcdefABCDEF") || strings.Contains(field, "xapp-") || strings.Contains(field, "xox") {
					t.Errorf("%s token left in %q", name, field)
				}
			}
			if !strings.Contains(m.Body, "before "+redacted+" after") {
				t.Errorf("Body = %q, want only the token replaced", m.Body)
			}
		})
	}
}

// A job that runs in parallel must not open a competing thread.
func TestReplyOnlyNeverStartsAThread(t *testing.T) {
	p, s := &fakeProvider{}, &MemoryState{}
	n := notifier(t, twoChannels, p, s)
	n.ReplyOnly = true

	res, err := n.Trigger(context.Background(), "q:start", Data{}, "v1")
	if err == nil || res.Posted != 0 || res.StateCreated || len(p.posted) != 0 {
		t.Fatalf("= %+v, %v, posted %d; want an error and nothing posted", res, err, len(p.posted))
	}
	if got, _ := s.Get("v1", "slack:C0000000001"); got != "" {
		t.Errorf("state = %q, want none", got)
	}
}

func TestReplyOnlyRepliesInAnExistingThread(t *testing.T) {
	p, s := &fakeProvider{}, &MemoryState{}
	n := notifier(t, twoChannels, p, s)
	if _, err := n.Trigger(context.Background(), "q:start", Data{}, "v1"); err != nil {
		t.Fatal(err)
	}
	root, _ := s.Get("v1", "slack:C0000000001")

	p.posted = nil
	n.ReplyOnly = true
	res, err := n.Trigger(context.Background(), "q:fail", Data{Event: map[string]string{"stream": "a"}}, "v1")
	if err != nil || res.Posted != 1 || p.posted[0].ThreadTS != root {
		t.Fatalf("= %+v, %v, %+v; want one reply under %q", res, err, p.posted, root)
	}
}

// A notification outside threads never needed one, so reply-only leaves it be.
func TestReplyOnlyStillSendsThreadNone(t *testing.T) {
	p := &fakeProvider{}
	n := notifier(t, twoChannels, p, &MemoryState{})
	n.ReplyOnly = true
	res, err := n.Trigger(context.Background(), "q:loose", Data{}, "v1")
	if err != nil || res.Posted != 1 {
		t.Fatalf("= %+v, %v; want the unthreaded message sent", res, err)
	}
}
