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
	PromptFile = "PROMPT.md"
	StopSignal = "RALPH_DONE"
)

func main() {
	// Parse flags
	agentPtr := flag.String("agent", "claude", "The AI agent to use (claude, gemini, copilot)")
	flag.Parse()

	// Handle positional argument if flag not used (e.g., 'ralph gemini')
	agent := *agentPtr
	if len(flag.Args()) > 0 {
		agent = flag.Args()[0]
	}

	fmt.Printf("🎯 Starting Ralph Loop using: %s\n", agent)
	fmt.Println("🛑 Press Ctrl+C to stop.")
	fmt.Println("----------------------------------------")

	// Setup Signal Handling (Ctrl+C)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for {
		// Check for cancellation before starting loop
		if ctx.Err() != nil {
			fmt.Println("\n🛑 Loop stopped by user.")
			return
		}

		// 1. Read Prompt
		instructions, err := os.ReadFile(PromptFile)
		if err != nil {
			fmt.Printf("❌ Error: %s not found in current directory.\n", PromptFile)
			time.Sleep(2 * time.Second)
			continue
		}

		fmt.Println("\n⚡ Running iteration...")

		// 2. Run the Agent
		output, err := runAgent(ctx, agent, string(instructions))

		if err != nil {
			// If the context was canceled (Ctrl+C), exit immediately
			if ctx.Err() != nil {
				fmt.Println("\n🛑 Operation cancelled.")
				return
			}
			fmt.Printf("\n⚠️ Agent process exited with error: %v\n", err)
		}

		// 3. Check for Completion
		if strings.Contains(output, StopSignal) {
			fmt.Println("\n✅ Task Complete (RALPH_DONE detected).")
			return
		}

		fmt.Println("\n🔄 Iteration finished. Resting for 2 seconds...")

		// Wait with interrupt support
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
			continue
		}
	}
}

func runAgent(ctx context.Context, agent string, prompt string) (string, error) {
	var cmd *exec.Cmd

	// Configure command based on agent
	switch agent {
	case "claude":
		// Claude: Args for headless mode
		cmd = exec.CommandContext(ctx, "claude", "-p", prompt, "--dangerously-skip-permissions")

	case "gemini":
		// Gemini: Reads from Stdin
		cmd = exec.CommandContext(ctx, "gemini", "--yolo")
		cmd.Stdin = strings.NewReader(prompt)

	case "copilot":
		// Copilot: Args for headless mode
		cmd = exec.CommandContext(ctx, "copilot", "-p", prompt, "--allow-all-tools")

	default:
		return "", fmt.Errorf("unknown agent: %s", agent)
	}

	// Capture output AND stream to screen simultaneously
	var captureBuf bytes.Buffer
	multiWriter := io.MultiWriter(os.Stdout, &captureBuf)

	cmd.Stdout = multiWriter
	cmd.Stderr = multiWriter

	err := cmd.Run()
	return captureBuf.String(), err
}
