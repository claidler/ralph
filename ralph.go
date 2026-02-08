package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	DefaultMaxLoops     = 50
	DefaultSuccessToken = "RALPH_DONE"
	ProgressFileName    = ".ralph-progress.md"

	// Logging previews (ONLY for the audit log + console; state files keep full text)
	MaxSignatureLen   = 140
	MaxPromptPreview  = 200
	MaxOutputPreview  = 300
	MaxFailCtxPreview = 600

	// Externalized state dir/files
	RalphDirName      = ".ralph"
	RalphTaskFile     = "TASK.md"
	RalphProgressFile = "PROGRESS.md"
	RalphErrorFile    = "ERROR.md"      // latest full error/output
	RalphContextFile  = "CONTEXT.md"    // lightweight git context
	RalphStateFile    = "STATE.md"      // durable environment state (NOT overwritten)
	RalphAttemptsDir  = "attempts"      // per-attempt full transcripts
	RalphAttemptExt   = ".log"          // attempt-0001.log, etc.
)

type TaskStatus int

const (
	TaskPending TaskStatus = iota
	TaskInProgress
	TaskCompleted
)

type Task struct {
	Index       int
	Description string
	Status      TaskStatus
	StartedAt   time.Time
	CompletedAt *time.Time
	Attempts    int
}

type AttemptRecord struct {
	AttemptNum    int
	Timestamp     time.Time
	Prompt        string
	PromptHash    string
	ExitCode      int
	Duration      time.Duration
	ErrorSig      string
	ErrorCtx      string // full combined output (stderr preferred, includes stdout if stderr empty)
	MutationNotes string
}

type AgentConfig struct {
	Name        string
	Args        []string
	NeedsPrompt bool
}

var agentConfigs = map[string]AgentConfig{
	"claude":  {Name: "claude", Args: []string{"-p", "--dangerously-skip-permissions"}, NeedsPrompt: true},
	"codex":   {Name: "codex", Args: []string{"exec", "--full-auto", "--skip-git-repo-check"}, NeedsPrompt: false},
	"gemini":  {Name: "gemini", Args: []string{}, NeedsPrompt: true},
	"copilot": {Name: "copilot", Args: []string{}, NeedsPrompt: false}, // Special handling in execute()
}

type RalphSession struct {
	File            *os.File
	ToolName        string
	SuccessToken    string
	MaxLoops        int
	StartTime       time.Time
	GlobalIteration int
	KeepLog         bool

	RalphDir     string
	LastErrorCtx string

	FinishReason string
	FinishNote   string
	FinishTime   time.Time

	Tasks          []Task
	CurrentTaskIdx int

	CurrentAttempts []AttemptRecord
	FailureCount    map[string]int
	LastFailure     string
	RepeatStreak    int
	LastPrompt      string
}

func main() {
	args := os.Args[1:]
	maxLoops := DefaultMaxLoops
	keepLog := true
	successToken := DefaultSuccessToken

	i := 0
	for i < len(args) {
		switch args[i] {
		case "--max", "-m":
			if i+1 >= len(args) {
				printUsage()
				os.Exit(1)
			}
			val, err := strconv.Atoi(args[i+1])
			if err == nil && val > 0 {
				maxLoops = val
			}
			i += 2
		case "--rm", "--cleanup":
			keepLog = false
			i++
		default:
			goto DoneParsing
		}
	}
DoneParsing:
	args = args[i:]

	if len(args) < 1 {
		printUsage()
		os.Exit(1)
	}

	toolName := args[0]

	var taskInput string
	if len(args) >= 2 {
		taskInput = strings.Join(args[1:], "\n")
	} else {
		info, err := os.Stdin.Stat()
		if err == nil && info.Mode()&os.ModeCharDevice == 0 {
			data, err := io.ReadAll(os.Stdin)
			if err == nil {
				taskInput = string(data)
			}
		}
	}

	if strings.TrimSpace(taskInput) == "" {
		fmt.Fprintf(os.Stderr, "💥 No tasks provided\n")
		os.Exit(1)
	}

	tasks := parseTasks(taskInput)
	if len(tasks) == 0 {
		fmt.Fprintf(os.Stderr, "💥 No tasks found\n")
		os.Exit(1)
	}

	session, err := newRalphSession(toolName, successToken, maxLoops, keepLog, tasks)
	if err != nil {
		fmt.Fprintf(os.Stderr, "💥 Ralph couldn't start: %v\n", err)
		os.Exit(1)
	}
	defer session.close()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		session.logFinish("interrupted", "User interrupted")
		session.File.WriteString("\n\n---\n**Interrupted**\n")
		fmt.Printf("\n🛑 Interrupted! Check %s\n", ProgressFileName)
		session.close()
		os.Exit(130)
	}()

	fmt.Printf("🌀 Ralph v4.4 - File-State Ralph Loop (durable STATE.md + full error transcripts)\n")
	fmt.Printf("📋 Tasks: %d\n", len(session.Tasks))
	session.printTaskList()
	fmt.Printf("📓 Log: %s\n", session.File.Name())
	fmt.Printf("🎯 Agent: %s\n", toolName)
	fmt.Printf("🔧 Max Loops: %d\n", maxLoops)
	fmt.Printf("🗂  State dir: %s/\n", RalphDirName)
	fmt.Println(strings.Repeat("=", 60))

	for session.GlobalIteration = 1; session.GlobalIteration <= maxLoops; session.GlobalIteration++ {
		if session.CurrentTaskIdx >= len(session.Tasks) {
			break
		}

		currentTask := &session.Tasks[session.CurrentTaskIdx]
		if currentTask.Status == TaskPending && currentTask.Attempts == 0 {
			currentTask.StartedAt = time.Now()
			currentTask.Status = TaskInProgress
		}

		currentTask.Attempts++
		attemptNum := currentTask.Attempts

		// Externalize state at the *start* of each attempt.
		// NOTE: STATE.md is durable (we create it if missing, but do not overwrite).
		session.writeRalphFiles()

		// Keep prompt stable; the agent reads/writes files in .ralph/*.
		prompt := session.buildStablePrompt()
		mutationNotes := session.calculateMutation()

		session.logIterationStart(attemptNum, prompt, mutationNotes)

		fmt.Printf("\n🔄 [Global %d/%d] Task %d/%d (Attempt %d): %s\n",
			session.GlobalIteration, maxLoops,
			session.CurrentTaskIdx+1, len(session.Tasks),
			attemptNum, currentTask.Description)

		if mutationNotes != "" {
			fmt.Printf("🧬 Mutation: %s\n", mutationNotes)
		}

		stdout, stderr, exitCode, duration := session.execute(prompt)
		success := exitCode == 0 && session.hasSuccessToken(stdout)

		// Always write a full transcript file per attempt (helps “what was tried” without truncation).
		_ = session.writeAttemptTranscript(attemptNum, prompt, stdout, stderr, exitCode, duration)

		if success {
			now := time.Now()
			currentTask.Status = TaskCompleted
			currentTask.CompletedAt = &now

			session.logSuccess(duration, stdout)
			fmt.Printf("\n✅ Task %d complete after %d attempt(s) (%s)\n",
				session.CurrentTaskIdx+1, attemptNum, duration)

			session.logMutationSummary()

			// Reset per-task evolution
			session.CurrentAttempts = nil
			session.FailureCount = make(map[string]int)
			session.LastFailure = ""
			session.LastErrorCtx = ""
			session.RepeatStreak = 0
			session.LastPrompt = ""

			session.CurrentTaskIdx++
			if session.CurrentTaskIdx < len(session.Tasks) {
				fmt.Printf("📝 Moving to task %d...\n", session.CurrentTaskIdx+1)
				time.Sleep(1 * time.Second)
			}
			continue
		}

		// Full error context for the agent (NO truncation in ERROR.md or attempt logs).
		errorContext := session.extractFailureFull(stderr, stdout)
		if exitCode == 0 {
			errorContext = session.tokenMissingContext(stdout, errorContext)
		}
		session.LastErrorCtx = errorContext

		// Update ERROR.md immediately (so next loop reads the full last output).
		session.writeLatestErrorFile()

		sig := failureSignature(errorContext)
		session.recordAttempt(attemptNum, prompt, exitCode, duration, sig, errorContext, mutationNotes)

		fmt.Printf("\n❌ Failed (exit %d) after %s\n", exitCode, duration)
		fmt.Printf("🧩 Signature: %s\n", sig)

		if session.RepeatStreak >= 2 {
			fmt.Printf("⚠️ Same error %d times in a row\n", session.RepeatStreak)
		}

		session.logFailure(attemptNum, duration, exitCode, errorContext)

		if session.GlobalIteration < maxLoops {
			fmt.Printf("🔄 Throwing back on the wheel...\n")
			time.Sleep(1 * time.Second)
		}
	}

	completed := 0
	for _, t := range session.Tasks {
		if t.Status == TaskCompleted {
			completed++
		}
	}

	if session.CurrentTaskIdx >= len(session.Tasks) {
		session.logFinish("complete", fmt.Sprintf("All %d tasks completed", len(session.Tasks)))
		fmt.Printf("\n✅ Pipeline complete!\n")
	} else {
		session.logFinish("exhausted", fmt.Sprintf("Completed %d/%d", completed, len(session.Tasks)))
		fmt.Printf("\n🛑 Exhausted. Completed %d/%d tasks\n", completed, len(session.Tasks))
	}

	if !keepLog {
		fmt.Println("🧹 Cleaning up...")
	} else {
		fmt.Printf("📋 Audit log: %s\n", ProgressFileName)
		fmt.Printf("🗂  State files: %s/%s, %s/%s, %s/%s, %s/%s, %s/%s\n",
			RalphDirName, RalphTaskFile,
			RalphDirName, RalphProgressFile,
			RalphDirName, RalphStateFile,
			RalphDirName, RalphErrorFile,
			RalphDirName, RalphContextFile,
		)
		fmt.Printf("🧾 Full transcripts: %s/%s/\n", RalphDirName, RalphAttemptsDir)
	}

	if completed < len(session.Tasks) {
		os.Exit(1)
	}
}

func parseTasks(input string) []Task {
	var tasks []Task
	lines := strings.Split(input, "\n")

	checkboxRe := regexp.MustCompile(`^\s*[-*]\s*\[([ xX])\]\s*(.+)$`)
	numberedRe := regexp.MustCompile(`^\s*\d+[.)]\s*(.+)$`)
	plainRe := regexp.MustCompile(`^\s*[-*]\s*(.+)$`)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var desc string
		status := TaskPending

		if matches := checkboxRe.FindStringSubmatch(line); matches != nil {
			desc = strings.TrimSpace(matches[2])
			if strings.ToLower(matches[1]) == "x" {
				status = TaskCompleted
			}
		} else if matches := numberedRe.FindStringSubmatch(line); matches != nil {
			desc = strings.TrimSpace(matches[1])
		} else if matches := plainRe.FindStringSubmatch(line); matches != nil {
			desc = strings.TrimSpace(matches[1])
		} else {
			desc = line
		}

		if desc != "" {
			tasks = append(tasks, Task{
				Index:       len(tasks),
				Description: desc,
				Status:      status,
			})
		}
	}

	return tasks
}

func (s *RalphSession) buildStablePrompt() string {
	dir := s.RalphDir
	return strings.TrimSpace(fmt.Sprintf(`
You are running inside a non-interactive loop.

Read and use these files (filesystem is your memory):
- %s/%s: current task and rules (one task only)
- %s/%s: durable environment state (browser/servers/ports/env vars/etc) — KEEP THIS UPDATED
- %s/%s: attempt history and plan (UPDATE this as you work)
- %s/%s: last error/output (full) — read first on retries
- %s/%s: lightweight repo context (git status + diff stat)

Instructions:
1) Read %s/%s and do ONLY that task.
2) Read %s/%s first for anything outside git (URLs opened, servers running, ports, etc).
3) Use %s/%s + %s/%s to understand what failed previously.
4) Make minimal changes to succeed; run appropriate commands/tests.
5) Update %s/%s with: Observed / Hypothesis / Change / Result / Next step.
6) Update %s/%s when you touch environment state (start/stop server, open URL, set env var, etc).
7) When done and verified, print EXACTLY: %s
`,
		dir, RalphTaskFile,
		dir, RalphStateFile,
		dir, RalphProgressFile,
		dir, RalphErrorFile,
		dir, RalphContextFile,
		dir, RalphTaskFile,
		dir, RalphStateFile,
		dir, RalphProgressFile, dir, RalphErrorFile,
		dir, RalphProgressFile,
		dir, RalphStateFile,
		s.SuccessToken,
	))
}

func (s *RalphSession) calculateMutation() string {
	if len(s.CurrentAttempts) == 0 {
		return "Baseline (fresh task)"
	}
	last := s.CurrentAttempts[len(s.CurrentAttempts)-1].ErrorSig
	if s.RepeatStreak >= 2 {
		return fmt.Sprintf("Retry (same error x%d): %s", s.RepeatStreak, last)
	}
	return fmt.Sprintf("Retry: %s", last)
}

func (s *RalphSession) recordAttempt(attemptNum int, prompt string, exitCode int, duration time.Duration, sig, ctx, mutation string) {
	sum := sha256.Sum256([]byte(prompt))
	hash := hex.EncodeToString(sum[:])[:12]

	rec := AttemptRecord{
		AttemptNum:    attemptNum,
		Timestamp:     time.Now(),
		Prompt:        prompt,
		PromptHash:    hash,
		ExitCode:      exitCode,
		Duration:      duration,
		ErrorSig:      sig,
		ErrorCtx:      ctx, // FULL
		MutationNotes: mutation,
	}
	s.CurrentAttempts = append(s.CurrentAttempts, rec)
	s.LastPrompt = prompt

	if s.FailureCount == nil {
		s.FailureCount = make(map[string]int)
	}
	s.FailureCount[sig]++

	if s.LastFailure == sig {
		s.RepeatStreak++
	} else {
		s.LastFailure = sig
		s.RepeatStreak = 1
	}
}

func (s *RalphSession) captureProjectState() string {
	var b strings.Builder

	cmd := exec.Command("git", "status", "--short")
	out, _ := cmd.Output()
	if len(out) > 0 {
		b.WriteString("Git status:\n")
		b.Write(out)
	}

	cmd = exec.Command("git", "diff", "--stat")
	out, _ = cmd.Output()
	if len(out) > 0 {
		stat := string(out)
		// Keep lightweight for context file, not huge diffs
		if len(stat) > 400 {
			stat = "..." + stat[len(stat)-400:]
		}
		b.WriteString("\nChanges:\n")
		b.WriteString(stat)
	}

	return b.String()
}

func (s *RalphSession) ensureRalphDir() {
	if s.RalphDir != "" {
		return
	}
	cwd, _ := os.Getwd()
	s.RalphDir = filepath.Join(cwd, RalphDirName)
	_ = os.MkdirAll(s.RalphDir, 0o755)
	_ = os.MkdirAll(filepath.Join(s.RalphDir, RalphAttemptsDir), 0o755)
}

func (s *RalphSession) writeRalphFiles() {
	s.ensureRalphDir()

	_ = os.WriteFile(filepath.Join(s.RalphDir, RalphTaskFile), []byte(s.renderTaskFile()), 0o644)
	_ = os.WriteFile(filepath.Join(s.RalphDir, RalphProgressFile), []byte(s.renderProgressFile()), 0o644)
	_ = os.WriteFile(filepath.Join(s.RalphDir, RalphContextFile), []byte(s.renderContextFile()), 0o644)

	// Durable STATE.md: create if missing, but DO NOT overwrite (agent owns it).
	s.ensureStateFileExists()

	// ERROR.md: write current last error if present; otherwise keep "(none)".
	s.writeLatestErrorFile()
}

func (s *RalphSession) ensureStateFileExists() {
	path := filepath.Join(s.RalphDir, RalphStateFile)
	_, err := os.Stat(path)
	if err == nil {
		return
	}
	// Create a helpful template.
	template := `# STATE (durable environment memory)

Keep this file SHORT and TRUE. Update whenever environment state changes.

## Current session
- Working URL(s):
- Browser state:
- Servers/processes running (command + port):
- Containers running:
- Env vars set (names only, no secrets):
- How to verify quickly (curl/health/lsof):

## Reset / restart
- How to stop running processes:
- How to start the app:

## Notes
- (keep to a few bullets)
`
	_ = os.WriteFile(path, []byte(template), 0o644)
}

func (s *RalphSession) writeLatestErrorFile() {
	path := filepath.Join(s.RalphDir, RalphErrorFile)
	content := s.renderErrorFile()
	_ = os.WriteFile(path, []byte(content), 0o644)
}

func (s *RalphSession) renderTaskFile() string {
	if s.CurrentTaskIdx >= len(s.Tasks) {
		return "# TASK\n\n(no current task)\n"
	}

	var b strings.Builder
	b.WriteString("# TASK\n\n")
	b.WriteString("## Current\n\n")
	b.WriteString("- ")
	b.WriteString(s.Tasks[s.CurrentTaskIdx].Description)
	b.WriteString("\n\n")

	b.WriteString("## Completed\n\n")
	found := false
	for i := 0; i < len(s.Tasks); i++ {
		if s.Tasks[i].Status == TaskCompleted {
			found = true
			b.WriteString(fmt.Sprintf("- ✅ %s\n", s.Tasks[i].Description))
		}
	}
	if !found {
		b.WriteString("- (none)\n")
	}
	b.WriteString("\n")

	b.WriteString("## Rules\n\n")
	b.WriteString("1. Do ONLY the current task.\n")
	b.WriteString("2. Don’t work on future tasks.\n")
	b.WriteString("3. Prefer small, verifiable changes.\n")
	b.WriteString("4. Run checks/tests as needed.\n")
	b.WriteString(fmt.Sprintf("5. When done, print exactly: `%s`\n", s.SuccessToken))
	b.WriteString("\n")

	return b.String()
}

func (s *RalphSession) renderProgressFile() string {
	var b strings.Builder
	b.WriteString("# PROGRESS\n\n")
	b.WriteString(fmt.Sprintf("- Time: %s\n", time.Now().Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("- Global iteration: %d\n", s.GlobalIteration))
	b.WriteString(fmt.Sprintf("- Task: %d/%d\n\n", s.CurrentTaskIdx+1, len(s.Tasks)))

	if s.CurrentTaskIdx < len(s.Tasks) {
		b.WriteString("## Current task\n\n")
		b.WriteString(s.Tasks[s.CurrentTaskIdx].Description)
		b.WriteString("\n\n")
	}

	b.WriteString("## Attempts (this task)\n\n")
	if len(s.CurrentAttempts) == 0 {
		b.WriteString("- (none yet)\n\n")
	} else {
		for _, att := range s.CurrentAttempts {
			b.WriteString(fmt.Sprintf("- Attempt %d: exit=%d, dur=%s, sig=%q\n",
				att.AttemptNum, att.ExitCode, att.Duration, att.ErrorSig))
		}
		b.WriteString("\n")
	}

	b.WriteString("## Next plan (update this)\n\n")
	b.WriteString("- Observed:\n- Hypothesis:\n- Change made:\n- Result:\n- Next step:\n\n")

	b.WriteString("## Hint\n\n")
	b.WriteString(fmt.Sprintf("- Full attempt transcripts: `%s/%s/attempt-XXXX%s`\n", RalphDirName, RalphAttemptsDir, RalphAttemptExt))
	b.WriteString("- Keep STATE.md up to date for anything outside git.\n")

	return b.String()
}

func (s *RalphSession) renderErrorFile() string {
	var b strings.Builder
	b.WriteString("# ERROR (last failure/output)\n\n")
	b.WriteString(fmt.Sprintf("- Updated: %s\n\n", time.Now().Format(time.RFC3339)))

	if strings.TrimSpace(s.LastErrorCtx) == "" {
		b.WriteString("(none)\n")
		return b.String()
	}

	// IMPORTANT: no truncation here. This is the “last known reality” for the next loop.
	b.WriteString("```text\n")
	b.WriteString(strings.TrimRight(s.LastErrorCtx, "\n"))
	b.WriteString("\n```\n")
	return b.String()
}

func (s *RalphSession) renderContextFile() string {
	var b strings.Builder
	b.WriteString("# CONTEXT\n\n")
	b.WriteString(fmt.Sprintf("- Time: %s\n", time.Now().Format(time.RFC3339)))
	cwd, _ := os.Getwd()
	b.WriteString(fmt.Sprintf("- CWD: %s\n\n", cwd))

	st := strings.TrimSpace(s.captureProjectState())
	if st == "" {
		b.WriteString("(no git context)\n")
		return b.String()
	}

	b.WriteString("```text\n")
	b.WriteString(st)
	b.WriteString("\n```\n")
	return b.String()
}

func (s *RalphSession) writeAttemptTranscript(attemptNum int, prompt, stdout, stderr string, exitCode int, duration time.Duration) error {
	s.ensureRalphDir()
	path := filepath.Join(s.RalphDir, RalphAttemptsDir, fmt.Sprintf("attempt-%04d%s", attemptNum, RalphAttemptExt))

	var b strings.Builder
	b.WriteString("# RALPH ATTEMPT TRANSCRIPT\n")
	b.WriteString(fmt.Sprintf("Time: %s\n", time.Now().Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("Agent: %s\n", s.ToolName))
	b.WriteString(fmt.Sprintf("Task: %d/%d\n", s.CurrentTaskIdx+1, len(s.Tasks)))
	b.WriteString(fmt.Sprintf("Attempt: %d\n", attemptNum))
	b.WriteString(fmt.Sprintf("Exit code: %d\n", exitCode))
	b.WriteString(fmt.Sprintf("Duration: %s\n\n", duration))

	sum := sha256.Sum256([]byte(prompt))
	b.WriteString(fmt.Sprintf("Prompt hash: %s\n\n", hex.EncodeToString(sum[:])[:12]))

	b.WriteString("=== PROMPT ===\n")
	b.WriteString(prompt)
	if !strings.HasSuffix(prompt, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n=== STDERR ===\n")
	b.WriteString(stderr)
	if !strings.HasSuffix(stderr, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n=== STDOUT ===\n")
	b.WriteString(stdout)
	if !strings.HasSuffix(stdout, "\n") {
		b.WriteString("\n")
	}

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func (s *RalphSession) execute(prompt string) (stdout, stderr string, exitCode int, duration time.Duration) {
	var cmd *exec.Cmd

	// Special handling for Copilot CLI - correct argument order
	if s.ToolName == "copilot" {
		cmd = exec.Command("copilot", "suggest", "-p", prompt, "--yolo")
	} else {
		config, exists := agentConfigs[s.ToolName]
		if !exists {
			config = AgentConfig{Name: s.ToolName, NeedsPrompt: true}
		}

		var args []string
		if config.NeedsPrompt {
			args = append([]string{"-p"}, config.Args...)
			args = append(args, prompt)
		} else {
			args = append(config.Args, prompt)
		}
		cmd = exec.Command(s.ToolName, args...)
	}

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	cmd.Stdin = os.Stdin

	cmd.Env = append(os.Environ(),
		fmt.Sprintf("RALPH_ITERATION=%d", s.GlobalIteration),
		fmt.Sprintf("RALPH_TASK=%d", s.CurrentTaskIdx),
		fmt.Sprintf("RALPH_ATTEMPT=%d", s.Tasks[s.CurrentTaskIdx].Attempts),
		"CI=true",
		"NON_INTERACTIVE=true",
	)

	start := time.Now()
	err := cmd.Run()
	duration = time.Since(start).Round(time.Millisecond)

	stdout = outBuf.String()
	stderr = errBuf.String()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	return
}

func (s *RalphSession) has
