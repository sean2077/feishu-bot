// Package native implements Claude Code's persistent, bidirectional stream-json
// host protocol. Production does not load the TypeScript Agent SDK or Node.
// Protocol behavior is contract-tested against native Claude Code 2.1.316 (the
// version shipped by the previous pinned Agent SDK 0.3.316).
package native

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
)

const maxFrameBytes = 2 * 1024 * 1024

var modelName = regexp.MustCompile(`^[a-zA-Z0-9_.:/-]{1,200}$`)

type compactState struct {
	uuid string
	ran  bool
}
type worker struct {
	config                 core.Config
	project                core.Project
	session                core.Session
	workspace              core.Workspace
	log                    core.Logger
	secrets                []string
	ctx                    context.Context
	cancel                 context.CancelFunc
	cmd                    *exec.Cmd
	tree                   *processTree
	stdin                  io.WriteCloser
	stdout, stderr         *os.File
	writeMu                sync.Mutex
	mu                     sync.Mutex
	known                  map[string]bool
	compact                *compactState
	output                 *nativeOutput
	snapshot               contextSnapshot
	models                 []string
	modelSet               map[string]bool
	requests               map[string]chan args
	controls               map[string]context.CancelFunc
	controlSeen            map[string]bool
	controlOrder           []string
	pending                chan args
	initialized            chan struct{}
	initOnce               sync.Once
	initID                 string
	events                 chan core.WorkerEvent
	eventsDone             chan struct{}
	exited, done           chan struct{}
	stdoutDone, stderrDone chan struct{}
	cleanupOnce            sync.Once
	cleanupDone            chan struct{}
	cleanupErr             error
	stopOnce               sync.Once
	errorOnce              sync.Once
	settings               string
	relay                  *actionRelay
	started                time.Time
	exiting                bool
	readyReported          bool
}

// NewFactory creates one persistent native Claude process for each admitted
// conversation. Workspace policy and lifecycle admission remain in the host.
func NewFactory(c core.Config, log core.Logger) core.WorkerFactory {
	if log == nil {
		log = func(string, core.Args) {}
	}
	return func(ctx context.Context, room core.Room, session core.Session, workspace core.Workspace, onEvent func(core.WorkerEvent)) (core.Worker, error) {
		project, ok := c.Project(room.ProjectID)
		if !ok {
			return nil, fmt.Errorf("unknown conversation project")
		}
		token := os.Getenv(c.Model.TokenEnv)
		if token == "" {
			return nil, fmt.Errorf("missing model credential %s", c.Model.TokenEnv)
		}
		if c.Model.BaseURL == "" {
			return nil, fmt.Errorf("missing model endpoint")
		}
		headers, err := config.ModelHeaders(c)
		if err != nil {
			return nil, err
		}
		workerCtx, cancel := context.WithCancel(ctx)
		queueLimit := c.Limits.InboxPerRoom
		if queueLimit <= 0 {
			queueLimit = 128
		}
		if queueLimit > 128 {
			queueLimit = 128
		}
		w := &worker{config: c, project: project, session: session, workspace: workspace, log: log, secrets: config.Secrets(c), ctx: workerCtx, cancel: cancel, known: map[string]bool{}, modelSet: map[string]bool{}, requests: map[string]chan args{}, controls: map[string]context.CancelFunc{}, controlSeen: map[string]bool{}, pending: make(chan args, queueLimit), initialized: make(chan struct{}), events: make(chan core.WorkerEvent, 256), eventsDone: make(chan struct{}), exited: make(chan struct{}), done: make(chan struct{}), stdoutDone: make(chan struct{}), stderrDone: make(chan struct{}), cleanupDone: make(chan struct{}), started: time.Now(), initID: core.NewID()}
		w.output = newOutput(func(id string) bool { return w.known[id] })
		modelEnv := modelEnvironment(c, token, headers)
		w.settings, err = privateSettings(workspace, modelEnv)
		if err != nil {
			cancel()
			return nil, err
		}
		rollback := func() {
			cancel()
			if w.relay != nil {
				w.relay.close()
			}
			if w.settings != "" {
				_ = os.Remove(w.settings)
			}
		}
		env := Environment(workspace, c.AppSecretEnv)
		for key, value := range modelEnv {
			env[key] = value
		}
		delete(env, "ANTHROPIC_API_KEY")
		// The native CLI selects its programmatic entrypoint from this marker.
		env["CLAUDE_CODE_ENTRYPOINT"] = "sdk-go"
		if len(project.Actions) > 0 {
			w.relay, err = startActionRelay(workerCtx, project.Actions, func(ctx context.Context, name string, input args) args { return w.ask(ctx, "action", name, input) })
			if err != nil {
				rollback()
				return nil, err
			}
			client, err := os.Executable()
			if err != nil {
				rollback()
				return nil, err
			}
			env["FEISHU_ACTION_URL"] = w.relay.url
			env["FEISHU_ACTION_TOKEN"] = w.relay.token
			env["FEISHU_ACTION_CLIENT"] = client
			w.secrets = append(w.secrets, w.relay.token)
		}
		executable := c.Runtime.ClaudeExecutable
		if executable == "" {
			executable = "claude"
		}
		w.cmd = exec.Command(executable, launchArgs(c, project, session, workspace, w.settings)...)
		w.cmd.Dir = workspace.Work
		w.cmd.Env = environmentList(env)
		prepareProcess(w.cmd)
		w.stdin, err = w.cmd.StdinPipe()
		if err != nil {
			rollback()
			return nil, err
		}
		var stdoutWrite, stderrWrite *os.File
		w.stdout, stdoutWrite, err = os.Pipe()
		if err != nil {
			_ = w.stdin.Close()
			rollback()
			return nil, err
		}
		w.stderr, stderrWrite, err = os.Pipe()
		if err != nil {
			_ = stdoutWrite.Close()
			_ = w.stdout.Close()
			_ = w.stdin.Close()
			rollback()
			return nil, err
		}
		w.cmd.Stdout = stdoutWrite
		w.cmd.Stderr = stderrWrite
		if err = w.cmd.Start(); err != nil {
			_ = stdoutWrite.Close()
			_ = stderrWrite.Close()
			_ = w.stdout.Close()
			_ = w.stderr.Close()
			_ = w.stdin.Close()
			rollback()
			return nil, fmt.Errorf("start Claude native executable: %w", err)
		}
		_ = stdoutWrite.Close()
		_ = stderrWrite.Close()
		w.tree, err = attachProcess(w.cmd)
		if err != nil {
			_ = w.cmd.Process.Kill()
			_ = w.cmd.Wait()
			_ = w.stdout.Close()
			_ = w.stderr.Close()
			_ = w.stdin.Close()
			rollback()
			return nil, err
		}
		go func() {
			defer close(w.eventsDone)
			for event := range w.events {
				if onEvent != nil {
					onEvent(event)
				}
				if event.Type == "exit" {
					return
				}
			}
		}()
		go w.readFrames()
		go w.readStderr()
		go w.waitProcess()
		go w.pumpInputs()
		if err = w.write(args{"type": "control_request", "request_id": w.initID, "request": initialization(project, c.Limits.ApprovalMS)}); err != nil {
			w.fail("Claude initialization write failed", err)
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			_ = w.Stop(stopCtx)
			return nil, err
		}
		go func() {
			timer := time.NewTimer(30 * time.Second)
			defer timer.Stop()
			select {
			case <-w.initialized:
			case <-w.ctx.Done():
			case <-timer.C:
				w.fail("Claude native initialization timed out", nil)
			}
		}()
		go func() { <-workerCtx.Done(); w.beginStop() }()
		return w, nil
	}
}
func (w *worker) emit(event core.WorkerEvent) {
	select {
	case w.events <- event:
	case <-w.eventsDone:
	}
}
func (w *worker) write(value args) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxFrameBytes {
		return fmt.Errorf("native protocol frame too large")
	}
	data = append(data, '\n')
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if w.ctx.Err() != nil {
		return fmt.Errorf("native worker unavailable")
	}
	_, err = w.stdin.Write(data)
	return err
}
func (w *worker) Send(ctx context.Context, d core.Delivery) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.UUID == "" {
		return fmt.Errorf("native input UUID required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx.Err() != nil || w.exiting {
		return fmt.Errorf("native worker unavailable")
	}
	if d.SupplementTo != "" && !w.known[d.SupplementTo] {
		w.emit(core.WorkerEvent{Type: "input_rejected", UUID: d.UUID, Reason: "foreground_ended"})
		return nil
	}
	if w.known[d.UUID] {
		return fmt.Errorf("duplicate native input UUID")
	}
	if len(w.pending) == cap(w.pending) {
		return fmt.Errorf("native input queue full")
	}
	var content string
	command := d.Command != nil && d.Command.Name == "compact"
	if command {
		if w.compact != nil {
			return fmt.Errorf("compaction already pending")
		}
		content = "/compact"
		if d.Command.Instructions != "" {
			content += " " + d.Command.Instructions
		}
	} else {
		messageContent := func(d core.Delivery) args {
			return args{"trigger": d.Message, "background": d.Background, "quote": d.Quote, "attachments": d.Attachments}
		}
		envelope := args{"timezone": timezone()}
		if d.BatchInputs != nil {
			batch := make([]args, 0, len(d.BatchInputs))
			for _, input := range d.BatchInputs {
				item := messageContent(input)
				item["uuid"] = input.UUID
				batch = append(batch, item)
			}
			envelope["imageBatch"] = batch
		} else {
			for key, value := range messageContent(d) {
				envelope[key] = value
			}
		}
		encoded, err := json.Marshal(envelope)
		if err != nil {
			return err
		}
		content = string(encoded)
	}
	frame := args{"type": "user", "uuid": d.UUID, "message": args{"role": "user", "content": content}, "parent_tool_use_id": nil, "priority": "next", "origin": args{"kind": "human"}, "client_composed": true}
	if command {
		frame["priority"] = "later"
		delete(frame, "client_composed")
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(encoded) > maxFrameBytes {
		return fmt.Errorf("native input frame too large")
	}
	w.known[d.UUID] = true
	if command {
		w.compact = &compactState{uuid: d.UUID}
	}
	// Acknowledgement precedes the write, so even a very fast local command cannot
	// produce a terminal result before the host observes acceptance.
	w.emit(core.WorkerEvent{Type: "accepted", UUID: d.UUID})
	w.pending <- frame
	return nil
}
func timezone() string {
	if zone := os.Getenv("TZ"); zone != "" {
		if _, err := time.LoadLocation(zone); err == nil {
			return zone
		}
	}
	if name := time.Local.String(); name != "Local" && name != "" {
		return name
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if _, zone, ok := strings.Cut(link, "zoneinfo/"); ok {
			return zone
		}
	}
	if data, err := os.ReadFile("/etc/timezone"); err == nil && len(data) < 200 {
		if zone := strings.TrimSpace(string(data)); zone != "" {
			return zone
		}
	}
	return "UTC"
}
func (w *worker) pumpInputs() {
	select {
	case <-w.initialized:
	case <-w.ctx.Done():
		return
	}
	for {
		select {
		case <-w.ctx.Done():
			return
		case input := <-w.pending:
			if err := w.write(input); err != nil {
				w.fail("Claude input write failed", err)
				return
			}
		}
	}
}
func (w *worker) Respond(ctx context.Context, id string, value core.Args) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	ch := w.requests[id]
	if ch != nil {
		delete(w.requests, id)
	}
	w.mu.Unlock()
	if ch != nil {
		if value == nil {
			value = args{}
		}
		ch <- value
	}
	return nil
}
func (w *worker) ask(ctx context.Context, kind, name string, input args) args {
	if ctx.Err() != nil || w.ctx.Err() != nil {
		return args{"approved": false, "error": "cancelled"}
	}
	id := core.NewID()
	ch := make(chan args, 1)
	w.mu.Lock()
	if len(w.requests) >= 128 {
		w.mu.Unlock()
		return args{"approved": false, "error": "too many pending host requests"}
	}
	w.requests[id] = ch
	w.mu.Unlock()
	defer func() { w.mu.Lock(); delete(w.requests, id); w.mu.Unlock() }()
	w.emit(core.WorkerEvent{Type: "request", ID: id, Kind: kind, Name: name, Input: input})
	select {
	case answer := <-ch:
		return answer
	case <-ctx.Done():
		return args{"approved": false, "error": "cancelled"}
	case <-w.ctx.Done():
		return args{"approved": false, "error": "stopped"}
	}
}
func (w *worker) readFrames() {
	defer close(w.stdoutDone)
	defer w.stdout.Close()
	scanner := bufio.NewScanner(w.stdout)
	scanner.Buffer(make([]byte, 4096), maxFrameBytes+1)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		if len(line) > maxFrameBytes {
			w.fail("Claude protocol frame too large", nil)
			return
		}
		var event args
		if json.Unmarshal(line, &event) != nil || event == nil {
			w.fail("Claude protocol violation", nil)
			return
		}
		w.handleFrame(event)
	}
	if err := scanner.Err(); err != nil && w.ctx.Err() == nil {
		w.fail("Claude protocol read failed", err)
	}
}
func (w *worker) readStderr() {
	defer close(w.stderrDone)
	defer w.stderr.Close()
	reader := bufio.NewReaderSize(w.stderr, 8192)
	var line []byte
	oversized := false
	for {
		part, err := reader.ReadSlice('\n')
		if !oversized {
			if len(line)+len(part) > 65536 {
				oversized = true
				line = nil
			} else {
				line = append(line, part...)
			}
		}
		if err != bufio.ErrBufferFull {
			if oversized {
				w.log("worker_stderr", args{"text": "[oversized diagnostic line omitted]"})
			} else if len(line) > 0 {
				safe := core.Redact(string(line), w.secrets)
				if len(safe) > 4000 {
					safe = safe[:4000]
				}
				w.log("worker_stderr", args{"text": safe})
			}
			line = nil
			oversized = false
		}
		if err != nil && err != bufio.ErrBufferFull {
			return
		}
	}
}
func (w *worker) handleFrame(event args) {
	switch event["type"] {
	case "control_request":
		w.handleControl(event)
		return
	case "control_cancel_request":
		w.mu.Lock()
		cancel := w.controls[text(event["request_id"])]
		w.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	case "control_response":
		response := object(event["response"])
		if response["request_id"] != w.initID {
			return
		}
		if response["subtype"] != "success" {
			w.fail("Claude native initialization rejected", fmt.Errorf("%s", text(response["error"])))
			return
		}
		w.initOnce.Do(func() { close(w.initialized) })
		for _, pending := range array(response["pending_permission_requests"]) {
			frame := object(pending)
			if object(frame["request"])["subtype"] == "can_use_tool" {
				w.handleControl(frame)
			}
		}
		return
	}
	if w.ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	events, err := w.observe(event)
	w.mu.Unlock()
	if err != nil {
		w.fail(err.Error(), nil)
		return
	}
	for _, out := range events {
		w.emit(out)
	}
}
func (w *worker) handleControl(frame args) {
	id := text(frame["request_id"])
	request := object(frame["request"])
	if id == "" || request == nil {
		w.fail("invalid Claude control request", nil)
		return
	}
	w.mu.Lock()
	if w.controlSeen[id] {
		w.mu.Unlock()
		return
	}
	if len(w.controls) >= 128 {
		w.mu.Unlock()
		_ = w.write(args{"type": "control_response", "response": args{"subtype": "error", "request_id": id, "error": "too many pending control requests"}})
		return
	}
	ctx, cancel := context.WithCancel(w.ctx)
	w.controls[id] = cancel
	w.controlSeen[id] = true
	w.controlOrder = append(w.controlOrder, id)
	for len(w.controlOrder) > 10000 {
		delete(w.controlSeen, w.controlOrder[0])
		w.controlOrder = w.controlOrder[1:]
	}
	w.mu.Unlock()
	go func() {
		defer cancel()
		defer func() { w.mu.Lock(); delete(w.controls, id); w.mu.Unlock() }()
		result, err := w.control(ctx, request)
		response := args{"subtype": "success", "request_id": id, "response": result}
		if err != nil {
			response = args{"subtype": "error", "request_id": id, "error": err.Error()}
		}
		if w.ctx.Err() == nil {
			if err := w.write(args{"type": "control_response", "response": response}); err != nil {
				w.fail("Claude control response write failed", err)
			}
		}
	}()
}
func (w *worker) observe(event args) ([]core.WorkerEvent, error) {
	var events []core.WorkerEvent
	w.snapshot.observe(event)
	if event["type"] == "system" && event["subtype"] == "init" {
		if id := text(event["session_id"]); id != "" {
			if w.session.CCID != "" && id != w.session.CCID {
				return nil, fmt.Errorf("Native session ID changed during resume; refusing a silent reset")
			}
			events = append(events, core.WorkerEvent{Type: "ready", CCID: id})
			if !w.readyReported {
				w.readyReported = true
				w.log("worker_ready", args{"startupMs": time.Since(w.started).Milliseconds(), "resumed": w.session.CCID != ""})
			}
		}
	}
	if event["type"] == "system" && event["subtype"] == "compact_boundary" {
		trigger := "auto"
		if object(event["compact_metadata"])["trigger"] == "manual" {
			trigger = "manual"
			if w.compact != nil {
				w.compact.ran = true
			}
		}
		events = append(events, core.WorkerEvent{Type: "compaction", Phase: "complete", Trigger: trigger})
	}
	if !has(event["parent_tool_use_id"]) {
		var message args
		if event["type"] == "assistant" {
			message = object(event["message"])
		} else if event["type"] == "stream_event" {
			stream := object(event["event"])
			if stream["type"] == "message_start" {
				message = object(stream["message"])
			}
		}
		model := text(message["model"])
		if modelName.MatchString(model) && len(w.models) < 32 && !w.modelSet[model] {
			w.models = append(w.models, model)
			w.modelSet[model] = true
		}
	}
	events = append(events, w.output.handle(event)...)
	consumed := stringsOf(event["user_message_uuids"])
	if _, ok := event["user_message_uuids"].([]any); !ok {
		if id := text(event["user_message_uuid"]); id != "" {
			consumed = []string{id}
		}
	}
	for _, id := range consumed {
		if w.known[id] {
			events = append(events, core.WorkerEvent{Type: "consumed", UUID: id})
		}
	}
	if event["type"] == "system" {
		switch event["subtype"] {
		case "session_state_changed":
			state := text(event["state"])
			if state == "idle" || state == "running" || state == "requires_action" {
				events = append(events, core.WorkerEvent{Type: "activity", State: state})
			}
		case "background_tasks_changed":
			tasks := event["tasks"]
			if tasks == nil {
				tasks = event["background_tasks"]
			}
			n := -1
			if a, ok := tasks.([]any); ok {
				n = len(a)
			}
			events = append(events, core.WorkerEvent{Type: "activity", Background: &n})
		case "task_started", "task_notification", "task_progress":
			events = append(events, core.WorkerEvent{Type: "activity"})
		}
	}
	if event["type"] == "result" {
		isCompact := false
		if w.compact != nil {
			isCompact = event["local_command"] == "compact" || event["local_command"] == "/compact"
			for _, id := range consumed {
				isCompact = isCompact || id == w.compact.uuid
			}
		}
		success := event["subtype"] == "success" && event["is_error"] != true
		anchor := w.output.origin(event)
		source := w.output.eventSource(event)
		uuid, uuids := resultOrigin(event, w.known, anchor, source)
		if isCompact {
			uuid = w.compact.uuid
			uuids = []string{uuid}
		}
		result := core.WorkerEvent{Type: "result", Text: text(event["result"]), Success: success, UUID: uuid, UUIDs: uuids, ResultID: text(event["uuid"]), Models: append([]string(nil), w.models...), ReplyToUUID: anchor, Source: source, BackgroundID: w.output.background(event), Usage: object(event["usage"])}
		if modelName.MatchString(w.snapshot.model) {
			result.Model = w.snapshot.model
		}
		if isCompact {
			result.Compact = "failed"
			if success {
				result.Compact = "not_needed"
				if w.compact.ran {
					result.Compact = "compacted"
				}
			}
			w.compact = nil
		} else {
			result.Context = w.snapshot.result(event, w.config.Model.Model)
		}
		events = append(events, result)
		for _, id := range uuids {
			delete(w.known, id)
		}
		if uuid != "" {
			delete(w.known, uuid)
		}
		w.models = nil
		w.modelSet = map[string]bool{}
		w.output.end()
		w.snapshot = contextSnapshot{}
	}
	return events, nil
}
func (w *worker) fail(message string, err error) {
	w.errorOnce.Do(func() {
		if err != nil {
			w.log("worker_failure", args{"error": core.Redact(err.Error(), w.secrets)})
		}
		w.emit(core.WorkerEvent{Type: "error", Message: message})
		w.beginStop()
	})
}
func (w *worker) cleanup() {
	w.cleanupOnce.Do(func() {
		defer close(w.cleanupDone)
		if w.tree != nil {
			w.cleanupErr = w.tree.terminate(w.exited)
			w.tree.close()
		}
		if w.cleanupErr != nil {
			w.log("worker_descendant_cleanup_failed", args{"error": core.Redact(w.cleanupErr.Error(), w.secrets)})
		}
	})
	<-w.cleanupDone
}
func (w *worker) beginStop() {
	w.stopOnce.Do(func() {
		w.cancel()
		if w.relay != nil {
			w.relay.close()
		}
		_ = w.stdin.Close()
		go w.cleanup()
	})
}
func (w *worker) waitProcess() {
	err := w.cmd.Wait()
	close(w.exited)
	w.mu.Lock()
	w.exiting = true
	w.mu.Unlock()
	if w.relay != nil {
		w.relay.close()
	}
	w.cleanup()
	_ = w.stdin.Close()
	// The process tree has been terminated before the room can be reused. Readers
	// drain all remaining frames (including a final local command result).
	timer := time.NewTimer(2 * time.Second)
	select {
	case <-w.stdoutDone:
	case <-timer.C:
		_ = w.stdout.Close()
	}
	timer.Stop()
	<-w.stdoutDone
	timer = time.NewTimer(2 * time.Second)
	select {
	case <-w.stderrDone:
	case <-timer.C:
		_ = w.stderr.Close()
	}
	timer.Stop()
	<-w.stderrDone
	w.cancel()
	if w.settings != "" {
		if removeErr := os.Remove(w.settings); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			w.log("worker_settings_cleanup_failed", args{"error": removeErr.Error()})
		}
	}
	code := 0
	if w.cmd.ProcessState != nil {
		code = w.cmd.ProcessState.ExitCode()
	}
	event := core.WorkerEvent{Type: "exit", Code: &code}
	if w.cleanupErr != nil {
		confirmed := false
		event.CleanupConfirmed = &confirmed
	}
	if err != nil && code != 0 {
		w.log("worker_exit", args{"code": code})
	}
	w.emit(event)
	close(w.done)
}
func (w *worker) Stop(ctx context.Context) error {
	w.beginStop()
	select {
	case <-w.done:
		return w.cleanupErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
