package core

import (
	"strings"
	"testing"
)

func TestEngineSendToSessionWithButtons(t *testing.T) {
	p := &stubInlineButtonPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.interactiveStates["session-1"] = &interactiveState{platform: p, replyCtx: "ctx-1"}
	rows := [][]ButtonOption{{{Text: "喝了", Data: "cmd:/remind_act abc 0"}}}

	if err := e.SendToSessionWithOptions("session-1", "提醒", nil, nil, SendOptions{Buttons: rows}); err != nil {
		t.Fatalf("SendToSessionWithOptions returned error: %v", err)
	}
	if p.buttonContent != "提醒" || len(p.buttonRows) != 1 || p.buttonRows[0][0].Data != "cmd:/remind_act abc 0" {
		t.Fatalf("buttons not sent: content=%q rows=%#v", p.buttonContent, p.buttonRows)
	}
	if got := p.getSent(); len(got) != 0 {
		t.Fatalf("plain send should not be used, got %#v", got)
	}
}

func TestEngineSendButtonsFallsBackToText(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.interactiveStates["session-1"] = &interactiveState{platform: p, replyCtx: "ctx-1"}
	rows := [][]ButtonOption{{{Text: "OK", Data: "cmd:/ok"}}}

	if err := e.SendToSessionWithOptions("session-1", "提醒", nil, nil, SendOptions{Buttons: rows}); err != nil {
		t.Fatalf("SendToSessionWithOptions returned error: %v", err)
	}
	if got := p.getSent(); len(got) != 1 || got[0] != "提醒" {
		t.Fatalf("sent = %#v, want text fallback", got)
	}
}

func TestValidateSendButtons(t *testing.T) {
	ok := [][]ButtonOption{{{Text: "A", Data: "cmd:/a"}, {Text: "B", Data: "cmd:/b"}}}
	if err := validateSendButtons("msg", ok); err != nil {
		t.Fatalf("valid buttons rejected: %v", err)
	}
	if err := validateSendButtons("msg", nil); err != nil {
		t.Fatalf("no buttons rejected: %v", err)
	}
	cases := map[string]struct {
		message string
		rows    [][]ButtonOption
	}{
		"no message": {"", ok},
		"empty row":  {"msg", [][]ButtonOption{{}}},
		"no text":    {"msg", [][]ButtonOption{{{Text: " ", Data: "cmd:/a"}}}},
		"no data":    {"msg", [][]ButtonOption{{{Text: "A"}}}},
		"long data":  {"msg", [][]ButtonOption{{{Text: "A", Data: strings.Repeat("x", 65)}}}},
	}
	for name, tc := range cases {
		if err := validateSendButtons(tc.message, tc.rows); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
