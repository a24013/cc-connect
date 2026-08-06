package antigravity

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"cc-connect", "cc-connect"},
		{"Daily", "daily"},
		{"My Project", "my-project"},
		{"hello_world", "hello-world"},
		{"Test.123", "test-123"},
		{"---weird---", "weird"},
		{"", "project"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := slugify(tt.input)
			if got != tt.want {
				t.Errorf("slugify(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeMode(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"default", "default"},
		{"yolo", "yolo"},
		{"auto", "yolo"},
		{"force", "yolo"},
		{"plan", "plan"},
		{"sandbox", "plan"},
		{"invalid", "default"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeMode(tt.input)
			if got != tt.want {
				t.Errorf("normalizeMode(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSession_ContinueSessionTreatedAsFresh(t *testing.T) {
	s, err := newAntigravitySession(context.Background(), "echo", nil, "/tmp", "", "default", core.ContinueSession, nil, 0)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got := s.CurrentSessionID(); got != "" {
		t.Errorf("ContinueSession should be treated as fresh: chatID = %q, want empty", got)
	}
}

func TestBuildAntigravityArgs_PromptAtEnd(t *testing.T) {
	s, _ := newAntigravitySession(context.Background(), "echo", nil, "/tmp", "", "default", "", nil, 0)
	args := s.buildAntigravityArgs("sid-1", true, "plan", "gemini-3.1-pro-preview", "What is 1+1?")
	if len(args) < 2 {
		t.Fatalf("args too short: %v", args)
	}
	if args[len(args)-2] != "-p" || args[len(args)-1] != "What is 1+1?" {
		t.Fatalf("expected prompt to be final '-p <prompt>', got: %v", args)
	}
	if !contains(args, "--sandbox") {
		t.Fatalf("expected --sandbox in args, got: %v", args)
	}
	if !containsSequence(args, "--output-format", "stream-json") {
		t.Fatalf("expected stream-json output in args, got: %v", args)
	}
	if !containsSequence(args, "--model", "gemini-3.1-pro-preview") {
		t.Fatalf("expected model flag in args, got: %v", args)
	}
}

func TestAppendAntigravityAttachmentRefs(t *testing.T) {
	tests := []struct {
		name       string
		prompt     string
		imageRefs  []string
		wantPieces []string
	}{
		{
			name:       "single image without subject",
			imageRefs:  []string{"/work/.cc-connect/attachments/one.jpg"},
			wantPieces: []string{"Please analyze the attached image(s).", "one.jpg", "Inspect every attached image with view_file"},
		},
		{
			name:       "single image with subject",
			prompt:     "午餐，飲料是無糖豆漿",
			imageRefs:  []string{"/work/.cc-connect/attachments/one.jpg"},
			wantPieces: []string{"午餐，飲料是無糖豆漿", "one.jpg", "Inspect every attached image with view_file"},
		},
		{
			name:       "multiple images with subject",
			prompt:     "比較這兩張圖片",
			imageRefs:  []string{"/work/.cc-connect/attachments/one.jpg", "/work/.cc-connect/attachments/two.jpg"},
			wantPieces: []string{"比較這兩張圖片", "one.jpg", "two.jpg", "Inspect every attached image with view_file"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendAntigravityAttachmentRefs(tt.prompt, tt.imageRefs, nil)
			for _, piece := range tt.wantPieces {
				if !strings.Contains(got, piece) {
					t.Fatalf("prompt %q does not contain %q", got, piece)
				}
			}
		})
	}
}

func TestSession_AttachmentRemainsAvailableAfterTurn(t *testing.T) {
	workDir := t.TempDir()
	attachDir := filepath.Join(workDir, ".cc-connect", "attachments")
	if err := os.MkdirAll(attachDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	oldPath := filepath.Join(attachDir, "expired.jpg")
	if err := os.WriteFile(oldPath, []byte("old"), 0o600); err != nil {
		t.Fatalf("write expired attachment: %v", err)
	}
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("age expired attachment: %v", err)
	}

	scriptPath := filepath.Join(workDir, "fake-agy")
	argsPath := filepath.Join(workDir, "args.txt")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$AGY_ARGS_FILE\"\n" +
		"printf '%s\\n' '{\"event\":\"init\",\"conversation_id\":\"conv-image\",\"init\":{\"cwd\":\"/tmp\"}}'\n" +
		"printf '%s\\n' '{\"event\":\"result\",\"result\":{\"conversation_id\":\"conv-image\",\"status\":\"SUCCESS\",\"response\":\"done\",\"usage\":{}}}'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	t.Setenv("AGY_ARGS_FILE", argsPath)

	s, err := newAntigravitySession(context.Background(), scriptPath, nil, workDir, "", "yolo", "", nil, time.Minute)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Send("午餐", []core.ImageAttachment{{MimeType: "image/jpeg", Data: []byte("image-data")}}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	timeout := time.After(3 * time.Second)
	for {
		select {
		case event := <-s.Events():
			if event.Type == core.EventError {
				t.Fatalf("unexpected error: %v", event.Error)
			}
			if event.Type == core.EventResult {
				goto completed
			}
		case <-timeout:
			t.Fatal("timed out waiting for result")
		}
	}

completed:
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expired attachment still exists: %v", err)
	}
	entries, err := os.ReadDir(attachDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("attachment count = %d, want 1", len(entries))
	}
	imagePath := filepath.Join(attachDir, entries[0].Name())
	data, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatalf("attachment was deleted after turn: %v", err)
	}
	if string(data) != "image-data" {
		t.Fatalf("attachment data = %q, want image-data", data)
	}
	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	if !strings.Contains(string(argsData), "午餐") || !strings.Contains(string(argsData), imagePath) {
		t.Fatalf("agy args do not contain subject and image path: %q", argsData)
	}
}

func TestParseStreamLine_Init(t *testing.T) {
	events, sid, err := parseStreamLine([]byte(`{"event":"init","conversation_id":"conv-1","init":{"cwd":"/tmp"}}`), map[int]bool{})
	if err != nil {
		t.Fatalf("parseStreamLine: %v", err)
	}
	if sid != "conv-1" {
		t.Fatalf("session ID = %q, want conv-1", sid)
	}
	if len(events) != 0 {
		t.Fatalf("init events = %#v, want none", events)
	}
}

func TestParseStreamLine_AgentResponseDelta(t *testing.T) {
	line := `{"event":"step_update","step_update":{"conversation_id":"conv-1","step_index":3,"state":"ACTIVE","step_type":"agent_response","text_delta":"hello"}}`
	events, sid, err := parseStreamLine([]byte(line), map[int]bool{})
	if err != nil {
		t.Fatalf("parseStreamLine: %v", err)
	}
	if sid != "conv-1" || len(events) != 1 {
		t.Fatalf("sid=%q events=%#v", sid, events)
	}
	if events[0].Type != core.EventText || events[0].Content != "hello" {
		t.Fatalf("event = %#v, want EventText hello", events[0])
	}
}

func TestParseStreamLine_ToolLifecycle(t *testing.T) {
	started := map[int]bool{}
	active := `{"event":"step_update","step_update":{"conversation_id":"conv-1","step_index":4,"state":"ACTIVE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"pwd"}}}}`
	events, _, err := parseStreamLine([]byte(active), started)
	if err != nil {
		t.Fatalf("parse active tool: %v", err)
	}
	if len(events) != 1 || events[0].Type != core.EventToolUse {
		t.Fatalf("active events = %#v, want one EventToolUse", events)
	}
	if events[0].ToolInputRaw["CommandLine"] != "pwd" {
		t.Fatalf("tool input = %#v", events[0].ToolInputRaw)
	}

	done := `{"event":"step_update","step_update":{"conversation_id":"conv-1","step_index":4,"state":"DONE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"pwd"},"output":"/tmp\n"}}}`
	events, _, err = parseStreamLine([]byte(done), started)
	if err != nil {
		t.Fatalf("parse completed tool: %v", err)
	}
	if len(events) != 1 || events[0].Type != core.EventToolResult {
		t.Fatalf("done events = %#v, want one EventToolResult", events)
	}
	if events[0].ToolResult != "/tmp\n" {
		t.Fatalf("tool result = %q, want /tmp newline", events[0].ToolResult)
	}
}

func TestParseStreamLine_ToolDoneWithoutActive(t *testing.T) {
	line := `{"event":"step_update","step_update":{"conversation_id":"conv-1","step_index":4,"state":"DONE","step_type":"tool","tool_info":{"name":"read_file","parameters":{"path":"README.md"},"output":"ok"}}}`
	events, _, err := parseStreamLine([]byte(line), map[int]bool{})
	if err != nil {
		t.Fatalf("parseStreamLine: %v", err)
	}
	if len(events) != 2 || events[0].Type != core.EventToolUse || events[1].Type != core.EventToolResult {
		t.Fatalf("events = %#v, want use followed by result", events)
	}
}

func TestParseStreamLine_SubagentLifecycle(t *testing.T) {
	started := map[int]bool{}
	active := `{"event":"step_update","step_update":{"conversation_id":"conv-1","step_index":8,"state":"ACTIVE","step_type":"subagent","subagent_info":{"agent_name":"researcher","conversation_id":"child-1","log_uri":"file:///tmp/child.log"}}}`
	events, _, err := parseStreamLine([]byte(active), started)
	if err != nil {
		t.Fatalf("parse active subagent: %v", err)
	}
	if len(events) != 1 || events[0].Type != core.EventToolUse || events[0].ToolName != "researcher" {
		t.Fatalf("active events = %#v", events)
	}

	done := `{"event":"step_update","step_update":{"conversation_id":"conv-1","step_index":8,"state":"DONE","step_type":"subagent","subagent_info":{"agent_name":"researcher","conversation_id":"child-1","log_uri":"file:///tmp/child.log"}}}`
	events, _, err = parseStreamLine([]byte(done), started)
	if err != nil {
		t.Fatalf("parse completed subagent: %v", err)
	}
	if len(events) != 1 || events[0].Type != core.EventToolResult || !strings.Contains(events[0].ToolResult, "child-1") {
		t.Fatalf("done events = %#v", events)
	}
}

func TestParseStreamLine_UnknownTextStepSurfacesAsThinking(t *testing.T) {
	line := `{"event":"step_update","step_update":{"conversation_id":"conv-1","step_index":2,"state":"ACTIVE","step_type":"future_step","text_delta":"working"}}`
	events, _, err := parseStreamLine([]byte(line), map[int]bool{})
	if err != nil {
		t.Fatalf("parseStreamLine: %v", err)
	}
	if len(events) != 1 || events[0].Type != core.EventThinking || events[0].Content != "working" {
		t.Fatalf("events = %#v", events)
	}
}

func TestParseStreamLine_Result(t *testing.T) {
	line := `{"event":"result","result":{"conversation_id":"conv-1","status":"SUCCESS","response":"done\n","usage":{"input_tokens":18725,"output_tokens":284,"thinking_tokens":276,"cache_read_tokens":12000,"total_tokens":19009}}}`
	events, sid, err := parseStreamLine([]byte(line), map[int]bool{})
	if err != nil {
		t.Fatalf("parseStreamLine: %v", err)
	}
	if sid != "conv-1" || len(events) != 1 {
		t.Fatalf("sid=%q events=%#v", sid, events)
	}
	event := events[0]
	if event.Type != core.EventResult || !event.Done || event.Content != "done\n" {
		t.Fatalf("result event = %#v", event)
	}
	if event.InputTokens != 18725 || event.OutputTokens != 284 || event.CacheReadInputTokens != 12000 {
		t.Fatalf("usage not propagated: %#v", event)
	}
	if authoritative, _ := event.Metadata["authoritative_response"].(bool); !authoritative {
		t.Fatalf("result metadata = %#v, want authoritative_response=true", event.Metadata)
	}
}

func TestParseStreamLine_MalformedJSON(t *testing.T) {
	if _, _, err := parseStreamLine([]byte(`{"event":`), map[int]bool{}); err == nil {
		t.Fatal("expected malformed JSON error")
	}
}

func TestSession_SendParsesStreamJSONEndToEnd(t *testing.T) {
	workDir := t.TempDir()
	scriptPath := filepath.Join(workDir, "fake-agy")
	argsPath := filepath.Join(workDir, "args.txt")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$AGY_ARGS_FILE\"\n" +
		"printf '%s\\n' '{\"event\":\"init\",\"conversation_id\":\"conv-e2e\",\"init\":{\"cwd\":\"/tmp\"}}'\n" +
		"printf '%s\\n' '{\"event\":\"step_update\",\"step_update\":{\"conversation_id\":\"conv-e2e\",\"step_index\":3,\"state\":\"ACTIVE\",\"step_type\":\"agent_response\",\"text_delta\":\"hello\"}}'\n" +
		"printf '%s\\n' '{\"event\":\"result\",\"result\":{\"conversation_id\":\"conv-e2e\",\"status\":\"SUCCESS\",\"response\":\"hello\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"cache_read_tokens\":3,\"total_tokens\":12}}}'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	t.Setenv("AGY_ARGS_FILE", argsPath)

	s, err := newAntigravitySession(context.Background(), scriptPath, nil, workDir, "", "yolo", "", nil, time.Minute)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Send("test prompt", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var gotText, gotResult bool
	timeout := time.After(3 * time.Second)
	for !gotResult {
		select {
		case event := <-s.Events():
			switch event.Type {
			case core.EventText:
				if event.Content == "hello" {
					gotText = true
				}
			case core.EventError:
				t.Fatalf("unexpected error: %v", event.Error)
			case core.EventResult:
				gotResult = true
				if event.Content != "hello" || event.SessionID != "conv-e2e" {
					t.Fatalf("result = %#v", event)
				}
			}
		case <-timeout:
			t.Fatal("timed out waiting for result")
		}
	}
	if !gotText {
		t.Fatal("missing streamed EventText")
	}
	if got := s.CurrentSessionID(); got != "conv-e2e" {
		t.Fatalf("CurrentSessionID = %q, want conv-e2e", got)
	}
	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := strings.Fields(string(argsData))
	if !containsSequence(args, "--output-format", "stream-json") {
		t.Fatalf("args = %v, missing stream-json", args)
	}
}

func TestSession_TimeoutEmitsError(t *testing.T) {
	workDir := t.TempDir()
	scriptPath := filepath.Join(workDir, "fake-agy-timeout")
	script := "#!/bin/sh\n" +
		"sleep 5\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}

	s, err := newAntigravitySession(context.Background(), scriptPath, nil, workDir, "", "yolo", "", nil, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Send("test timeout", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case event := <-s.Events():
		if event.Type != core.EventError || event.Error == nil || !strings.Contains(event.Error.Error(), "timed out") {
			t.Fatalf("event = %#v, want timeout EventError", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for timeout EventError")
	}
}

func TestSession_ScheduleToolIsRejected(t *testing.T) {
	workDir := t.TempDir()
	scriptPath := filepath.Join(workDir, "fake-agy-schedule")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"event\":\"init\",\"conversation_id\":\"conv-schedule\"}'\n" +
		"printf '%s\\n' '{\"event\":\"step_update\",\"step_update\":{\"conversation_id\":\"conv-schedule\",\"step_index\":4,\"state\":\"ACTIVE\",\"step_type\":\"tool\",\"tool_name\":\"schedule\",\"tool_info\":{\"name\":\"schedule\",\"parameters\":{\"DurationSeconds\":\"3300\"}}}}'\n" +
		"sleep 5\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}

	s, err := newAntigravitySession(context.Background(), scriptPath, nil, workDir, "", "yolo", "", nil, time.Minute)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Send("test schedule", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-s.Events():
			if event.Type == core.EventError {
				if event.Error == nil || !strings.Contains(event.Error.Error(), "schedule tool is unsupported") {
					t.Fatalf("event = %#v, want schedule rejection", event)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for schedule rejection")
		}
	}
}

func TestUsesInteractivePermission(t *testing.T) {
	if !usesInteractivePermission("default") {
		t.Fatal("default mode should use interactive permission stdin")
	}
	if usesInteractivePermission("yolo") {
		t.Fatal("yolo mode should not use interactive permission stdin")
	}
	if usesInteractivePermission("plan") {
		t.Fatal("plan mode should not use interactive permission stdin")
	}
}

func TestRespondPermission_WritesTerminalAnswer(t *testing.T) {
	s, err := newAntigravitySession(context.Background(), "echo", nil, "/tmp", "", "default", "", nil, 0)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	s.stdin = w

	s.permReqID.Store("req")
	if err := s.RespondPermission("req", core.PermissionResult{Behavior: "allow"}); err != nil {
		t.Fatalf("RespondPermission allow: %v", err)
	}
	buf := make([]byte, 8)
	n, err := r.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read allow response: %v", err)
	}
	if got := string(buf[:n]); got != "y\n" {
		t.Fatalf("allow response = %q, want %q", got, "y\n")
	}

	s.permReqID.Store("req")
	if err := s.RespondPermission("req", core.PermissionResult{Behavior: "deny"}); err != nil {
		t.Fatalf("RespondPermission deny: %v", err)
	}
	n, err = r.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read deny response: %v", err)
	}
	if got := string(buf[:n]); got != "n\n" {
		t.Fatalf("deny response = %q, want %q", got, "n\n")
	}
}

func TestExtractPermissionPrompt(t *testing.T) {
	text := "Tool wants to run command. Allow this action? (y/N)"
	got, ok := extractPermissionPrompt(text)
	if !ok {
		t.Fatalf("expected permission prompt to be detected")
	}
	if got == "" {
		t.Fatalf("detected prompt should not be empty")
	}
}

func TestExtractPermissionPrompt_SplitChunksDetectedInWindow(t *testing.T) {
	part1 := "Tool wants to run command. Allow this"
	part2 := " action? (y/N)"
	got, ok := extractPermissionPrompt(part1 + part2)
	if !ok || got == "" {
		t.Fatalf("expected split prompt to be detected, got ok=%v prompt=%q", ok, got)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if strings.TrimSpace(x) == want {
			return true
		}
	}
	return false
}

func containsSequence(xs []string, want ...string) bool {
	for i := 0; i+len(want) <= len(xs); i++ {
		match := true
		for j := range want {
			if xs[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
