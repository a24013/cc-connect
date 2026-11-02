package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type rolloverResultSession struct {
	events chan Event
	result string
	emit   bool
	once   sync.Once
}

func newRolloverResultSession(result string) *rolloverResultSession {
	return &rolloverResultSession{events: make(chan Event, 1), result: result, emit: true}
}

func (s *rolloverResultSession) Send(_ string, _ string, _ []ImageAttachment, _ []FileAttachment) error {
	if !s.emit {
		return nil
	}
	s.once.Do(func() {
		s.events <- Event{Type: EventResult, Content: s.result, SessionID: "rollover-result", Done: true}
	})
	return nil
}

func (s *rolloverResultSession) RespondPermission(_ string, _ PermissionResult) error { return nil }
func (s *rolloverResultSession) Events() <-chan Event                                 { return s.events }
func (s *rolloverResultSession) CurrentSessionID() string                             { return "rollover-result" }
func (s *rolloverResultSession) Alive() bool                                          { return true }
func (s *rolloverResultSession) Close() error                                         { return nil }

type rolloverTestAgent struct{ session AgentSession }

func (a *rolloverTestAgent) Name() string { return "antigravity" }
func (a *rolloverTestAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	return a.session, nil
}
func (a *rolloverTestAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *rolloverTestAgent) Stop() error { return nil }

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

func TestCollectSilentAgentTurnStopsUnsolicitedReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &stubPlatformEngine{n: "test"}
	sessions := NewSessionManager("")
	session := sessions.GetOrCreateActive("test:user")
	agentSession := newRolloverResultSession("交接摘要")
	agent := &rolloverTestAgent{session: agentSession}
	e := &Engine{
		ctx:               ctx,
		eventIdleTimeout:  time.Second,
		interactiveStates: map[string]*interactiveState{},
	}
	state := &interactiveState{
		agentSession:     agentSession,
		platform:         p,
		replyCtx:         "reply",
		agent:            agent,
		eventsNeedResync: false,
	}

	e.startUnsolicitedReader(state, session, sessions, "test:user", "")
	state.mu.Lock()
	readerDone := state.unsolicitedDone
	state.mu.Unlock()
	if readerDone == nil {
		t.Fatal("expected unsolicited reader to be running before rollover")
	}

	got, err := e.collectSilentAgentTurn(state, session, sessions, agent, "產生摘要")
	if err != nil {
		t.Fatalf("collectSilentAgentTurn returned error: %v", err)
	}
	if got != "交接摘要" {
		t.Fatalf("summary = %q, want %q", got, "交接摘要")
	}
	select {
	case <-readerDone:
	default:
		t.Fatal("unsolicited reader still running during silent rollover turn")
	}
	if sent := p.getSent(); len(sent) != 0 {
		t.Fatalf("silent rollover leaked output to platform: %v", sent)
	}
	state.mu.Lock()
	needResync := state.eventsNeedResync
	state.mu.Unlock()
	if needResync {
		t.Fatal("clean rollover result left eventsNeedResync=true")
	}
}

func TestCollectSilentAgentTurnCapsTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agentSession := &rolloverResultSession{events: make(chan Event)}
	agent := &rolloverTestAgent{session: agentSession}
	e := &Engine{ctx: ctx, eventIdleTimeout: 20 * time.Millisecond}
	state := &interactiveState{agentSession: agentSession, agent: agent}
	sessions := NewSessionManager("")
	session := sessions.GetOrCreateActive("test:user")

	started := time.Now()
	_, err := e.collectSilentAgentTurn(state, session, sessions, agent, "產生摘要")
	if err == nil || !strings.Contains(err.Error(), "timed out after 20ms") {
		t.Fatalf("timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("rollover timeout took too long: %v", elapsed)
	}
}

func TestAntigravityRolloverTimeoutNeverUsesGlobalTwoHourTimeout(t *testing.T) {
	e := &Engine{eventIdleTimeout: 2 * time.Hour}
	if got := e.antigravityRolloverTimeout(); got != 3*time.Minute {
		t.Fatalf("rollover timeout = %v, want 3m", got)
	}
}

func TestIsCronAgyRolloverPrompt(t *testing.T) {
	for _, prompt := range []string{"/rollover_agy", " /ROLLOVER_AGY\n", "/rollover"} {
		if !isCronAgyRolloverPrompt(prompt) {
			t.Fatalf("isCronAgyRolloverPrompt(%q) = false, want true", prompt)
		}
	}
	for _, prompt := range []string{"", "rollover_agy", "/rollover_agy now", "please /rollover_agy", "/help"} {
		if isCronAgyRolloverPrompt(prompt) {
			t.Fatalf("isCronAgyRolloverPrompt(%q) = true, want false", prompt)
		}
	}
}

func TestExecuteCronJob_RolloverPromptRunsBuiltinCommand(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore() error = %v", err)
	}
	platform := &stubCronReplyTargetPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "discord"},
	}
	agent := &resultAgent{session: newResultAgentSession("should not run as a prompt")}
	e := NewEngine("test", agent, []Platform{platform}, "", LangEnglish)
	defer e.cancel()
	e.cronScheduler = NewCronScheduler(store)

	job := &CronJob{
		ID:          "rollover-job",
		SessionKey:  "discord:channel-1:user-1",
		Prompt:      "/rollover_agy",
		Description: "Weekly rollover",
	}
	if err := store.Add(job); err != nil {
		t.Fatalf("store.Add() error = %v", err)
	}
	if err := e.ExecuteCronJob(job); err != nil {
		t.Fatalf("ExecuteCronJob() error = %v", err)
	}

	sent := platform.getSent()
	if len(sent) != 2 {
		t.Fatalf("sent messages = %v, want cron notice and rollover reply", sent)
	}
	if sent[1] != "這個指令只支援 Antigravity。" {
		t.Fatalf("sent[1] = %q, want the built-in rollover command reply", sent[1])
	}
}

func TestExecuteCronJob_RolloverAskPromptSendsChoice(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore() error = %v", err)
	}
	platform := &stubCronReplyTargetPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "discord"},
	}
	agent := &resultAgent{session: newResultAgentSession("should not run as a prompt")}
	e := NewEngine("test", agent, []Platform{platform}, "", LangEnglish)
	defer e.cancel()
	e.cronScheduler = NewCronScheduler(store)

	job := &CronJob{
		ID:          "rollover-ask",
		SessionKey:  "discord:channel-1:user-1",
		Prompt:      "/rollover_agy_ask",
		Description: "Ask about rollover",
	}
	if err := store.Add(job); err != nil {
		t.Fatalf("store.Add() error = %v", err)
	}
	if err := e.ExecuteCronJob(job); err != nil {
		t.Fatalf("ExecuteCronJob() error = %v", err)
	}

	sent := platform.getSent()
	if len(sent) != 2 {
		t.Fatalf("sent messages = %v, want cron notice and rollover choice", sent)
	}
	if !strings.Contains(sent[1], "/rollover_skip") || !strings.Contains(sent[1], "/rollover_keep") {
		t.Fatalf("sent[1] = %q, want skip and keep choices", sent[1])
	}
	if isCronAgyRolloverAskPrompt("/rollover_agy") || !isCronAgyRolloverAskPrompt(" /ROLLOVER_AGY_ASK ") {
		t.Fatal("isCronAgyRolloverAskPrompt matched the wrong prompts")
	}
}
