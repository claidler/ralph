package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// Exit codes for script integration
const (
	ExitComplete  = 0 // Task completed (RALPH_DONE detected)
	ExitError     = 1 // An error occurred
	ExitCancelled = 2 // User cancelled (Ctrl+C / SIGTERM)
)

// StatusEvent represents a machine-readable status update written to the status file.
type StatusEvent struct {
	Event     string `json:"event"`               // "iteration_start", "iteration_end", "complete", "cancelled"
	Iteration int    `json:"iteration"`            // Current iteration number (1-based)
	Agent     string `json:"agent"`                // Agent name
	Timestamp string `json:"timestamp"`            // RFC3339 timestamp
	Message   string `json:"message,omitempty"`    // Human-readable message
	ExitCode  int    `json:"exit_code,omitempty"`  // Set on terminal events
	DoneFlag  bool   `json:"done_flag,omitempty"`  // True when RALPH_DONE was detected
}

// writeStatus writes a JSON status event to the given file (one JSON object per line).
func writeStatus(path string, evt StatusEvent) error {
	evt.Timestamp = time.Now().UTC().Format(time.RFC3339)
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

func main() {
	os.Exit(run())
}

func run() int {
	// Parse flags
	agentPtr := flag.String("agent", "claude", "The AI agent to use (claude, gemini, copilot)")
	statusFilePtr := flag.String("status-file", "", "Path to write machine-readable JSON status events (for script integration)")
	flag.Parse()

	// Handle positional argument if flag not used (e.g., 'ralph gemini')
	agent := *agentPtr
	if len(flag.Args()) > 0 {
		agent = flag.Args()[0]
	}
	statusFile := *statusFilePtr

	fmt.Printf("🎯 Starting Ralph Loop using: %s\n", agent)
	fmt.Println("🛑 Press Ctrl+C to stop.")
	fmt.Println("----------------------------------------")

	// Setup Signal Handling (Ctrl+C)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	iteration := 0

	for {
		// Check for cancellation before starting loop
		if ctx.Err() != nil {
			fmt.Println("\n🛑 Loop stopped by user.")
			emitStatus(statusFile, StatusEvent{Event: "cancelled", Iteration: iteration, Agent: agent, ExitCode: ExitCancelled})
			return ExitCancelled
		}

		// 1. Read Prompt
		instructions, err := os.ReadFile(PromptFile)
		if err != nil {
			fmt.Printf("❌ Error: %s not found in current directory.\n", PromptFile)
			time.Sleep(2 * time.Second)
			continue
		}

		iteration++
		fmt.Println("\n⚡ Running iteration...")
		emitStatus(statusFile, StatusEvent{Event: "iteration_start", Iteration: iteration, Agent: agent})

		// 2. Run the Agent
		output, err := runAgent(ctx, agent, string(instructions))

		if err != nil {
			// If the context was canceled (Ctrl+C), exit immediately
			if ctx.Err() != nil {
				fmt.Println("\n🛑 Operation cancelled.")
				emitStatus(statusFile, StatusEvent{Event: "cancelled", Iteration: iteration, Agent: agent, ExitCode: ExitCancelled})
				return ExitCancelled
			}
			fmt.Printf("\n⚠️ Agent process exited with error: %v\n", err)
		}

		// 3. Check for Completion
		if strings.Contains(output, StopSignal) {
			fmt.Println("\n✅ Task Complete (RALPH_DONE detected).")
			emitStatus(statusFile, StatusEvent{Event: "complete", Iteration: iteration, Agent: agent, ExitCode: ExitComplete, DoneFlag: true})
			return ExitComplete
		}

		fmt.Println("\n🔄 Iteration finished. Resting for 2 seconds...")
		emitStatus(statusFile, StatusEvent{Event: "iteration_end", Iteration: iteration, Agent: agent})

		// Wait with interrupt support
		select {
		case <-ctx.Done():
			emitStatus(statusFile, StatusEvent{Event: "cancelled", Iteration: iteration, Agent: agent, ExitCode: ExitCancelled})
			return ExitCancelled
		case <-time.After(2 * time.Second):
			continue
		}
	}
}

// emitStatus writes a status event if a status file path is configured.
func emitStatus(path string, evt StatusEvent) {
	if path == "" {
		return
	}
	if err := writeStatus(path, evt); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️ Failed to write status file: %v\n", err)
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
