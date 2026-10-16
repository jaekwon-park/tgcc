package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/config"
	"github.com/jaekwon-park/tgcc/internal/store"
)

const codexQueueLimit = 16
const codexJSONLimit = 4 * 1024 * 1024

type codexOutput interface {
	Enqueue(bot.OutgoingMsg)
}

type codexProgress interface {
	Ping(int64, int64)
	Heartbeat(int64, int64)
	Clear(int64, int64)
}

type codexMessage struct {
	text   string
	images []string
}

type codexWorker struct {
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	messages  []codexMessage
	topic     *store.Topic
	sessionID string
	drainHash [32]byte
}

type codexRunner struct {
	store      *store.Store
	bin        string
	sshBin     string
	env        map[string]string
	sender     codexOutput
	progress   codexProgress
	logger     *slog.Logger
	mu         sync.Mutex
	workers    map[string]*codexWorker
	closed     bool
	drain      bool
	retryDelay time.Duration
	loadMemory func(context.Context, *store.Session) string
}

func newCodexRunner(st *store.Store, bin string, env map[string]string, sender codexOutput, progress codexProgress, logger *slog.Logger) *codexRunner {
	return &codexRunner{store: st, bin: bin, sshBin: "ssh", env: env, sender: sender, progress: progress, logger: logger,
		workers: make(map[string]*codexWorker), retryDelay: 2 * time.Second}
}

func (runner *codexRunner) enqueue(ctx context.Context, sessionID string, message codexMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.closed {
		return fmt.Errorf("Codex 종료 중입니다")
	}
	sess, err := runner.store.SessionByID(sessionID)
	if err != nil {
		return err
	}
	if sess == nil || sess.ArchivedAt != 0 || sess.CLI != "codex" {
		return fmt.Errorf("Codex 세션이 없습니다")
	}
	if sess.Status != "active" && sess.Status != "idle" {
		return fmt.Errorf("Codex 세션 상태: %s; /resume 또는 /new를 사용하세요", sess.Status)
	}
	topic, err := runner.store.TopicByID(sess.TopicID)
	if err != nil {
		return err
	}
	if topic == nil || topic.CLI != sess.CLI {
		return fmt.Errorf("토픽 실행기가 변경되었습니다. /new를 사용하세요")
	}
	topic.WorkspacePath = sess.WorkspacePath
	worker := runner.workers[sessionID]
	if worker != nil {
		if worker.ctx.Err() != nil {
			return fmt.Errorf("Codex 종료 중입니다")
		}
		if message.text == "[queue-drain]" {
			return nil
		}
		if len(worker.messages) >= codexQueueLimit {
			return fmt.Errorf("Codex 대기열이 가득 찼습니다. 잠시 후 다시 보내세요")
		}
		worker.messages = append(worker.messages, message)
		return nil
	}
	if err := runner.store.UpdateSessionStatus(sess.ID, "active"); err != nil {
		return err
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	worker = &codexWorker{ctx: workerCtx, cancel: cancel, done: make(chan struct{}),
		messages: []codexMessage{message}, topic: topic, sessionID: sess.ID}
	runner.workers[sess.ID] = worker
	if runner.progress != nil {
		runner.progress.Ping(topic.ChatID, topic.ThreadID)
	}
	go runner.work(worker)
	return nil
}

func (runner *codexRunner) work(worker *codexWorker) {
	var failure error
	for {
		runner.mu.Lock()
		if worker.ctx.Err() != nil || failure != nil || len(worker.messages) == 0 {
			status := "idle"
			if worker.ctx.Err() != nil {
				status = "stopped"
				runner.notify(worker.topic, fmt.Sprintf("🛑 Codex 요청이 취소되었습니다. 대기 요청 %d개도 취소했습니다.", len(worker.messages)))
			} else if failure != nil {
				status = "failed"
				runner.logger.Error("codex turn failed", "session_id", worker.sessionID, "error", failure)
				runner.notify(worker.topic, fmt.Sprintf("❌ Codex 처리 실패: %v\n대기 요청 %d개를 취소했습니다. /resume 후 다시 보내세요.", failure, len(worker.messages)))
			}
			if err := runner.store.UpdateSessionStatus(worker.sessionID, status); err != nil {
				runner.logger.Error("codex status update failed", "error", err)
			}
			if err := runner.store.UpdateSessionCodexProcess(worker.sessionID, 0, ""); err != nil {
				runner.logger.Error("codex pid clear failed", "error", err)
			}
			if runner.progress != nil {
				runner.progress.Clear(worker.topic.ChatID, worker.topic.ThreadID)
			}
			delete(runner.workers, worker.sessionID)
			worker.cancel()
			close(worker.done)
			runner.mu.Unlock()
			return
		}
		message := worker.messages[0]
		worker.messages = worker.messages[1:]
		runner.mu.Unlock()
		if message.text == "[queue-drain]" {
			if data, err := os.ReadFile(filepath.Join(worker.topic.WorkspacePath, ".notify-queue")); err == nil {
				worker.drainHash = sha256.Sum256(data)
			}
		}
		failure = runner.turn(worker, message)
		if failure == nil && runner.drain {
			data, err := os.ReadFile(filepath.Join(worker.topic.WorkspacePath, ".notify-queue"))
			if err == nil && len(bytes.TrimSpace(data)) > 0 {
				hash := sha256.Sum256(data)
				runner.mu.Lock()
				if worker.ctx.Err() == nil && hash != worker.drainHash && len(worker.messages) == 0 {
					worker.drainHash = hash
					worker.messages = append(worker.messages, codexMessage{text: "[queue-drain]"})
				}
				runner.mu.Unlock()
			}
		}
	}
}

func (runner *codexRunner) turn(worker *codexWorker, message codexMessage) error {
	for attempt := 0; attempt < 4; attempt++ {
		sess, err := runner.store.SessionByID(worker.sessionID)
		if err != nil {
			return err
		}
		if sess == nil {
			return fmt.Errorf("session disappeared")
		}
		topic, err := runner.store.TopicByID(sess.TopicID)
		if err != nil {
			return err
		}
		if topic == nil || topic.CLI != "codex" {
			return fmt.Errorf("topic CLI changed")
		}
		model, err := config.ModelForCLI("codex", topic.ClaudeModel.String)
		if err != nil {
			return err
		}
		advanced, err := runner.execute(worker, sess, message, model)
		if err == nil {
			_, err = runner.store.DB.Exec("UPDATE sessions SET turn_count = turn_count + 1, last_activity_at = ? WHERE id = ?", store.CurrentTimeMs(), sess.ID)
			if err == nil {
				runner.logger.Info("codex turn completed", "session_id", sess.ID, "codex_thread_id", sess.CodexThreadID)
			}
			return err
		}
		if worker.ctx.Err() != nil {
			return worker.ctx.Err()
		}
		if advanced || !strings.Contains(err.Error(), "already has an active writer") || attempt == 3 {
			return err
		}
		select {
		case <-worker.ctx.Done():
			return worker.ctx.Err()
		case <-time.After(runner.retryDelay):
		}
	}
	return fmt.Errorf("Codex writer is busy")
}

func (runner *codexRunner) execute(worker *codexWorker, sess *store.Session, message codexMessage, model string) (bool, error) {
	args := []string{"exec"}
	if sess.CodexThreadID != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check", "--model", model)
	for _, path := range message.images {
		args = append(args, "--image", path)
	}
	if sess.CodexThreadID != "" {
		args = append(args, sess.CodexThreadID)
	}
	args = append(args, "-")
	env := mergedCodexEnv(runner.env)

	cmdCtx, cancel := context.WithCancel(worker.ctx)
	defer cancel()

	remoteHost := worker.topic.RemoteSSHHost
	var command *exec.Cmd
	if remoteHost != "" {
		command = runner.buildRemoteCodexCmd(cmdCtx, remoteHost, worker.topic.RemoteWorkspacePath, args)
	} else {
		bin, err := resolveCodexBin(runner.bin, env)
		if err != nil {
			return false, err
		}
		command = exec.CommandContext(cmdCtx, bin, args...)
		command.Dir = sess.WorkspacePath
	}
	command.Env = append(env, "TGCC_CODEX_SESSION_ID="+sess.ID)
	prompt := message.text
	if sess.CodexThreadID == "" && runner.loadMemory != nil {
		if memory := runner.loadMemory(worker.ctx, sess); memory != "" {
			prompt = memory + "\n\n[Current User Message]\n" + prompt
		}
	}
	command.Stdin = strings.NewReader(prompt)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 2 * time.Second
	var stderr limitedCodexBuffer
	parser := &codexJSONWriter{seen: make(map[string]bool), cancel: cancel}
	parser.event = func(event codexEvent) error {
		if runner.progress != nil {
			runner.progress.Heartbeat(worker.topic.ChatID, worker.topic.ThreadID)
		}
		switch event.Type {
		case "thread.started":
			if event.ThreadID == "" {
				return fmt.Errorf("missing Codex thread ID")
			}
			if sess.CodexThreadID != "" && sess.CodexThreadID != event.ThreadID {
				return fmt.Errorf("unexpected Codex thread ID")
			}
			sess.CodexThreadID = event.ThreadID
			return runner.store.UpdateSessionCodexThread(sess.ID, event.ThreadID)
		case "item.completed":
			if event.Item.Type == "agent_message" && event.Item.Text != "" {
				runner.notify(worker.topic, event.Item.Text)
			}
		}
		return nil
	}
	command.Stdout = parser
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		if remoteHost != "" {
			return false, fmt.Errorf("원격 호스트 %s 연결 실패: %w", remoteHost, err)
		}
		return false, err
	}
	started, _, err := codexProcessIdentity(int64(command.Process.Pid))
	if err != nil {
		cancel()
		_ = command.Wait()
		return false, fmt.Errorf("identify Codex process: %w", err)
	}
	if err := runner.store.UpdateSessionCodexProcess(sess.ID, int64(command.Process.Pid), started); err != nil {
		cancel()
		_ = command.Wait()
		return false, err
	}
	waitErr := command.Wait()
	parseErr := parser.finish()
	if parseErr != nil {
		return parser.advanced, parseErr
	}
	if waitErr != nil {
		if remoteHost != "" {
			return parser.advanced, fmt.Errorf("원격 호스트 %s 연결 실패: %w: %s", remoteHost, waitErr, strings.TrimSpace(stderr.String()))
		}
		return parser.advanced, fmt.Errorf("codex exit: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	if !parser.completed || sess.CodexThreadID == "" {
		if remoteHost != "" {
			return parser.advanced, fmt.Errorf("원격 Codex가 완료된 턴/스레드 없이 종료되었습니다")
		}
		return parser.advanced, fmt.Errorf("Codex exited without a completed turn/thread")
	}
	return parser.advanced, nil
}

// buildRemoteCodexCmd constructs an `ssh` command that runs `codex` on a remote
// host. The remote command line is passed as a single quoted argument so the
// remote shell (cmd.exe on Windows) receives it verbatim, and stdin is left
// connected to the SSH pipe so the prompt flows through unchanged.
func (runner *codexRunner) buildRemoteCodexCmd(ctx context.Context, host, workspace string, args []string) *exec.Cmd {
	remote := buildRemoteCommandLine(workspace, args)
	return exec.CommandContext(ctx, runner.sshBin, "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, remote)
}

// buildRemoteCommandLine renders the codex command to run on the remote host.
// args = ["exec"] or ["exec", "resume"], followed by flags, the optional
// thread id, and finally "-" (read prompt from stdin). `--cd <workspace>` is
// inserted right after the subcommand prefix so it stays grouped with the
// flags, before the positional prompt.
func buildRemoteCommandLine(workspace string, args []string) string {
	head := args[:1]
	if len(args) > 1 && args[1] == "resume" {
		head = args[:2]
	}
	remoteArgs := make([]string, 0, len(args)+2)
	remoteArgs = append(remoteArgs, head...)
	remoteArgs = append(remoteArgs, "--cd", workspace)
	remoteArgs = append(remoteArgs, args[len(head):]...)

	remote := "codex"
	for _, arg := range remoteArgs {
		remote += " " + cmdQuote(arg)
	}
	return remote
}

// cmdQuote quotes a single argument for cmd.exe. Plain tokens pass through
// unchanged; anything containing whitespace or cmd.exe metacharacters is
// wrapped in double quotes with embedded double quotes doubled. Backslashes
// are left alone — cmd.exe treats them as literal path separators, not escapes.
func cmdQuote(arg string) string {
	if arg == "" {
		return `""`
	}
	if !strings.ContainsAny(arg, " \t\"&|<>^()%!") {
		return arg
	}
	return `"` + strings.ReplaceAll(arg, `"`, `""`) + `"`
}

func (runner *codexRunner) stop(sessionID string) error {
	runner.mu.Lock()
	worker := runner.workers[sessionID]
	if worker != nil {
		worker.cancel()
		runner.mu.Unlock()
		<-worker.done
		return nil
	}
	defer runner.mu.Unlock()
	sess, err := runner.store.SessionByID(sessionID)
	if err != nil {
		return err
	}
	if sess == nil {
		return fmt.Errorf("Codex session not found")
	}
	if err := stopOrphanCodex(sess); err != nil {
		return err
	}
	if err := runner.store.UpdateSessionCodexProcess(sessionID, 0, ""); err != nil {
		return err
	}
	return runner.store.UpdateSessionStatus(sessionID, "stopped")
}

func (runner *codexRunner) close() {
	runner.mu.Lock()
	runner.closed = true
	var pending []chan struct{}
	for _, worker := range runner.workers {
		worker.cancel()
		pending = append(pending, worker.done)
	}
	runner.mu.Unlock()
	for _, done := range pending {
		<-done
	}
}

func (runner *codexRunner) notify(topic *store.Topic, text string) {
	if runner.sender == nil {
		return
	}
	for _, chunk := range splitCodexText(text) {
		runner.sender.Enqueue(bot.OutgoingMsg{ChatID: topic.ChatID, ThreadID: topic.ThreadID, Text: chunk})
	}
}

func splitCodexText(text string) []string {
	var chunks []string
	var chunk strings.Builder
	units := 0
	for _, character := range text {
		width := 1
		if character > 0xffff {
			width = 2
		}
		if units+width > 4000 {
			chunks = append(chunks, chunk.String())
			chunk.Reset()
			units = 0
		}
		chunk.WriteRune(character)
		units += width
	}
	if chunk.Len() > 0 {
		chunks = append(chunks, chunk.String())
	}
	return chunks
}

func mergedCodexEnv(overrides map[string]string) []string {
	env := os.Environ()
	for key, value := range overrides {
		prefix := key + "="
		filtered := env[:0]
		for _, entry := range env {
			if !strings.HasPrefix(entry, prefix) {
				filtered = append(filtered, entry)
			}
		}
		env = append(filtered, prefix+value)
	}
	return env
}

func resolveCodexBin(bin string, env []string) (string, error) {
	if strings.ContainsRune(bin, filepath.Separator) {
		return filepath.Abs(bin)
	}
	for _, entry := range env {
		if !strings.HasPrefix(entry, "PATH=") {
			continue
		}
		for _, directory := range filepath.SplitList(strings.TrimPrefix(entry, "PATH=")) {
			if !filepath.IsAbs(directory) {
				continue
			}
			path := filepath.Join(directory, bin)
			if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("Codex binary %q not found; configure [codex].bin", bin)
}

type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
	Item struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
}

type codexJSONWriter struct {
	buffer    []byte
	seen      map[string]bool
	completed bool
	advanced  bool
	err       error
	cancel    context.CancelFunc
	event     func(codexEvent) error
}

func (writer *codexJSONWriter) Write(data []byte) (int, error) {
	size := len(data)
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			end = len(data)
		}
		if len(writer.buffer)+end > codexJSONLimit {
			writer.err = fmt.Errorf("Codex JSONL line exceeds %d bytes", codexJSONLimit)
			writer.cancel()
			return 0, writer.err
		}
		writer.buffer = append(writer.buffer, data[:end]...)
		if end == len(data) {
			break
		}
		if err := writer.line(); err != nil {
			writer.cancel()
			return 0, err
		}
		data = data[end+1:]
	}
	return size, nil
}

func (writer *codexJSONWriter) line() error {
	data := bytes.TrimSpace(writer.buffer)
	if len(data) == 0 {
		writer.buffer = nil
		return nil
	}
	var event codexEvent
	if err := json.Unmarshal(data, &event); err != nil {
		writer.err = fmt.Errorf("invalid Codex JSONL: %w", err)
		return writer.err
	}
	writer.buffer = nil
	if event.Type == "" {
		writer.err = fmt.Errorf("Codex event missing type")
		return writer.err
	}
	switch event.Type {
	case "turn.started", "item.started", "item.updated", "item.completed":
		writer.advanced = true
	case "turn.completed":
		writer.completed = true
		writer.advanced = true
	case "turn.failed", "error":
		message := event.Error.Message
		if message == "" {
			message = event.Message
		}
		writer.err = fmt.Errorf("Codex %s: %s", event.Type, message)
		return writer.err
	}
	if event.Type == "item.completed" && event.Item.ID != "" {
		if writer.seen[event.Item.ID] {
			return nil
		}
		writer.seen[event.Item.ID] = true
	}
	if err := writer.event(event); err != nil {
		writer.err = err
		return err
	}
	return nil
}

func (writer *codexJSONWriter) finish() error {
	if writer.err != nil {
		return writer.err
	}
	if len(writer.buffer) != 0 {
		return writer.line()
	}
	return nil
}

type limitedCodexBuffer struct{ bytes.Buffer }

func (buffer *limitedCodexBuffer) Write(data []byte) (int, error) {
	size := len(data)
	remaining := 16*1024 - buffer.Len()
	if remaining > len(data) {
		remaining = len(data)
	}
	if remaining > 0 {
		_, _ = buffer.Buffer.Write(data[:remaining])
	}
	return size, nil
}
