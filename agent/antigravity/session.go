package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// antigravitySession manages multi-turn conversations with the Antigravity CLI (agy).
type antigravitySession struct {
	cmd       string
	extraArgs []string // extra args from cmd, prepended before agy args
	workDir   string
	model     string
	mode      string
	timeout   time.Duration
	extraEnv  []string
	events    chan core.Event
	stdin     io.WriteCloser
	stdinMu   sync.Mutex
	closeOnce sync.Once
	permReqID atomic.Value // stores string
	chatID    atomic.Value // stores string
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	alive     atomic.Bool
}

var permissionPromptPattern = regexp.MustCompile(`(?is)(allow|approve|permission).{0,400}(\(y/n\)|\(y\/n\)|\(y\/N\)|\(Y\/n\)|\[y\/n\]|\[y\/N\]|\[Y\/n\]|yes\/no)`)

const personalAICorrectedResponseMarker = "[[PERSONAL_AI_CORRECTED_RESPONSE]]"

type streamEnvelope struct {
	Event          string            `json:"event"`
	ConversationID string            `json:"conversation_id"`
	StepUpdate     *streamStepUpdate `json:"step_update,omitempty"`
	Result         *streamResult     `json:"result,omitempty"`
}

type streamStepUpdate struct {
	ConversationID string          `json:"conversation_id"`
	StepIndex      int             `json:"step_index"`
	State          string          `json:"state"`
	StepType       string          `json:"step_type"`
	TextDelta      string          `json:"text_delta"`
	ToolName       string          `json:"tool_name"`
	ToolInfo       *streamToolInfo `json:"tool_info,omitempty"`
	SubagentInfo   map[string]any  `json:"subagent_info,omitempty"`
}

type streamToolInfo struct {
	Name       string         `json:"name"`
	Parameters map[string]any `json:"parameters"`
	Output     any            `json:"output,omitempty"`
}

type streamResult struct {
	ConversationID string      `json:"conversation_id"`
	Status         string      `json:"status"`
	Response       string      `json:"response"`
	Usage          streamUsage `json:"usage"`
}

type streamUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

func newAntigravitySession(ctx context.Context, cmd string, extraArgs []string, workDir, model, mode, resumeID string, extraEnv []string, timeout time.Duration) (*antigravitySession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	as := &antigravitySession{
		cmd:       cmd,
		extraArgs: extraArgs,
		workDir:   workDir,
		model:     model,
		mode:      mode,
		timeout:   timeout,
		extraEnv:  extraEnv,
		events:    make(chan core.Event, 64),
		ctx:       sessionCtx,
		cancel:    cancel,
	}
	as.alive.Store(true)

	if resumeID != "" && resumeID != core.ContinueSession {
		as.chatID.Store(resumeID)
	}

	return as, nil
}

func (as *antigravitySession) Send(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if !as.alive.Load() {
		return fmt.Errorf("session is closed")
	}

	// Capture existing chat logs so we can identify a new session on first turn
	preEntries := make(map[string]bool)
	homeDir, err := os.UserHomeDir()
	if err == nil {
		slug := antigravityProjectSlug(as.workDir)
		chatsDir := filepath.Join(homeDir, ".gemini", "tmp", slug, "chats")
		if entries, err := os.ReadDir(chatsDir); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
					preEntries[entry.Name()] = true
				}
			}
		}
	}

	// Persist attachments across agent turns. A resumed Antigravity conversation
	// may decide to inspect an image only after the user follows up, so deleting
	// these files when the current CLI process exits makes valid prompt paths
	// immediately stale. core.SaveFilesToDisk also expires files older than the
	// shared seven-day retention window.
	imageRefs, fileRefs := saveAntigravityAttachments(as.workDir, messageID, images, files)

	chatID := as.CurrentSessionID()
	isResume := chatID != ""

	// Attach image and file references to prompt
	fullPrompt := appendAntigravityAttachmentRefs(prompt, imageRefs, fileRefs)
	args := as.buildAntigravityArgs(chatID, isResume, as.mode, as.model, fullPrompt)

	var ctx context.Context
	var cancel context.CancelFunc
	if as.timeout > 0 {
		ctx, cancel = context.WithTimeout(as.ctx, as.timeout)
	} else {
		ctx, cancel = context.WithCancel(as.ctx)
	}

	started := false
	defer func() {
		if !started {
			cancel()
		}
	}()

	slog.Debug("antigravitySession: launching", "resume", isResume, "args", core.RedactArgs(args))
	cmd := exec.CommandContext(ctx, as.cmd, args...)
	cmd.WaitDelay = 1 * time.Second
	cmd.Dir = as.workDir
	env := os.Environ()
	if len(as.extraEnv) > 0 {
		env = core.MergeEnv(env, as.extraEnv)
	}
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("antigravitySession: stdout pipe: %w", err)
	}
	var stdin io.WriteCloser
	if usesInteractivePermission(as.mode) {
		stdin, err = cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("antigravitySession: stdin pipe: %w", err)
		}
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("antigravitySession: start: %w", err)
	}
	as.stdinMu.Lock()
	as.stdin = stdin
	as.stdinMu.Unlock()

	started = true
	as.wg.Add(1)
	go func() {
		defer cancel()
		as.readLoop(ctx, cmd, stdout, &stderrBuf, preEntries, time.Now())
	}()

	return nil
}

func saveAntigravityAttachments(workDir, messageID string, images []core.ImageAttachment, files []core.FileAttachment) ([]string, []string) {
	stamp := time.Now().UnixMilli()
	imageFiles := make([]core.FileAttachment, 0, len(images))
	for i, img := range images {
		ext := ".png"
		switch img.MimeType {
		case "image/jpeg":
			ext = ".jpg"
		case "image/gif":
			ext = ".gif"
		case "image/webp":
			ext = ".webp"
		}
		imageFiles = append(imageFiles, core.FileAttachment{
			MimeType: img.MimeType,
			Data:     img.Data,
			FileName: fmt.Sprintf("img_%d_%d%s", stamp, i, ext),
		})
	}
	return core.SaveFilesToDisk(workDir, messageID, imageFiles), core.SaveFilesToDisk(workDir, messageID, files)
}

func appendAntigravityAttachmentRefs(prompt string, imageRefs, fileRefs []string) string {
	fullPrompt := prompt
	if len(imageRefs) > 0 {
		if fullPrompt == "" {
			fullPrompt = "Please analyze the attached image(s)."
		}
		fullPrompt += "\n\n[Attached images saved at: " + strings.Join(imageRefs, ", ") + "]"
		fullPrompt += "\n[Instruction: Inspect every attached image with view_file before responding; do not infer visual content from the filename or text alone.]"
	}
	if len(fileRefs) > 0 {
		if fullPrompt == "" {
			fullPrompt = "Please analyze the attached file(s)."
		}
		fullPrompt += "\n\n[Attached files saved at: " + strings.Join(fileRefs, ", ") + "]"
	}
	return fullPrompt
}

func (as *antigravitySession) buildAntigravityArgs(chatID string, isResume bool, mode, model, fullPrompt string) []string {
	// Prepend extra args from cmd so wrappers like "timeout 3600 agy" work.
	// Keep "-p <prompt>" at the very end because agy consumes the immediate next arg.
	args := append([]string{}, as.extraArgs...)
	if isResume {
		args = append(args, "--conversation", chatID)
	}
	switch mode {
	case "yolo":
		args = append(args, "--dangerously-skip-permissions")
	case "plan":
		args = append(args, "--sandbox")
	}
	if model = strings.TrimSpace(model); model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "--output-format", "stream-json", "-p", fullPrompt)
	return args
}

func usesInteractivePermission(mode string) bool {
	return strings.EqualFold(strings.TrimSpace(mode), "default")
}

func (as *antigravitySession) readLoop(ctx context.Context, cmd *exec.Cmd, stdout io.ReadCloser, stderrBuf *bytes.Buffer, preEntries map[string]bool, sendStartedAt time.Time) {
	defer as.wg.Done()

	go func() {
		<-ctx.Done()
		_ = stdout.Close()
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	startedTools := make(map[int]bool)
	var terminal *core.Event

	for scanner.Scan() {
		events, sid, err := parseStreamLine(scanner.Bytes(), startedTools)
		if err != nil {
			as.emit(core.Event{Type: core.EventError, Error: fmt.Errorf("parse stream-json: %w", err)})
			continue
		}
		if sid != "" && sid != as.CurrentSessionID() {
			as.chatID.Store(sid)
			as.emit(core.Event{Type: core.EventText, SessionID: sid})
		}
		for _, event := range events {
			if event.Type == core.EventResult {
				eventCopy := event
				terminal = &eventCopy
				continue
			}
			as.emit(event)
		}
	}

	scanErr := scanner.Err()
	waitErr := cmd.Wait()

	// AGY 1.1.8 emits the conversation ID in the init event. Keep the file
	// lookup only as a compatibility fallback for an incomplete stream.
	if as.CurrentSessionID() == "" {
		for attempt := 0; attempt < 15; attempt++ {
			if sid := as.detectNewSessionID(preEntries, sendStartedAt); sid != "" {
				as.chatID.Store(sid)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	if ctx.Err() == context.DeadlineExceeded {
		as.emit(core.Event{
			Type:  core.EventError,
			Error: fmt.Errorf("antigravity turn timed out after %v", as.timeout),
		})
		return
	}
	if ctx.Err() != nil {
		return
	}
	if scanErr != nil && !strings.Contains(scanErr.Error(), "file already closed") {
		as.emit(core.Event{Type: core.EventError, Error: fmt.Errorf("read stream-json: %w", scanErr)})
		return
	}
	if waitErr != nil {
		stderrMsg := strings.TrimSpace(stderrBuf.String())
		userErr := summarizeAntigravityProcessError(stderrMsg, waitErr, sendStartedAt)
		slog.Error("antigravitySession: process failed", "error", waitErr, "stderr", stderrMsg)
		as.emit(core.Event{Type: core.EventError, Error: fmt.Errorf("%s", userErr)})
		return
	}
	if terminal == nil {
		stderrMsg := strings.TrimSpace(stderrBuf.String())
		if stderrMsg != "" {
			slog.Warn("antigravitySession: stream ended without result", "stderr", stderrMsg)
		}
		as.emit(core.Event{Type: core.EventError, Error: fmt.Errorf("agy stream ended without terminal result")})
		return
	}

	terminal.SessionID = as.CurrentSessionID()
	as.emit(*terminal)
}

var antigravityBackendErrorPattern = regexp.MustCompile(`(?s)"code"\s*:\s*(\d{3}).{0,500}?"message"\s*:\s*"([^"]+)".{0,500}?"status"\s*:\s*"([A-Z_]+)"`)

// summarizeAntigravityProcessError keeps Telegram errors useful without
// forwarding AGY's full diagnostic log, which may contain local paths or other
// sensitive runtime details. AGY writes backend failures to its own CLI log and
// can leave stderr as only "exit status 1".
func summarizeAntigravityProcessError(stderr string, waitErr error, startedAt time.Time) string {
	stderr = strings.TrimSpace(stderr)
	if stderr != "" && stderr != waitErr.Error() {
		return stderr
	}

	if logText := latestAntigravityLogSince(startedAt); logText != "" {
		matches := antigravityBackendErrorPattern.FindAllStringSubmatch(logText, -1)
		if len(matches) > 0 {
			last := matches[len(matches)-1]
			return fmt.Sprintf("Google Antigravity backend error: %s %s — %s", last[1], last[3], last[2])
		}
	}

	if stderr != "" {
		return stderr
	}
	return waitErr.Error()
}

func latestAntigravityLogSince(startedAt time.Time) string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	logDir := filepath.Join(homeDir, ".gemini", "antigravity-cli", "log")
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return ""
	}

	var newest os.DirEntry
	var newestTime time.Time
	cutoff := startedAt.Add(-2 * time.Second)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "cli-") || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().Before(cutoff) || (!newestTime.IsZero() && !info.ModTime().After(newestTime)) {
			continue
		}
		newest = entry
		newestTime = info.ModTime()
	}
	if newest == nil {
		return ""
	}

	data, err := os.ReadFile(filepath.Join(logDir, newest.Name()))
	if err != nil {
		return ""
	}
	const maxLogTail = 1024 * 1024
	if len(data) > maxLogTail {
		data = data[len(data)-maxLogTail:]
	}
	return string(data)
}

func parseStreamLine(line []byte, startedTools map[int]bool) ([]core.Event, string, error) {
	var envelope streamEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nil, "", err
	}

	switch envelope.Event {
	case "init":
		return nil, envelope.ConversationID, nil
	case "step_update":
		if envelope.StepUpdate == nil {
			return nil, "", fmt.Errorf("step_update event missing payload")
		}
		update := envelope.StepUpdate
		sid := update.ConversationID
		if len(update.SubagentInfo) > 0 {
			return parseSubagentStep(update, startedTools), sid, nil
		}
		switch update.StepType {
		case "agent_response":
			if update.TextDelta == "" {
				return nil, sid, nil
			}
			return []core.Event{{
				Type:      core.EventText,
				Content:   update.TextDelta,
				SessionID: sid,
				Metadata: map[string]any{
					core.EventMetadataAuthoritativeResponsePending: true,
				},
			}}, sid, nil
		case "tool":
			return parseToolStep(update, startedTools), sid, nil
		case "thinking", "planning":
			if update.TextDelta == "" {
				return nil, sid, nil
			}
			return []core.Event{{Type: core.EventThinking, Content: update.TextDelta, SessionID: sid}}, sid, nil
		default:
			if update.TextDelta != "" {
				return []core.Event{{Type: core.EventThinking, Content: update.TextDelta, SessionID: sid}}, sid, nil
			}
			return nil, sid, nil
		}
	case "result":
		if envelope.Result == nil {
			return nil, "", fmt.Errorf("result event missing payload")
		}
		result := envelope.Result
		metadata := map[string]any{
			"status":                 result.Status,
			"thinking_tokens":        result.Usage.ThinkingTokens,
			"total_tokens":           result.Usage.TotalTokens,
			"authoritative_response": true,
		}
		event := core.Event{
			Type:                 core.EventResult,
			Content:              correctedAntigravityResponse(result.Response),
			SessionID:            result.ConversationID,
			Done:                 true,
			InputTokens:          result.Usage.InputTokens,
			OutputTokens:         result.Usage.OutputTokens,
			CacheReadInputTokens: result.Usage.CacheReadTokens,
			Metadata:             metadata,
		}
		return []core.Event{event}, result.ConversationID, nil
	default:
		return nil, envelope.ConversationID, nil
	}
}

func correctedAntigravityResponse(response string) string {
	idx := strings.LastIndex(response, personalAICorrectedResponseMarker)
	if idx < 0 {
		return response
	}
	return strings.TrimSpace(response[idx+len(personalAICorrectedResponseMarker):])
}

func parseSubagentStep(update *streamStepUpdate, startedTools map[int]bool) []core.Event {
	name := "subagent"
	for _, key := range []string{"name", "agent_name", "agent"} {
		if value, ok := update.SubagentInfo[key].(string); ok && strings.TrimSpace(value) != "" {
			name = value
			break
		}
	}

	var events []core.Event
	if !startedTools[update.StepIndex] {
		startedTools[update.StepIndex] = true
		events = append(events, core.Event{
			Type:         core.EventToolUse,
			ToolName:     name,
			ToolInput:    formatStreamValue(update.SubagentInfo),
			ToolInputRaw: update.SubagentInfo,
			SessionID:    update.ConversationID,
		})
	}
	if strings.EqualFold(update.State, "DONE") {
		events = append(events, core.Event{
			Type:       core.EventToolResult,
			ToolName:   name,
			ToolResult: formatStreamValue(update.SubagentInfo),
			ToolStatus: strings.ToLower(update.State),
			SessionID:  update.ConversationID,
		})
		delete(startedTools, update.StepIndex)
	}
	return events
}

func parseToolStep(update *streamStepUpdate, startedTools map[int]bool) []core.Event {
	if update.ToolInfo == nil {
		return nil
	}
	name := update.ToolInfo.Name
	if name == "" {
		name = update.ToolName
	}
	input := formatStreamValue(update.ToolInfo.Parameters)
	var events []core.Event
	if !startedTools[update.StepIndex] {
		startedTools[update.StepIndex] = true
		events = append(events, core.Event{
			Type:         core.EventToolUse,
			ToolName:     name,
			ToolInput:    input,
			ToolInputRaw: update.ToolInfo.Parameters,
			SessionID:    update.ConversationID,
		})
	}
	if strings.EqualFold(update.State, "DONE") {
		events = append(events, core.Event{
			Type:       core.EventToolResult,
			ToolName:   name,
			ToolResult: formatStreamValue(update.ToolInfo.Output),
			ToolStatus: strings.ToLower(update.State),
			SessionID:  update.ConversationID,
		})
		delete(startedTools, update.StepIndex)
	}
	return events
}

func formatStreamValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(encoded)
	}
}

func (as *antigravitySession) emit(event core.Event) {
	select {
	case as.events <- event:
	case <-as.ctx.Done():
	}
}

func (as *antigravitySession) detectNewSessionID(preEntries map[string]bool, sendStartedAt time.Time) string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	slug := antigravityProjectSlug(as.workDir)
	chatsDir := filepath.Join(homeDir, ".gemini", "tmp", slug, "chats")

	entries, err := os.ReadDir(chatsDir)
	if err != nil {
		return ""
	}

	type candidate struct {
		sessionID string
		modTime   time.Time
		diff      time.Duration
	}
	var candidates []candidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		if preEntries[entry.Name()] {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		mod := info.ModTime()
		if !sendStartedAt.IsZero() && mod.Before(sendStartedAt.Add(-2*time.Second)) {
			continue
		}

		fpath := filepath.Join(chatsDir, entry.Name())
		file, err := os.Open(fpath)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		if scanner.Scan() {
			var sf struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal([]byte(scanner.Text()), &sf) == nil && sf.SessionID != "" {
				diff := time.Duration(0)
				if !sendStartedAt.IsZero() {
					if mod.After(sendStartedAt) {
						diff = mod.Sub(sendStartedAt)
					} else {
						diff = sendStartedAt.Sub(mod)
					}
				}
				candidates = append(candidates, candidate{
					sessionID: sf.SessionID,
					modTime:   mod,
					diff:      diff,
				})
			}
		}
		_ = file.Close()
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].diff == candidates[j].diff {
			return candidates[i].modTime.After(candidates[j].modTime)
		}
		return candidates[i].diff < candidates[j].diff
	})
	return candidates[0].sessionID
}

func extractPermissionPrompt(text string) (string, bool) {
	loc := permissionPromptPattern.FindStringIndex(text)
	if loc == nil {
		return "", false
	}
	prompt := strings.TrimSpace(text[loc[0]:loc[1]])
	if prompt == "" {
		return "", false
	}
	return prompt, true
}

func (as *antigravitySession) RespondPermission(requestID string, result core.PermissionResult) error {
	if !as.alive.Load() {
		return fmt.Errorf("session is closed")
	}
	if pending, _ := as.permReqID.Load().(string); pending != "" && requestID != "" && requestID != pending {
		return fmt.Errorf("permission request mismatch: got %q, pending %q", requestID, pending)
	}
	as.stdinMu.Lock()
	defer as.stdinMu.Unlock()
	if as.stdin == nil {
		return fmt.Errorf("stdin is not available")
	}
	// agy permission prompts accept terminal-style responses.
	// Keep this conservative until agy exposes a structured permission protocol.
	reply := "y\n"
	if strings.EqualFold(result.Behavior, "deny") {
		reply = "n\n"
	}
	_, err := io.WriteString(as.stdin, reply)
	if err != nil {
		return fmt.Errorf("write permission response: %w", err)
	}
	as.permReqID.Store("")
	return nil
}

func (as *antigravitySession) Events() <-chan core.Event {
	return as.events
}

func (as *antigravitySession) CurrentSessionID() string {
	v, _ := as.chatID.Load().(string)
	return v
}

func (as *antigravitySession) Alive() bool {
	return as.alive.Load()
}

func (as *antigravitySession) Close() error {
	as.alive.Store(false)
	as.cancel()
	as.stdinMu.Lock()
	if as.stdin != nil {
		_ = as.stdin.Close()
		as.stdin = nil
	}
	as.stdinMu.Unlock()
	done := make(chan struct{})
	go func() {
		as.wg.Wait()
		as.closeOnce.Do(func() {
			close(as.events)
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		slog.Warn("antigravitySession: close timed out")
	}
	return nil
}
