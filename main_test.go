package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// exists reports whether the named file or directory exists.
func exists(path string) bool {
	if path == "" {
		return false
	}
	if _, err := os.Stat(path); err == nil {
		return true
	} else if os.IsNotExist(err) {
		return false
	} else {
		return false
	}
}

func TestOllamaWrapper_Query(t *testing.T) {
	binary := os.Getenv("OLLAMA_BIN")
	if binary == "" {
		binary = "/usr/local/bin/ollama"
	}
	// Quick sanity check: binary path must exist
	if !exists(binary) {
		t.Skipf("ollama binary not found at %s; set OLLAMA_BIN to run this test", binary)
	}
	// Also ensure it's executable
	if fi, err := os.Stat(binary); err == nil {
		if fi.Mode()&0111 == 0 {
			t.Skipf("ollama binary at %s is not executable", binary)
		}
	}

	model := os.Getenv("OLLAMA_MODEL")
	if model == "" {
		model = "SmolLM"
	}

	// Log the command we are about to run (mirrors application startup log)
	t.Logf("starting ollama: %s run %s", binary, model)

	// Ensure PATH contains the directory of the binary so any sub-process lookups work consistently
	binDir := filepath.Dir(binary)
	os.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Defensive: ensure we can at least run `ollama --version`
	ctxCheck, cancelCheck := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCheck()
	if err := exec.CommandContext(ctxCheck, binary, "--version").Run(); err != nil {
		t.Skipf("ollama binary exists but not runnable: %v", err)
	}

	// Perform the test query using non-interactive mode
	prompt := "hello, hello, hello, is there anybody there?"
	ctx, c := context.WithTimeout(context.Background(), 2*time.Minute)
	defer c()
	t.Logf("starting ollama: %s run %s %q", binary, model, prompt)
	resp, err := runOllamaOnce(ctx, binary, model, prompt)
	if err != nil {
		t.Fatalf("non-interactive run failed: %v\noutput: %s", err, resp)
	}
	if len(resp) == 0 {
		t.Fatalf("empty response from ollama")
	}
	t.Logf("response: %q", resp)
}
