package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAntigravityRolloverSummaryPromptMarksUnverifiedContext(t *testing.T) {
	e := &Engine{}
	got := e.antigravityRolloverSummaryPrompt()

	for _, want := range []string{
		"需要新 session 重新查證",
		"嚴格區分已確認事實、你的推論、待查證事項",
		"不要把角色扮演、比喻、玩笑或情緒化描述寫成客觀事實",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary prompt missing %q", want)
		}
	}
}

func TestAntigravityRolloverHandoffPromptReloadsAuthorityFiles(t *testing.T) {
	e := &Engine{}
	got := e.antigravityRolloverHandoffPrompt("上一輪摘要")

	for _, want := range []string{
		"/Users/bluerain/personal-ai/AGENTS.md",
		"/Users/bluerain/personal-ai/GEMINI.md",
		"/Users/bluerain/personal-ai/agents/shared/l1/current.md",
		"handoff 摘要只能當延續上下文",
		"上一輪摘要",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("handoff prompt missing %q", want)
		}
	}
}

func TestAntigravityRolloverPromptsLoadFromFiles(t *testing.T) {
	dir := t.TempDir()
	promptDir := filepath.Join(dir, "agents", "gemini", "rollover")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "summary-prompt.md"), []byte("外部摘要 prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "handoff-prompt.md"), []byte("外部交接 {{SUMMARY}} prompt"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := &Engine{baseWorkDir: dir}

	if got := e.antigravityRolloverSummaryPrompt(); got != "外部摘要 prompt" {
		t.Fatalf("summary prompt = %q", got)
	}
	if got := e.antigravityRolloverHandoffPrompt("摘要內容"); got != "外部交接 摘要內容 prompt" {
		t.Fatalf("handoff prompt = %q", got)
	}
}
