package core

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const antigravityRolloverTurnTimeout = 3 * time.Minute

func (e *Engine) antigravityRolloverTimeout() time.Duration {
	if e.eventIdleTimeout > 0 && e.eventIdleTimeout < antigravityRolloverTurnTimeout {
		return e.eventIdleTimeout
	}
	return antigravityRolloverTurnTimeout
}

const antigravityRolloverSummaryPromptFallback = `請為這個 Antigravity conversation 產生一份交接摘要，用來換到新的 session。

請包含：
- 目前正在處理的任務與目標。
- 已確認的使用者偏好、限制與禁忌。
- 最近重要決策與已完成修改。
- 尚未完成或需要注意的待辦。
- 重要路徑、session id、設定檔、備份檔、指令或狀態。
- 需要新 session 重新查證的假設、缺口或可能過期資訊。

要求：
- 使用繁體中文。
- 稱呼使用者為「主人」。
- 精簡但可執行。
- 嚴格區分已確認事實、你的推論、待查證事項；不要把推論寫成事實。
- 不要把角色扮演、比喻、玩笑或情緒化描述寫成客觀事實。
- 不要複製大量逐字對話，只保留下一個 session 需要行動的資訊。
- 不要加入寒暄。
- 不要改任何檔案。`

const antigravityRolloverHandoffPromptFallback = `這是 Antigravity rollover 交接。請先重新讀取權威檔案，再承接上一個 session 的摘要。

請依序讀取並遵守：
1. /Users/bluerain/personal-ai/AGENTS.md
2. /Users/bluerain/personal-ai/GEMINI.md
3. /Users/bluerain/personal-ai/agents/shared/index.md
4. /Users/bluerain/personal-ai/agents/shared/l1/user.md
5. /Users/bluerain/personal-ai/agents/shared/l1/runtime.md
6. /Users/bluerain/personal-ai/agents/shared/l1/current.md
7. /Users/bluerain/personal-ai/agents/shared/l1/device.md

承接規則：
- 使用繁體中文，稱呼使用者為「主人」。
- 回覆簡潔、直接、可執行。
- shared memory 與實際檔案/API/日誌優先於 handoff 摘要。
- handoff 摘要只能當延續上下文；遇到衝突、缺口或可能過期資訊，要重新查證。
- 精確區分「已實際修改」與「只是建議或計畫」。

<handoff_summary>
{{SUMMARY}}
</handoff_summary>

讀完後請只用一句繁體中文確認你已承接上下文，不要展開摘要內容。若必要檔案讀不到，請在同一句中點名缺哪一份。`

func (e *Engine) antigravityRolloverSummaryPrompt() string {
	return e.readAntigravityRolloverPrompt("summary-prompt.md", antigravityRolloverSummaryPromptFallback)
}

func (e *Engine) antigravityRolloverHandoffPrompt(summary string) string {
	template := e.readAntigravityRolloverPrompt("handoff-prompt.md", antigravityRolloverHandoffPromptFallback)
	summary = strings.TrimSpace(summary)
	if strings.Contains(template, "{{SUMMARY}}") {
		return strings.ReplaceAll(template, "{{SUMMARY}}", summary)
	}
	return strings.TrimSpace(template) + "\n\n<handoff_summary>\n" + summary + "\n</handoff_summary>"
}

func (e *Engine) readAntigravityRolloverPrompt(name, fallback string) string {
	if e != nil && strings.TrimSpace(e.baseWorkDir) != "" {
		path := filepath.Join(e.baseWorkDir, "agents", "gemini", "rollover", name)
		if data, err := os.ReadFile(path); err == nil {
			if prompt := strings.TrimSpace(string(data)); prompt != "" {
				return prompt
			}
		} else if !os.IsNotExist(err) {
			slog.Warn("antigravity rollover prompt read failed", "path", path, "error", err)
		}
	}
	return fallback
}

func (e *Engine) cmdAgyRollover(p Platform, msg *Message) {
	agent, sessions, interactiveKey, err := e.commandContext(p, msg)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
		return
	}
	if agent == nil || !strings.EqualFold(agent.Name(), "antigravity") {
		e.reply(p, msg.ReplyCtx, "這個指令只支援 Antigravity。")
		return
	}

	oldSession := sessions.GetOrCreateActive(msg.SessionKey)
	if !oldSession.TryLock() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPreviousProcessing))
		return
	}

	e.send(p, msg.ReplyCtx, "正在整理目前 Antigravity session，完成後會自動換到新 session。")
	go e.runAgyRollover(p, msg.ReplyCtx, msg.SessionKey, interactiveKey, agent, sessions, oldSession)
}

func (e *Engine) runAgyRollover(p Platform, replyCtx any, sessionKey, interactiveKey string, agent Agent, sessions *SessionManager, oldSession *Session) {
	oldUnlocked := false
	defer func() {
		if !oldUnlocked {
			oldSession.Unlock()
		}
	}()

	oldState := e.getOrCreateInteractiveStateWith(interactiveKey, p, replyCtx, oldSession, sessions, agent, sessionKey)
	if oldState == nil || oldState.agentSession == nil {
		e.reply(p, replyCtx, e.i18n.T(MsgFailedToStartAgentSession))
		return
	}
	oldReaderRestored := false
	defer func() {
		if oldReaderRestored || oldUnlocked {
			return
		}
		oldState.mu.Lock()
		alive := oldState.agentSession != nil && oldState.agentSession.Alive() && !oldState.stopped && !oldState.eventsNeedResync
		workspaceDir := oldState.workspaceDir
		oldState.mu.Unlock()
		if alive {
			e.startUnsolicitedReader(oldState, oldSession, sessions, interactiveKey, workspaceDir)
		}
		oldReaderRestored = true
	}()

	oldID := oldSession.GetAgentSessionID()
	steps := 0
	if reporter, ok := agent.(RolloverStatusReporter); ok && oldID != "" {
		ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
		if status, err := reporter.GetRolloverStatus(ctx, oldID); err == nil && status != nil {
			steps = status.Steps
		}
		cancel()
	}

	summary, err := e.collectSilentAgentTurn(oldState, oldSession, sessions, agent, e.antigravityRolloverSummaryPrompt())
	if err != nil {
		e.reply(p, replyCtx, fmt.Sprintf("換線失敗：整理舊 session 時出錯：%v", err))
		return
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		e.reply(p, replyCtx, "換線失敗：舊 session 沒有產生交接摘要。")
		return
	}

	e.cleanupInteractiveState(interactiveKey, oldState)
	oldSession.Unlock()
	oldUnlocked = true

	name := "rollover " + time.Now().Format("2006-01-02 15:04")
	newSession := sessions.NewSession(sessionKey, name)
	if !newSession.TryLock() {
		e.reply(p, replyCtx, e.i18n.T(MsgPreviousProcessing))
		return
	}
	defer newSession.Unlock()

	newState := e.getOrCreateInteractiveStateWith(interactiveKey, p, replyCtx, newSession, sessions, agent, sessionKey)
	if newState == nil || newState.agentSession == nil {
		e.reply(p, replyCtx, e.i18n.T(MsgFailedToStartAgentSession))
		return
	}

	handoffPrompt := e.antigravityRolloverHandoffPrompt(summary)
	ack, err := e.collectSilentAgentTurn(newState, newSession, sessions, agent, handoffPrompt)
	if err != nil {
		e.cleanupInteractiveState(interactiveKey, newState)
		e.reply(p, replyCtx, fmt.Sprintf("換線失敗：建立新 session 時出錯：%v", err))
		return
	}

	newSession.AddHistory("user", "[rollover handoff]\n"+summary)
	if strings.TrimSpace(ack) != "" {
		newSession.AddHistory("assistant", strings.TrimSpace(ack))
	}
	sessions.Save()

	newState.mu.Lock()
	alive := newState.agentSession != nil && newState.agentSession.Alive() && !newState.stopped && !newState.eventsNeedResync
	workspaceDir := newState.workspaceDir
	newState.mu.Unlock()
	if alive {
		e.startUnsolicitedReader(newState, newSession, sessions, interactiveKey, workspaceDir)
	}

	newID := newSession.GetAgentSessionID()
	if oldID == "" {
		oldID = "(unknown)"
	}
	if newID == "" {
		newID = "(pending)"
	}
	if steps > 0 {
		e.send(p, replyCtx, fmt.Sprintf("已整理並換到新的 Antigravity session。\n\n舊 session: `%s` (%d steps)\n新 session: `%s`", oldID, steps, newID))
	} else {
		e.send(p, replyCtx, fmt.Sprintf("已整理並換到新的 Antigravity session。\n\n舊 session: `%s`\n新 session: `%s`", oldID, newID))
	}
}

func (e *Engine) collectSilentAgentTurn(state *interactiveState, session *Session, sessions *SessionManager, agent Agent, prompt string) (string, error) {
	if state == nil || state.agentSession == nil {
		return "", fmt.Errorf("agent session is not available")
	}

	// A normal interactive turn leaves an unsolicited reader behind so that
	// background agent output can still reach the platform. A rollover turn is
	// intentionally silent and must be the only consumer of Events(); otherwise
	// the background reader can steal the summary/result and leave rollover
	// blocked until the global idle timeout.
	e.stopUnsolicitedReader(state)
	state.mu.Lock()
	needResync := state.eventsNeedResync
	state.eventsNeedResync = true
	state.mu.Unlock()
	if needResync {
		drainEvents(state.agentSession.Events())
	}

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- state.agentSession.Send(prompt, "", nil, nil)
	}()

	var textParts []string
	events := state.agentSession.Events()
	stopCh := state.stopSignal()
	pendingSend := sendDone

	turnTimeout := e.antigravityRolloverTimeout()
	turnTimer := time.NewTimer(turnTimeout)
	defer turnTimer.Stop()

	for {
		select {
		case <-stopCh:
			return "", fmt.Errorf("session stopped")
		case err := <-pendingSend:
			pendingSend = nil
			if err != nil {
				return "", err
			}
		case event, ok := <-events:
			if !ok {
				return "", fmt.Errorf("agent process exited")
			}
			switch event.Type {
			case EventText:
				if event.Content != "" {
					textParts = append(textParts, event.Content)
				}
				e.persistAgentSessionID(session, sessions, agent, event.SessionID)
			case EventResult:
				if !event.Done {
					continue
				}
				e.persistAgentSessionID(session, sessions, agent, event.SessionID)
				e.persistAgentSessionID(session, sessions, agent, state.agentSession.CurrentSessionID())
				result := strings.TrimSpace(event.Content)
				if result == "" && len(textParts) > 0 {
					result = strings.TrimSpace(strings.Join(textParts, ""))
				}
				state.mu.Lock()
				state.eventsNeedResync = false
				state.mu.Unlock()
				return result, nil
			case EventError:
				if event.Error != nil {
					return "", event.Error
				}
				return "", fmt.Errorf("agent error")
			case EventPermissionRequest:
				_ = state.agentSession.RespondPermission(event.RequestID, PermissionResult{
					Behavior:     "allow",
					UpdatedInput: event.ToolInputRaw,
				})
			}
		case <-turnTimer.C:
			return "", fmt.Errorf("agent session timed out after %s", turnTimeout)
		case <-e.ctx.Done():
			return "", e.ctx.Err()
		}
	}
}

func (e *Engine) persistAgentSessionID(session *Session, sessions *SessionManager, agent Agent, id string) {
	if id == "" || id == ContinueSession {
		return
	}
	if session.CompareAndSetAgentSessionID(id, agent.Name()) {
		pendingName := session.GetName()
		if pendingName != "" && pendingName != "session" && pendingName != "default" {
			sessions.SetSessionName(id, pendingName)
		}
		sessions.Save()
	}
}

func (e *Engine) maybeSendRolloverReminder(p Platform, replyCtx any, agent Agent, state *interactiveState) {
	if agent == nil || state == nil || state.agentSession == nil {
		return
	}
	reporter, ok := agent.(RolloverStatusReporter)
	if !ok {
		return
	}
	sid := strings.TrimSpace(state.agentSession.CurrentSessionID())
	if sid == "" {
		return
	}

	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	status, err := reporter.GetRolloverStatus(ctx, sid)
	cancel()
	if err != nil || status == nil {
		if err != nil {
			slog.Debug("rollover status unavailable", "agent", agent.Name(), "session_id", sid, "error", err)
		}
		return
	}

	level := 0
	switch {
	case status.UrgentAt > 0 && status.Steps >= status.UrgentAt:
		level = 3
	case status.RecommendAt > 0 && status.Steps >= status.RecommendAt:
		level = 2
	case status.WarnAt > 0 && status.Steps >= status.WarnAt:
		level = 1
	}
	if level == 0 {
		return
	}

	state.mu.Lock()
	if level <= state.lastRolloverReminder {
		state.mu.Unlock()
		return
	}
	state.lastRolloverReminder = level
	state.mu.Unlock()

	var msg string
	switch level {
	case 3:
		msg = fmt.Sprintf("這條 Antigravity session 已經 %d steps，強烈建議用 `/rollover_agy` 整理並換到新 session。", status.Steps)
	case 2:
		msg = fmt.Sprintf("這條 Antigravity session 已經 %d steps，建議用 `/rollover_agy` 整理並換到新 session。", status.Steps)
	default:
		msg = fmt.Sprintf("這條 Antigravity session 已經 %d steps，開始偏長了；需要時可以用 `/rollover_agy` 整理並換線。", status.Steps)
	}
	e.send(p, replyCtx, msg)
}
