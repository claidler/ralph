package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")

	evt := StatusEvent{
		Event:     "iteration_start",
		Iteration: 1,
		Agent:     "claude",
	}

	if err := writeStatus(path, evt); err != nil {
		t.Fatalf("writeStatus returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read status file: %v", err)
	}

	var got StatusEvent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("failed to unmarshal status: %v", err)
	}

	if got.Event != "iteration_start" {
		t.Errorf("event = %q, want %q", got.Event, "iteration_start")
	}
	if got.Iteration != 1 {
		t.Errorf("iteration = %d, want 1", got.Iteration)
	}
	if got.Agent != "claude" {
		t.Errorf("agent = %q, want %q", got.Agent, "claude")
	}
	if got.Timestamp == "" {
		t.Error("timestamp should be set")
	}
}

func TestWriteStatusComplete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")

	evt := StatusEvent{
		Event:     "complete",
		Iteration: 3,
		Agent:     "gemini",
		ExitCode:  ExitComplete,
		DoneFlag:  true,
	}

	if err := writeStatus(path, evt); err != nil {
		t.Fatalf("writeStatus returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read status file: %v", err)
	}

	var got StatusEvent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("failed to unmarshal status: %v", err)
	}

	if got.Event != "complete" {
		t.Errorf("event = %q, want %q", got.Event, "complete")
	}
	if got.DoneFlag != true {
		t.Error("done_flag should be true for complete event")
	}
	if got.ExitCode != ExitComplete {
		t.Errorf("exit_code = %d, want %d", got.ExitCode, ExitComplete)
	}
}

func TestWriteStatusOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")

	// Write first event
	writeStatus(path, StatusEvent{Event: "iteration_start", Iteration: 1, Agent: "claude"})
	// Write second event (should overwrite)
	writeStatus(path, StatusEvent{Event: "iteration_end", Iteration: 1, Agent: "claude"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read status file: %v", err)
	}

	var got StatusEvent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("failed to unmarshal status: %v", err)
	}

	if got.Event != "iteration_end" {
		t.Errorf("event = %q, want %q (should reflect latest write)", got.Event, "iteration_end")
	}
}

func TestEmitStatusNoOp(t *testing.T) {
	// emitStatus with empty path should be a no-op (no panic, no error)
	emitStatus("", StatusEvent{Event: "test", Iteration: 1, Agent: "claude"})
}

func TestEmitStatusWritesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")

	emitStatus(path, StatusEvent{Event: "iteration_start", Iteration: 2, Agent: "copilot"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("status file should exist: %v", err)
	}

	var got StatusEvent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if got.Event != "iteration_start" {
		t.Errorf("event = %q, want %q", got.Event, "iteration_start")
	}
	if got.Iteration != 2 {
		t.Errorf("iteration = %d, want 2", got.Iteration)
	}
}

func TestExitCodes(t *testing.T) {
	// Verify exit code constants have expected values
	if ExitComplete != 0 {
		t.Errorf("ExitComplete = %d, want 0", ExitComplete)
	}
	if ExitError != 1 {
		t.Errorf("ExitError = %d, want 1", ExitError)
	}
	if ExitCancelled != 2 {
		t.Errorf("ExitCancelled = %d, want 2", ExitCancelled)
	}
}

func TestStatusEventJSON(t *testing.T) {
	evt := StatusEvent{
		Event:     "cancelled",
		Iteration: 5,
		Agent:     "claude",
		Timestamp: "2025-01-01T00:00:00Z",
		ExitCode:  ExitCancelled,
	}

	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	// Verify omitempty fields are absent when zero
	var m map[string]interface{}
	json.Unmarshal(data, &m)

	if _, ok := m["message"]; ok {
		t.Error("message should be omitted when empty")
	}
	if _, ok := m["done_flag"]; ok {
		t.Error("done_flag should be omitted when false")
	}

	// exit_code 2 should be present
	if code, ok := m["exit_code"]; !ok || int(code.(float64)) != ExitCancelled {
		t.Errorf("exit_code should be %d", ExitCancelled)
	}
}
