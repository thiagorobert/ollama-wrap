package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// runOllamaOnce executes a single non-interactive ollama run with the given prompt.
func runOllamaOnce(ctx context.Context, binaryPath, model, prompt string) (string, error) {
	// Build command: ollama run <model> <prompt>
	args := []string{"run", model, prompt}
	log.Printf("exec: %s", strings.Join(append([]string{binaryPath}, args...), " "))
	cmd := exec.CommandContext(ctx, binaryPath, args...)
	// Capture combined stdout+stderr to return/log on failures
	output, err := cmd.CombinedOutput()
	str_output := string(output)
	// Remove ANSI escape codes
	// From https://github.com/acarl005/stripansi/blob/master/stripansi.go
	const ansi = "[\u001B\u009B][[\\]()#;?]*(?:(?:(?:[a-zA-Z\\d]*(?:;[a-zA-Z\\d]*)*)?\u0007)|(?:(?:\\d{1,4}(?:;\\d{0,4})*)?[\\dA-PRZcf-ntqry=><~]))"
	var re = regexp.MustCompile(ansi)
	cleanedString := re.ReplaceAllString(str_output, "")
	text := strings.TrimSpace(cleanedString)
	if err != nil {
		// Include output in error for debugging
		return text, fmt.Errorf("ollama run failed: %w", err)
	}
	return text, nil
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "SmolLM"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: ":8080"}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ollama wrapper")
	})

	http.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
		input := r.URL.Query().Get("input")
		if strings.TrimSpace(input) == "" {
			http.Error(w, "missing 'input'", http.StatusBadRequest)
			return
		}
		// Log the incoming query before sending it to the ollama process
		log.Printf("/query from %s: %q", r.RemoteAddr, input)
		// Per-request timeout (5 minutes)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		resp, err := runOllamaOnce(ctx, binary, model, input)
		if err != nil {
			// Include partial output if any for debugging
			http.Error(w, fmt.Sprintf("%v\n%s", err, resp), http.StatusInternalServerError)
			return
		}
		// Log the response obtained from ollama
		// log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}
