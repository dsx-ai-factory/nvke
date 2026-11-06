// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// Data is what a template reads. Context is the run (see githubctx) and Event
// is what the caller passed for this event.
type Data struct {
	Context map[string]string
	Event   map[string]string
}

// Message is a notification rendered for sending.
type Message struct {
	Title    string
	Body     string
	Severity string
	Mentions []string
}

// slackEscaper writes the three characters Slack gives meaning to as the
// entities it documents, so a value cannot become a link or a mention.
var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// Render fills a notification's title and body from the data.
//
// Every value is escaped for Slack first, and the template text is not. The
// template is config a maintainer reviewed and writes links as <url|text> on
// purpose; the values come from the run, and a branch name or an error line
// that carries "<!channel>" must arrive as text.
func Render(n Notification, data Data) (Message, error) {
	escaped := Data{Context: escapeValues(data.Context), Event: escapeValues(data.Event)}

	title, err := execute(n.Name+" title", n.Title, escaped)
	if err != nil {
		return Message{}, err
	}
	body, err := execute(n.Name+" message_template", n.MessageTemplate, escaped)
	if err != nil {
		return Message{}, err
	}

	severity := n.Severity
	if severity == "" {
		severity = SeverityInfo
	}
	return Message{Title: strings.TrimSpace(title), Body: strings.TrimSpace(body), Severity: severity}, nil
}

func execute(name, text string, data Data) (string, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return out.String(), nil
}

func escapeValues(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = slackEscaper.Replace(v)
	}
	return out
}
