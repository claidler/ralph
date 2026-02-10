package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Configuration
const (
	PromptFile   = "PROMPT.md"
	ErrorLogFile = "ralph-error.log"
	MaxLogLines  = 300
)

func main() {
	// Parse flags
	agentPtr := flag.String("agent", "claude", "The AI agent to use (claude, gemini, copilot, codex, vibe, opencode)")
	checkCmdPtr := flag.String("check", "", "The verification command (e.g., 'go test ./...'). Loop stops when this passes.")
	flag.Parse()

	agent := *agentPtr
	if len(flag.Args()) > 0 {
		agent = flag.Args()[0]
	}

	fmt.Printf("🎯 Starting Ralph Loop using: %s\n", agent)
	if *checkCmdPtr != "" {
		fmt.Printf("🛡️  Verification Command: %s\n", *checkCmdPtr)
	}
	fmt.Println("----------------------------------------")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for {
		if ctx.Err() != nil {
			return
		}

		// 1. Run Verification (Physics Check)
		if *checkCmdPtr != "" {
			fmt.Printf("\n🔎 Running check: %s ...\n", *checkCmdPtr)
			output, err := runShellCommand(ctx, *checkCmdPtr)

			if err == nil {
				// Success! Clean up the error log so we don't confuse future runs
				_ = os.Remove(ErrorLogFile)
				fmt.Println("\n✅ Verification PASSED! Task complete.")
				return
			}

			// Failure! PERSIST the error to a file (The Ralph Way)
			fmt.Println("❌ Verification FAILED. Writing error tail to disk...")
			writeErrorLog(output)
		}

		// 2. Read Base Prompt
		instructions, err := os.ReadFile(PromptFile)
		if err != nil {
			fmt.Printf("❌ Error: %s not found.\n", PromptFile)
			time.Sleep(2 * time.Second)
			continue
		}

		// 3. Construct Prompt with Context
		fullPrompt := string(instructions)

		// Check if an error log exists from the verification step
		if _, err := os.Stat(ErrorLogFile); err == nil {
			errorContent, _ := os.ReadFile(ErrorLogFile)
			// Inject the error (Feedback Loop)
			fullPrompt = fmt.Sprintf("%s\n\n!!! PREVIOUS ATTEMPT FAILED !!!\nI have written the verification logs to '%s'.\nHere is the TAIL of the output (most relevant errors):\n```\n%s\n```\nFix this error based on the file content.", string(instructions), ErrorLogFile, string(errorContent))
		}

		fmt.Println("\n⚡ Running Agent iteration...")

		// 4. Run Agent (Fresh Malloc)
		_, err = runAgent(ctx, agent, fullPrompt)

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Printf("\n⚠️ Agent process exited with error: %v\n", err)
		}

		fmt.Println("\n🔄 Iteration finished. Resting for 2 seconds...")

		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
			continue
		}
	}
}

func writeErrorLog(content string) {
	lines := strings.Split(content, "\n")

	var finalContent string

	if len(lines) > MaxLogLines {
		startIndex := len(lines) - MaxLogLines
		tail := strings.Join(lines[startIndex:], "\n")
		finalContent = fmt.Sprintf("... [TRUNCATED: Removed %d lines of earlier output. Showing last %d lines] ...\n%s", startIndex, MaxLogLines, tail)
	} else {
		finalContent = content
	}

	err := os.WriteFile(ErrorLogFile, []byte(finalContent), 0644)
	if err != nil {
		fmt.Printf("⚠️ Failed to write error log: %v\n", err)
	}
}

func runShellCommand(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func runAgent(ctx context.Context, agent string, prompt string) (string, error) {
	var cmd *exec.Cmd
	switch agent {
	case "claude":
		cmd = exec.CommandContext(ctx, "claude", "-p", prompt, "--dangerously-skip-permissions")
	case "gemini":
		cmd = exec.CommandContext(ctx, "gemini", "--yolo")
		cmd.Stdin = strings.NewReader(prompt)
	case "copilot":
		cmd = exec.CommandContext(ctx, "copilot", "-p", prompt, "--allow-all-tools")
	case "codex":
		cmd = exec.CommandContext(ctx, "codex", "exec", "--dangerously-bypass-approvals-and-sandbox", "-")
		cmd.Stdin = strings.NewReader(prompt)
	case "vibe":
		// Mistral Vibe: Uses --prompt argument and --agent auto-approve for headless mode
		cmd = exec.CommandContext(ctx, "vibe", "--prompt", prompt, "--agent", "auto-approve")
	case "opencode":
		// OpenCode: Uses run command with prompt, auto-approves by default
		cmd = exec.CommandContext(ctx, "opencode", "run", prompt)
	default:
		return "", fmt.Errorf("unknown agent: %s", agent)
	}

	var captureBuf bytes.Buffer
	multiWriter := io.MultiWriter(os.Stdout, &captureBuf)
	cmd.Stdout = multiWriter
	cmd.Stderr = multiWriter

	err := cmd.Run()
	return captureBuf.String(), err
}
