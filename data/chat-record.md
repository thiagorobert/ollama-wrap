# Golang webserver for command line tool
_Exported on 8/10/2025 at 08:33:58 PDT from Cursor (1.4.3)_

---

**User**

this is a new project. I want a golang application which consists of a webserver that 'wraps' command line tool /usr/local/bin/ollama . On startup, the application will run 'ollama run gpt-oss', which is interactive. The webserver will have a handler '/query?input=' the value of input is passed to the running ollama process, and the process response is used as the HTTP response

---

**Cursor**

I'll set up a new Go module with a webserver, then create a persistent interactive subprocess to `/usr/local/bin/ollama run gpt-oss`. I'll wire a `/query?input=` HTTP handler that writes input to the process and returns the model output. I'll research the CLI interactive output format to correctly delimit responses, then implement concurrency-safe request/response handling. Proceeding to gather details on Ollama CLI interaction semantics.

I'll quickly check how the Ollama interactive CLI signals the end of a response so we can delimit responses reliably, then scaffold the Go module and server with a single serialized request pipeline.

I'll quickly verify if `ollama run` supports a JSON output mode to reliably detect the end of responses, then scaffold the Go module and implement a server that starts and manages a single interactive process with concurrency-safe request/response handling.

I'll look up current Ollama CLI flags to ensure we can parse responses reliably, ideally via JSON with a 'done' flag. Then I'll scaffold the Go module and implement the server, creating necessary files.

Setting up a new Go module and implementing the webserver that manages a persistent interactive Ollama process. I’ll create `go.mod`, `main.go`, and a brief `README.md`, then build to ensure it compiles.

```xml
module ollama-wrap

go 1.22


```

```go
package main

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "strings"
    "sync"
    "syscall"
    "time"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
    cmd            *exec.Cmd
    stdinWriter    io.WriteCloser
    stdoutReader   *bufio.Reader
    stderrReader   *bufio.Reader
    ioMutex        sync.Mutex // serialize access to interactive session
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
    cmd := exec.CommandContext(ctx, binaryPath, "run", model)
    // Create a pseudo-terminal-like environment by disabling stdin buffering issues
    // We'll rely on newline termination

    stdin, err := cmd.StdinPipe()
    if err != nil {
        return nil, fmt.Errorf("stdin pipe: %w", err)
    }
    stdout, err := cmd.StdoutPipe()
    if err != nil {
        return nil, fmt.Errorf("stdout pipe: %w", err)
    }
    stderr, err := cmd.StderrPipe()
    if err != nil {
        return nil, fmt.Errorf("stderr pipe: %w", err)
    }

    if err := cmd.Start(); err != nil {
        return nil, fmt.Errorf("start ollama: %w", err)
    }

    wrapper := &OllamaWrapper{
        cmd:          cmd,
        stdinWriter:  stdin,
        stdoutReader: bufio.NewReader(stdout),
        stderrReader: bufio.NewReader(stderr),
    }

    // Optionally read initial banner until ready; best-effort with timeout
    readyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
    defer cancel()
    go func() {
        // Drain any initial stderr to logs
        for {
            select {
            case <-readyCtx.Done():
                return
            default:
            }
            line, err := wrapper.stderrReader.ReadString('\n')
            if len(line) > 0 {
                log.Printf("ollama stderr: %s", strings.TrimRight(line, "\n"))
            }
            if err != nil {
                return
            }
        }
    }()

    return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
    ow.ioMutex.Lock()
    defer ow.ioMutex.Unlock()

    if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
        return "", errors.New("ollama process not running")
    }

    // Ensure input ends with a newline
    if !strings.HasSuffix(userInput, "\n") {
        userInput += "\n"
    }

    // Write input
    if _, err := io.WriteString(ow.stdinWriter, userInput); err != nil {
        return "", fmt.Errorf("write stdin: %w", err)
    }

    // Read response with idle timeout collection
    var b strings.Builder
    idleWindow := 400 * time.Millisecond
    var gotAny bool

    for {
        // Set read deadline by wrapping with a context deadline
        _ = ow.cmd // keep reference for liveness check
        // Use ReadString non-blocking with timeout via goroutine and select
        lineCh := make(chan struct{
            s string
            err error
        }, 1)
        go func() {
            s, err := ow.stdoutReader.ReadString('\n')
            lineCh <- struct{ s string; err error }{s, err}
        }()

        select {
        case res := <-lineCh:
            if len(res.s) > 0 {
                b.WriteString(res.s)
                gotAny = true
            }
            if res.err != nil {
                if errors.Is(res.err, io.EOF) {
                    // Process ended; return what we have
                    return strings.TrimSpace(b.String()), nil
                }
                return "", fmt.Errorf("read stdout: %w", res.err)
            }
            // Continue to try reading more until idle
            // Loop continues
        case <-time.After(idleWindow):
            if gotAny {
                return strings.TrimSpace(b.String()), nil
            }
            // No output yet; extend the wait unless context is done
            select {
            case <-ctx.Done():
                return "", ctx.Err()
            default:
                // loop back to try again
            }
        case <-ctx.Done():
            return "", ctx.Err()
        }
    }
}

func main() {
    binary := "/usr/local/bin/ollama"
    model := "gpt-oss"
    if b := os.Getenv("OLLAMA_BIN"); b != "" {
        binary = b
    }
    if m := os.Getenv("OLLAMA_MODEL"); m != "" {
        model = m
    }

    rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    wrapper, err := startOllamaInteractive(rootCtx, binary, model)
    if err != nil {
        log.Fatalf("failed to start ollama: %v", err)
    }

    srv := &http.Server{Addr: ":8080"}

    http.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
        input := r.URL.Query().Get("input")
        if strings.TrimSpace(input) == "" {
            http.Error(w, "missing 'input'", http.StatusBadRequest)
            return
        }
        // Per-request timeout
        ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
        defer cancel()
        resp, err := wrapper.query(ctx, input)
        if err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        w.Header().Set("Content-Type", "text/plain; charset=utf-8")
        _, _ = io.WriteString(w, resp)
    })

    go func() {
        <-rootCtx.Done()
        shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
        defer cancel()
        _ = srv.Shutdown(shutdownCtx)
        if wrapper.cmd.Process != nil {
            _ = wrapper.cmd.Process.Kill()
        }
    }()

    log.Printf("server listening on %s", srv.Addr)
    if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
        log.Fatalf("server error: %v", err)
    }
}


```

```markdown
<<'PATCH'
*** Begin Patch
*** Add File: /workspace/ollama-wrap/README.md
+# ollama-wrap

A minimal Go webserver that wraps an interactive `/usr/local/bin/ollama run gpt-oss` session and exposes a `/query?input=` endpoint.

- Binary path can be overridden via `OLLAMA_BIN`
- Model can be overridden via `OLLAMA_MODEL`
- Server listens on `:8080`

Build and run:

```bash
cd /workspace/ollama-wrap
go build -o ollama-wrap
./ollama-wrap
```

Example query:

```bash
curl --get 'http://localhost:8080/query' --data-urlencode 'input=Hello!'
```

Notes:
- Since `ollama run` interactive does not emit a clear end-of-response marker, the server collects output until an idle window (~400ms) elapses after receiving output.
- Requests are serialized through a mutex to avoid interleaved IO with the single interactive process.
*** End Patch
PATCH
```

I’m going to adjust the read logic to handle streaming without relying on newline delimiters, then add a short README, and build the project to ensure it compiles.

```markdown
ollama-wrap
===========

A minimal Go webserver that wraps an interactive `/usr/local/bin/ollama run gpt-oss` session and exposes a `/query?input=` endpoint.

- OLLAMA_BIN: override binary path (default `/usr/local/bin/ollama`)
- OLLAMA_MODEL: override model (default `gpt-oss`)
- Listens on `:8080`

Build and run:

```bash
cd /workspace/ollama-wrap
go build -o ollama-wrap
./ollama-wrap
```

Example:

```bash
curl --get 'http://localhost:8080/query' --data-urlencode 'input=Hello!'
```

Notes:
- Interactive `ollama run` does not expose a clear end-of-response marker; this wrapper collects output until an idle window (~400ms) elapses after receiving output and then returns the aggregated text.
- Requests are serialized to avoid interleaved I/O with the single interactive process.

```

Status: Scaffolded a new Go module and implemented a webserver that starts a persistent interactive `/usr/local/bin/ollama run gpt-oss` process. Added a serialized `/query?input=` handler that writes to stdin and reads stdout using an idle-timeout heuristic to delimit responses. Built successfully.

How to run:
- Build and run:
  - cd `/workspace/ollama-wrap`
  - go build -o ollama-wrap
  - ./ollama-wrap
- Query:
  - curl --get 'http://localhost:8080/query' --data-urlencode 'input=Hello!'

Notes:
- Env vars: `OLLAMA_BIN` (default `/usr/local/bin/ollama`), `OLLAMA_MODEL` (default `gpt-oss`).
- Concurrency: requests are serialized to avoid interleaved IO with the single interactive session.
- Response delimiting uses a short idle window (~400ms) after receiving output. This is a practical heuristic; if you prefer a different model behavior (e.g., JSON streaming with a done flag), we could switch to `ollama generate --json` per request instead of maintaining an interactive session.

Changes:
- Added `go.mod`.
- Implemented `main.go` webserver with `OllamaWrapper` and `/query` handler using time-based aggregation.
- Added `README.md` with usage instructions.

If you want streaming HTTP responses or per-request processes instead of a shared session, say the word and I’ll adjust.

---

**User**

the request might take quite a while to process, increase the context deadline to 5 minutes

---

**Cursor**

Making the per-request timeout five minutes so long-running requests won't be canceled prematurely. Building after the change.

```go
package main

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "strings"
    "sync"
    "syscall"
    "time"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
    cmd            *exec.Cmd
    stdinWriter    io.WriteCloser
    stdoutReader   *bufio.Reader
    stderrReader   *bufio.Reader
    ioMutex        sync.Mutex // serialize access to interactive session
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
    cmd := exec.CommandContext(ctx, binaryPath, "run", model)
    // Create a pseudo-terminal-like environment by disabling stdin buffering issues
    // We'll rely on newline termination

    stdin, err := cmd.StdinPipe()
    if err != nil {
        return nil, fmt.Errorf("stdin pipe: %w", err)
    }
    stdout, err := cmd.StdoutPipe()
    if err != nil {
        return nil, fmt.Errorf("stdout pipe: %w", err)
    }
    stderr, err := cmd.StderrPipe()
    if err != nil {
        return nil, fmt.Errorf("stderr pipe: %w", err)
    }

    if err := cmd.Start(); err != nil {
        return nil, fmt.Errorf("start ollama: %w", err)
    }

    wrapper := &OllamaWrapper{
        cmd:          cmd,
        stdinWriter:  stdin,
        stdoutReader: bufio.NewReader(stdout),
        stderrReader: bufio.NewReader(stderr),
    }

    // Optionally read initial banner until ready; best-effort with timeout
    readyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
    defer cancel()
    go func() {
        // Drain any initial stderr to logs
        for {
            select {
            case <-readyCtx.Done():
                return
            default:
            }
            line, err := wrapper.stderrReader.ReadString('\n')
            if len(line) > 0 {
                log.Printf("ollama stderr: %s", strings.TrimRight(line, "\n"))
            }
            if err != nil {
                return
            }
        }
    }()

    return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
    ow.ioMutex.Lock()
    defer ow.ioMutex.Unlock()

    if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
        return "", errors.New("ollama process not running")
    }

    // Ensure input ends with a newline
    if !strings.HasSuffix(userInput, "\n") {
        userInput += "\n"
    }

    // Write input
    if _, err := io.WriteString(ow.stdinWriter, userInput); err != nil {
        return "", fmt.Errorf("write stdin: %w", err)
    }

    // Read response with idle timeout collection
    var b strings.Builder
    idleWindow := 400 * time.Millisecond
    var gotAny bool

    for {
        // Set read deadline by wrapping with a context deadline
        _ = ow.cmd // keep reference for liveness check
        // Use ReadString non-blocking with timeout via goroutine and select
        lineCh := make(chan struct{
            s string
            err error
        }, 1)
        go func() {
            s, err := ow.stdoutReader.ReadString('\n')
            lineCh <- struct{ s string; err error }{s, err}
        }()

        select {
        case res := <-lineCh:
            if len(res.s) > 0 {
                b.WriteString(res.s)
                gotAny = true
            }
            if res.err != nil {
                if errors.Is(res.err, io.EOF) {
                    // Process ended; return what we have
                    return strings.TrimSpace(b.String()), nil
                }
                return "", fmt.Errorf("read stdout: %w", res.err)
            }
            // Continue to try reading more until idle
            // Loop continues
        case <-time.After(idleWindow):
            if gotAny {
                return strings.TrimSpace(b.String()), nil
            }
            // No output yet; extend the wait unless context is done
            select {
            case <-ctx.Done():
                return "", ctx.Err()
            default:
                // loop back to try again
            }
        case <-ctx.Done():
            return "", ctx.Err()
        }
    }
}

func main() {
    binary := "/usr/local/bin/ollama"
    model := "gpt-oss"
    if b := os.Getenv("OLLAMA_BIN"); b != "" {
        binary = b
    }
    if m := os.Getenv("OLLAMA_MODEL"); m != "" {
        model = m
    }

    rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    wrapper, err := startOllamaInteractive(rootCtx, binary, model)
    if err != nil {
        log.Fatalf("failed to start ollama: %v", err)
    }

    srv := &http.Server{Addr: ":8080"}

    http.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
        input := r.URL.Query().Get("input")
        if strings.TrimSpace(input) == "" {
            http.Error(w, "missing 'input'", http.StatusBadRequest)
            return
        }
        // Per-request timeout (5 minutes)
        ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
        defer cancel()
        resp, err := wrapper.query(ctx, input)
        if err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        w.Header().Set("Content-Type", "text/plain; charset=utf-8")
        _, _ = io.WriteString(w, resp)
    })

    go func() {
        <-rootCtx.Done()
        shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
        defer cancel()
        _ = srv.Shutdown(shutdownCtx)
        if wrapper.cmd.Process != nil {
            _ = wrapper.cmd.Process.Kill()
        }
    }()

    log.Printf("server listening on %s", srv.Addr)
    if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
        log.Fatalf("server error: %v", err)
    }
}



```

- Increased the per-request timeout to 5 minutes in `main.go` and rebuilt successfully.
- You can now handle long-running queries without premature cancellation.

---

**User**

also add a root handler (/) that simply responds with 'ollama wrapper' in plain text

---

**Cursor**

Adding a simple root handler that responds with plain text, then rebuilding to verify.

```go
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
	cmd          *exec.Cmd
	stdinWriter  io.WriteCloser
	stdoutReader *bufio.Reader
	stderrReader *bufio.Reader
	ioMutex      sync.Mutex // serialize access to interactive session
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ollama: %w", err)
	}

	wrapper := &OllamaWrapper{
		cmd:          cmd,
		stdinWriter:  stdin,
		stdoutReader: bufio.NewReader(stdout),
		stderrReader: bufio.NewReader(stderr),
	}

	// Optionally read initial banner until ready; best-effort with timeout
	readyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go func() {
		// Drain any initial stderr to logs
		for {
			select {
			case <-readyCtx.Done():
				return
			default:
			}
			line, err := wrapper.stderrReader.ReadString('\n')
			if len(line) > 0 {
				log.Printf("ollama stderr: %s", strings.TrimRight(line, "\n"))
			}
			if err != nil {
				return
			}
		}
	}()

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

	// Write input
	if _, err := io.WriteString(ow.stdinWriter, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

	// Read response with idle timeout collection
	var b strings.Builder
	idleWindow := 400 * time.Millisecond
	var gotAny bool

	for {
		// Set read deadline by wrapping with a context deadline
		_ = ow.cmd // keep reference for liveness check
		// Use ReadString non-blocking with timeout via goroutine and select
		lineCh := make(chan struct {
			s   string
			err error
		}, 1)
		go func() {
			s, err := ow.stdoutReader.ReadString('\n')
			lineCh <- struct {
				s   string
				err error
			}{s, err}
		}()

		select {
		case res := <-lineCh:
			if len(res.s) > 0 {
				b.WriteString(res.s)
				gotAny = true
			}
			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					// Process ended; return what we have
					return strings.TrimSpace(b.String()), nil
				}
				return "", fmt.Errorf("read stdout: %w", res.err)
			}
			// Continue to try reading more until idle
			// Loop continues
		case <-time.After(idleWindow):
			if gotAny {
				return strings.TrimSpace(b.String()), nil
			}
			// No output yet; extend the wait unless context is done
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
				// loop back to try again
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

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
		// Per-request timeout (5 minutes)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

---

**User**

add some logging: before any queries are passed on ollama they are logged

---

**Cursor**

Adding a log statement in the `/query` handler to record the input before it’s sent to the Ollama process, then rebuilding.

```go
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
	cmd          *exec.Cmd
	stdinWriter  io.WriteCloser
	stdoutReader *bufio.Reader
	stderrReader *bufio.Reader
	ioMutex      sync.Mutex // serialize access to interactive session
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ollama: %w", err)
	}

	wrapper := &OllamaWrapper{
		cmd:          cmd,
		stdinWriter:  stdin,
		stdoutReader: bufio.NewReader(stdout),
		stderrReader: bufio.NewReader(stderr),
	}

	// Optionally read initial banner until ready; best-effort with timeout
	readyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go func() {
		// Drain any initial stderr to logs
		for {
			select {
			case <-readyCtx.Done():
				return
			default:
			}
			line, err := wrapper.stderrReader.ReadString('\n')
			if len(line) > 0 {
				log.Printf("ollama stderr: %s", strings.TrimRight(line, "\n"))
			}
			if err != nil {
				return
			}
		}
	}()

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

	// Write input
	if _, err := io.WriteString(ow.stdinWriter, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

	// Read response with idle timeout collection
	var b strings.Builder
	idleWindow := 400 * time.Millisecond
	var gotAny bool

	for {
		// Set read deadline by wrapping with a context deadline
		_ = ow.cmd // keep reference for liveness check
		// Use ReadString non-blocking with timeout via goroutine and select
		lineCh := make(chan struct {
			s   string
			err error
		}, 1)
		go func() {
			s, err := ow.stdoutReader.ReadString('\n')
			lineCh <- struct {
				s   string
				err error
			}{s, err}
		}()

		select {
		case res := <-lineCh:
			if len(res.s) > 0 {
				b.WriteString(res.s)
				gotAny = true
			}
			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					// Process ended; return what we have
					return strings.TrimSpace(b.String()), nil
				}
				return "", fmt.Errorf("read stdout: %w", res.err)
			}
			// Continue to try reading more until idle
			// Loop continues
		case <-time.After(idleWindow):
			if gotAny {
				return strings.TrimSpace(b.String()), nil
			}
			// No output yet; extend the wait unless context is done
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
				// loop back to try again
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

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
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

- Added request logging in `main.go` for `/query` before sending input to Ollama:
  - Logs remote address and the exact input.
- Rebuilt successfully.

Example log line:
- /query from 127.0.0.1:54321: "Hello!"

---

**User**

how do I run the binary enabling logging to stdout?

---

**Cursor**

- Easiest: merge stderr into stdout when running (Go’s log writes to stderr by default).
  - cd /workspace/ollama-wrap
  - ./ollama-wrap 2>&1 | cat
- Alternatively (code change): add at the top of `main()`:
  ```go
  log.SetOutput(os.Stdout)
  ```
  Then rebuild and run normally.

```go
  log.SetOutput(os.Stdout)
```

---

**User**

after a response is retrieved from ollama, log that too. Also, on startup make a test request to ollama (send 'hello, hello, hello, is there anybody in there') and log input output

---

**Cursor**

Making the requested logging additions: logging the response after each query and performing a startup test query with logs. Then I’ll rebuild to verify.

```go
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
	cmd          *exec.Cmd
	stdinWriter  io.WriteCloser
	stdoutReader *bufio.Reader
	stderrReader *bufio.Reader
	ioMutex      sync.Mutex // serialize access to interactive session
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ollama: %w", err)
	}

	wrapper := &OllamaWrapper{
		cmd:          cmd,
		stdinWriter:  stdin,
		stdoutReader: bufio.NewReader(stdout),
		stderrReader: bufio.NewReader(stderr),
	}

	// Optionally read initial banner until ready; best-effort with timeout
	readyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go func() {
		// Drain any initial stderr to logs
		for {
			select {
			case <-readyCtx.Done():
				return
			default:
			}
			line, err := wrapper.stderrReader.ReadString('\n')
			if len(line) > 0 {
				log.Printf("ollama stderr: %s", strings.TrimRight(line, "\n"))
			}
			if err != nil {
				return
			}
		}
	}()

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

	// Write input
	if _, err := io.WriteString(ow.stdinWriter, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

	// Read response with idle timeout collection
	var b strings.Builder
	idleWindow := 400 * time.Millisecond
	var gotAny bool

	for {
		// Set read deadline by wrapping with a context deadline
		_ = ow.cmd // keep reference for liveness check
		// Use ReadString non-blocking with timeout via goroutine and select
		lineCh := make(chan struct {
			s   string
			err error
		}, 1)
		go func() {
			s, err := ow.stdoutReader.ReadString('\n')
			lineCh <- struct {
				s   string
				err error
			}{s, err}
		}()

		select {
		case res := <-lineCh:
			if len(res.s) > 0 {
				b.WriteString(res.s)
				gotAny = true
			}
			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					// Process ended; return what we have
					return strings.TrimSpace(b.String()), nil
				}
				return "", fmt.Errorf("read stdout: %w", res.err)
			}
			// Continue to try reading more until idle
			// Loop continues
		case <-time.After(idleWindow):
			if gotAny {
				return strings.TrimSpace(b.String()), nil
			}
			// No output yet; extend the wait unless context is done
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
				// loop back to try again
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

    // Startup test request
    go func() {
        testInput := "hello, hello, hello, is there anybody in there"
        log.Printf("startup test query: %q", testInput)
        ctx, cancel := context.WithTimeout(rootCtx, 60*time.Second)
        defer cancel()
        resp, err := wrapper.query(ctx, testInput)
        if err != nil {
            log.Printf("startup test query error: %v", err)
            return
        }
        log.Printf("startup test response: %q", resp)
    }()

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
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
        // Log the response obtained from ollama
        log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

- Added response logging for `/query` after retrieving output from Ollama.
- Added a startup self-test: sends "hello, hello, hello, is there anybody in there" to Ollama and logs both the input and the response.
- Rebuilt successfully.

To run and see logs on stdout:
- cd `/workspace/ollama-wrap`
- ./ollama-wrap 2>&1 | cat

---

**User**

there's something wrong with OllamaWrapper, I never get a response from it on startup.. write a unit test that instantiates one and performs that same test request ('hello, hello, hello, is there anybody there?') run the test to ensure that component is working as expected, that we can get resonses from it

---

**Cursor**

I'll add a Go test that starts the wrapper and sends the exact test prompt, then run it to verify we get a response.

```go
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
        model = "gpt-oss"
    }

    // Ensure PATH contains the directory of the binary so any sub-process lookups work consistently
    binDir := filepath.Dir(binary)
    os.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

    // Defensive: ensure we can at least run `ollama --version`
    ctxCheck, cancelCheck := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancelCheck()
    if err := exec.CommandContext(ctxCheck, binary, "--version").Run(); err != nil {
        t.Skipf("ollama binary exists but not runnable: %v", err)
    }

    rootCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
    defer cancel()

    wrapper, err := startOllamaInteractive(rootCtx, binary, model)
    if err != nil {
        t.Fatalf("failed to start ollama interactive: %v", err)
    }

    // Perform the test query
    prompt := "hello, hello, hello, is there anybody there?"
    ctx, c := context.WithTimeout(rootCtx, 90*time.Second)
    defer c()
    resp, err := wrapper.query(ctx, prompt)
    if err != nil {
        t.Fatalf("query failed: %v", err)
    }
    if len(resp) == 0 {
        t.Fatalf("empty response from ollama")
    }
    t.Logf("response: %q", resp)
}


```

Adding non-blocking stdout reading using poll/select to avoid blocking on newline and implementing robust idle-based response detection. Then I'll add the required dependency and run tests in parallel.

```go
<<'PATCH'
*** Begin Patch
*** Update File: /workspace/ollama-wrap/main.go
@@
-import (
-    "bufio"
-    "context"
-    "errors"
-    "fmt"
-    "io"
-    "log"
-    "net/http"
-    "os"
-    "os/exec"
-    "os/signal"
-    "strings"
-    "sync"
-    "syscall"
-    "time"
-)
+import (
+    "bufio"
+    "context"
+    "errors"
+    "fmt"
+    "io"
+    "log"
+    "net/http"
+    "os"
+    "os/exec"
+    "os/signal"
+    "strings"
+    "sync"
+    "syscall"
+    "time"
+
+    "golang.org/x/sys/unix"
+)
@@
 type OllamaWrapper struct {
     cmd            *exec.Cmd
     stdinWriter    io.WriteCloser
-    stdoutReader   *bufio.Reader
+    stdoutReader   *bufio.Reader
+    stdoutFile     *os.File
     stderrReader   *bufio.Reader
     ioMutex        sync.Mutex // serialize access to interactive session
 }
@@
-    stdout, err := cmd.StdoutPipe()
+    stdoutRC, err := cmd.StdoutPipe()
     if err != nil {
         return nil, fmt.Errorf("stdout pipe: %w", err)
     }
@@
-    if err := cmd.Start(); err != nil {
+    if err := cmd.Start(); err != nil {
         return nil, fmt.Errorf("start ollama: %w", err)
     }
 
+    // Extract *os.File from stdout for low-level polling
+    stdoutFile, ok := stdoutRC.(*os.File)
+    if !ok {
+        return nil, errors.New("unexpected stdout type; want *os.File")
+    }
+    // Set non-blocking for polling reads
+    if err := unix.SetNonblock(int(stdoutFile.Fd()), true); err != nil {
+        // Not fatal, but log it; we'll still try blocking reads
+        log.Printf("warning: failed to set nonblocking on stdout: %v", err)
+    }
+
     wrapper := &OllamaWrapper{
         cmd:          cmd,
         stdinWriter:  stdin,
-        stdoutReader: bufio.NewReader(stdout),
+        stdoutReader: bufio.NewReader(stdoutFile),
+        stdoutFile:   stdoutFile,
         stderrReader: bufio.NewReader(stderr),
     }
@@
-func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
+func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
@@
-    // Read response with idle timeout collection
-    var b strings.Builder
-    idleWindow := 400 * time.Millisecond
-    var gotAny bool
-
-    for {
-        // Set read deadline by wrapping with a context deadline
-        _ = ow.cmd // keep reference for liveness check
-        // Use ReadString non-blocking with timeout via goroutine and select
-        lineCh := make(chan struct{
-            s string
-            err error
-        }, 1)
-        go func() {
-            s, err := ow.stdoutReader.ReadString('\n')
-            lineCh <- struct{ s string; err error }{s, err}
-        }()
-
-        select {
-        case res := <-lineCh:
-            if len(res.s) > 0 {
-                b.WriteString(res.s)
-                gotAny = true
-            }
-            if res.err != nil {
-                if errors.Is(res.err, io.EOF) {
-                    // Process ended; return what we have
-                    return strings.TrimSpace(b.String()), nil
-                }
-                return "", fmt.Errorf("read stdout: %w", res.err)
-            }
-            // Continue to try reading more until idle
-            // Loop continues
-        case <-time.After(idleWindow):
-            if gotAny {
-                return strings.TrimSpace(b.String()), nil
-            }
-            // No output yet; extend the wait unless context is done
-            select {
-            case <-ctx.Done():
-                return "", ctx.Err()
-            default:
-                // loop back to try again
-            }
-        case <-ctx.Done():
-            return "", ctx.Err()
-        }
-    }
+    // Read response with idle timeout collection using poll on non-blocking fd
+    var b strings.Builder
+    idleWindow := 600 * time.Millisecond
+    var gotAny bool
+
+    fd := int(ow.stdoutFile.Fd())
+    pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
+    buf := make([]byte, 8192)
+
+    for {
+        // Check context first
+        select {
+        case <-ctx.Done():
+            return "", ctx.Err()
+        default:
+        }
+
+        // Wait for readability up to idleWindow
+        timeoutMs := int(idleWindow / time.Millisecond)
+        n, err := unix.Poll(pollFds, timeoutMs)
+        if err != nil {
+            if err == unix.EINTR {
+                continue
+            }
+            return "", fmt.Errorf("poll stdout: %w", err)
+        }
+        if n == 0 { // timeout
+            if gotAny {
+                return strings.TrimSpace(b.String()), nil
+            }
+            // No output yet; continue waiting unless context is done
+            continue
+        }
+        if pollFds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0 {
+            // Attempt to read as much as available
+            for {
+                readN, rerr := ow.stdoutFile.Read(buf)
+                if readN > 0 {
+                    b.Write(buf[:readN])
+                    gotAny = true
+                    // Keep looping to drain what's available in non-blocking mode
+                    continue
+                }
+                if rerr != nil {
+                    if errors.Is(rerr, unix.EAGAIN) || errors.Is(rerr, syscall.EAGAIN) {
+                        break
+                    }
+                    if errors.Is(rerr, io.EOF) {
+                        return strings.TrimSpace(b.String()), nil
+                    }
+                    return "", fmt.Errorf("read stdout: %w", rerr)
+                }
+                // Nothing read and no error – break to poll again
+                break
+            }
+        }
+    }
 }
*** End Patch
PATCH
```

I’m going to read the current `main.go` to apply the fix correctly, then update the wrapper to use non-blocking reads with polling and re-run tests.

```go
package main

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "strings"
    "sync"
    "syscall"
    "time"

    "golang.org/x/sys/unix"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
	cmd          *exec.Cmd
	stdinWriter  io.WriteCloser
	stdoutReader *bufio.Reader
	stderrReader *bufio.Reader
	ioMutex      sync.Mutex // serialize access to interactive session
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ollama: %w", err)
	}

	wrapper := &OllamaWrapper{
		cmd:          cmd,
		stdinWriter:  stdin,
		stdoutReader: bufio.NewReader(stdout),
		stderrReader: bufio.NewReader(stderr),
	}

	// Optionally read initial banner until ready; best-effort with timeout
	readyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go func() {
		// Drain any initial stderr to logs
		for {
			select {
			case <-readyCtx.Done():
				return
			default:
			}
			line, err := wrapper.stderrReader.ReadString('\n')
			if len(line) > 0 {
				log.Printf("ollama stderr: %s", strings.TrimRight(line, "\n"))
			}
			if err != nil {
				return
			}
		}
	}()

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

	// Write input
	if _, err := io.WriteString(ow.stdinWriter, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

	// Read response with idle timeout collection
	var b strings.Builder
	idleWindow := 400 * time.Millisecond
	var gotAny bool

	for {
		// Set read deadline by wrapping with a context deadline
		_ = ow.cmd // keep reference for liveness check
		// Use ReadString non-blocking with timeout via goroutine and select
		lineCh := make(chan struct {
			s   string
			err error
		}, 1)
		go func() {
			s, err := ow.stdoutReader.ReadString('\n')
			lineCh <- struct {
				s   string
				err error
			}{s, err}
		}()

		select {
		case res := <-lineCh:
			if len(res.s) > 0 {
				b.WriteString(res.s)
				gotAny = true
			}
			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					// Process ended; return what we have
					return strings.TrimSpace(b.String()), nil
				}
				return "", fmt.Errorf("read stdout: %w", res.err)
			}
			// Continue to try reading more until idle
			// Loop continues
		case <-time.After(idleWindow):
			if gotAny {
				return strings.TrimSpace(b.String()), nil
			}
			// No output yet; extend the wait unless context is done
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
				// loop back to try again
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

	// Startup test request
	go func() {
		testInput := "hello, hello, hello, is there anybody in there"
		log.Printf("startup test query: %q", testInput)
		ctx, cancel := context.WithTimeout(rootCtx, 60*time.Second)
		defer cancel()
		resp, err := wrapper.query(ctx, testInput)
		if err != nil {
			log.Printf("startup test query error: %v", err)
			return
		}
		log.Printf("startup test response: %q", resp)
	}()

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
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Log the response obtained from ollama
		log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

```go
package main

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "strings"
    "sync"
    "syscall"
    "time"

    "golang.org/x/sys/unix"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
    cmd          *exec.Cmd
    stdinWriter  io.WriteCloser
    stdoutReader *bufio.Reader
    stdoutFile   *os.File
    stderrReader *bufio.Reader
    ioMutex      sync.Mutex // serialize access to interactive session
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ollama: %w", err)
	}

	wrapper := &OllamaWrapper{
		cmd:          cmd,
		stdinWriter:  stdin,
		stdoutReader: bufio.NewReader(stdout),
		stderrReader: bufio.NewReader(stderr),
	}

	// Optionally read initial banner until ready; best-effort with timeout
	readyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go func() {
		// Drain any initial stderr to logs
		for {
			select {
			case <-readyCtx.Done():
				return
			default:
			}
			line, err := wrapper.stderrReader.ReadString('\n')
			if len(line) > 0 {
				log.Printf("ollama stderr: %s", strings.TrimRight(line, "\n"))
			}
			if err != nil {
				return
			}
		}
	}()

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

	// Write input
	if _, err := io.WriteString(ow.stdinWriter, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

	// Read response with idle timeout collection
	var b strings.Builder
	idleWindow := 400 * time.Millisecond
	var gotAny bool

	for {
		// Set read deadline by wrapping with a context deadline
		_ = ow.cmd // keep reference for liveness check
		// Use ReadString non-blocking with timeout via goroutine and select
		lineCh := make(chan struct {
			s   string
			err error
		}, 1)
		go func() {
			s, err := ow.stdoutReader.ReadString('\n')
			lineCh <- struct {
				s   string
				err error
			}{s, err}
		}()

		select {
		case res := <-lineCh:
			if len(res.s) > 0 {
				b.WriteString(res.s)
				gotAny = true
			}
			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					// Process ended; return what we have
					return strings.TrimSpace(b.String()), nil
				}
				return "", fmt.Errorf("read stdout: %w", res.err)
			}
			// Continue to try reading more until idle
			// Loop continues
		case <-time.After(idleWindow):
			if gotAny {
				return strings.TrimSpace(b.String()), nil
			}
			// No output yet; extend the wait unless context is done
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
				// loop back to try again
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

	// Startup test request
	go func() {
		testInput := "hello, hello, hello, is there anybody in there"
		log.Printf("startup test query: %q", testInput)
		ctx, cancel := context.WithTimeout(rootCtx, 60*time.Second)
		defer cancel()
		resp, err := wrapper.query(ctx, testInput)
		if err != nil {
			log.Printf("startup test query error: %v", err)
			return
		}
		log.Printf("startup test response: %q", resp)
	}()

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
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Log the response obtained from ollama
		log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

```go
package main

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "strings"
    "sync"
    "syscall"
    "time"

    "golang.org/x/sys/unix"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
    cmd          *exec.Cmd
    stdinWriter  io.WriteCloser
    stdoutReader *bufio.Reader
    stdoutFile   *os.File
    stderrReader *bufio.Reader
    ioMutex      sync.Mutex // serialize access to interactive session
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
    stdoutRC, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ollama: %w", err)
	}

    // Extract *os.File for polling
    stdoutFile, ok := stdoutRC.(*os.File)
    if !ok {
        return nil, errors.New("unexpected stdout type; want *os.File")
    }
    // Try to set non-blocking mode for polling
    if err := unix.SetNonblock(int(stdoutFile.Fd()), true); err != nil {
        log.Printf("warning: failed to set nonblocking on stdout: %v", err)
    }

    wrapper := &OllamaWrapper{
        cmd:          cmd,
        stdinWriter:  stdin,
        stdoutReader: bufio.NewReader(stdoutFile),
        stdoutFile:   stdoutFile,
        stderrReader: bufio.NewReader(stderr),
    }

	// Optionally read initial banner until ready; best-effort with timeout
	readyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go func() {
		// Drain any initial stderr to logs
		for {
			select {
			case <-readyCtx.Done():
				return
			default:
			}
			line, err := wrapper.stderrReader.ReadString('\n')
			if len(line) > 0 {
				log.Printf("ollama stderr: %s", strings.TrimRight(line, "\n"))
			}
			if err != nil {
				return
			}
		}
	}()

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

	// Write input
	if _, err := io.WriteString(ow.stdinWriter, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

	// Read response with idle timeout collection
	var b strings.Builder
	idleWindow := 400 * time.Millisecond
	var gotAny bool

	for {
		// Set read deadline by wrapping with a context deadline
		_ = ow.cmd // keep reference for liveness check
		// Use ReadString non-blocking with timeout via goroutine and select
		lineCh := make(chan struct {
			s   string
			err error
		}, 1)
		go func() {
			s, err := ow.stdoutReader.ReadString('\n')
			lineCh <- struct {
				s   string
				err error
			}{s, err}
		}()

		select {
		case res := <-lineCh:
			if len(res.s) > 0 {
				b.WriteString(res.s)
				gotAny = true
			}
			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					// Process ended; return what we have
					return strings.TrimSpace(b.String()), nil
				}
				return "", fmt.Errorf("read stdout: %w", res.err)
			}
			// Continue to try reading more until idle
			// Loop continues
		case <-time.After(idleWindow):
			if gotAny {
				return strings.TrimSpace(b.String()), nil
			}
			// No output yet; extend the wait unless context is done
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
				// loop back to try again
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

	// Startup test request
	go func() {
		testInput := "hello, hello, hello, is there anybody in there"
		log.Printf("startup test query: %q", testInput)
		ctx, cancel := context.WithTimeout(rootCtx, 60*time.Second)
		defer cancel()
		resp, err := wrapper.query(ctx, testInput)
		if err != nil {
			log.Printf("startup test query error: %v", err)
			return
		}
		log.Printf("startup test response: %q", resp)
	}()

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
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Log the response obtained from ollama
		log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

```go
package main

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "strings"
    "sync"
    "syscall"
    "time"

    "golang.org/x/sys/unix"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
    cmd          *exec.Cmd
    stdinWriter  io.WriteCloser
    stdoutReader *bufio.Reader
    stdoutFile   *os.File
    stderrReader *bufio.Reader
    ioMutex      sync.Mutex // serialize access to interactive session
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
    stdoutRC, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ollama: %w", err)
	}

    // Extract *os.File for polling
    stdoutFile, ok := stdoutRC.(*os.File)
    if !ok {
        return nil, errors.New("unexpected stdout type; want *os.File")
    }
    // Try to set non-blocking mode for polling
    if err := unix.SetNonblock(int(stdoutFile.Fd()), true); err != nil {
        log.Printf("warning: failed to set nonblocking on stdout: %v", err)
    }

    wrapper := &OllamaWrapper{
        cmd:          cmd,
        stdinWriter:  stdin,
        stdoutReader: bufio.NewReader(stdoutFile),
        stdoutFile:   stdoutFile,
        stderrReader: bufio.NewReader(stderr),
    }

	// Optionally read initial banner until ready; best-effort with timeout
	readyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go func() {
		// Drain any initial stderr to logs
		for {
			select {
			case <-readyCtx.Done():
				return
			default:
			}
			line, err := wrapper.stderrReader.ReadString('\n')
			if len(line) > 0 {
				log.Printf("ollama stderr: %s", strings.TrimRight(line, "\n"))
			}
			if err != nil {
				return
			}
		}
	}()

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

	// Write input
	if _, err := io.WriteString(ow.stdinWriter, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

    // Read response with idle timeout collection using poll
    var b strings.Builder
    idleWindow := 600 * time.Millisecond
    var gotAny bool
    fd := int(ow.stdoutFile.Fd())
    pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLHUP}}
    buf := make([]byte, 8192)

    for {
        // Respect context cancelation
        select {
        case <-ctx.Done():
            return "", ctx.Err()
        default:
        }

        timeoutMs := int(idleWindow / time.Millisecond)
        n, err := unix.Poll(pollFds, timeoutMs)
        if err != nil {
            if err == unix.EINTR {
                continue
            }
            return "", fmt.Errorf("poll stdout: %w", err)
        }
        if n == 0 {
            if gotAny {
                return strings.TrimSpace(b.String()), nil
            }
            // keep waiting
            continue
        }
        // Try to drain available bytes
        for {
            readN, rerr := ow.stdoutFile.Read(buf)
            if readN > 0 {
                b.Write(buf[:readN])
                gotAny = true
                // continue draining until EAGAIN
                continue
            }
            if rerr != nil {
                if errors.Is(rerr, io.EOF) {
                    return strings.TrimSpace(b.String()), nil
                }
                // When non-blocking, EAGAIN means no more data
                var errno syscall.Errno
                if errors.As(rerr, &errno) && (errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK) {
                    break
                }
                return "", fmt.Errorf("read stdout: %w", rerr)
            }
            break
        }
    }
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

	// Startup test request
	go func() {
		testInput := "hello, hello, hello, is there anybody in there"
		log.Printf("startup test query: %q", testInput)
		ctx, cancel := context.WithTimeout(rootCtx, 60*time.Second)
		defer cancel()
		resp, err := wrapper.query(ctx, testInput)
		if err != nil {
			log.Printf("startup test query error: %v", err)
			return
		}
		log.Printf("startup test response: %q", resp)
	}()

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
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Log the response obtained from ollama
		log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

I'll update the stderr handling to continuously drain it for the lifetime of the Ollama process to avoid blocking on large stderr output (like model downloads), then run the tests again.

```go
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
	cmd          *exec.Cmd
	stdinWriter  io.WriteCloser
	stdoutReader *bufio.Reader
	stdoutFile   *os.File
	stderrReader *bufio.Reader
	ioMutex      sync.Mutex // serialize access to interactive session
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutRC, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ollama: %w", err)
	}

	// Extract *os.File for polling
	stdoutFile, ok := stdoutRC.(*os.File)
	if !ok {
		return nil, errors.New("unexpected stdout type; want *os.File")
	}
	// Try to set non-blocking mode for polling
	if err := unix.SetNonblock(int(stdoutFile.Fd()), true); err != nil {
		log.Printf("warning: failed to set nonblocking on stdout: %v", err)
	}

	wrapper := &OllamaWrapper{
		cmd:          cmd,
		stdinWriter:  stdin,
		stdoutReader: bufio.NewReader(stdoutFile),
		stdoutFile:   stdoutFile,
		stderrReader: bufio.NewReader(stderr),
	}

    // Continuously drain stderr to avoid blocking the child process (large pulls/logs)
    go func() {
        for {
            line, err := wrapper.stderrReader.ReadString('\n')
            if len(line) > 0 {
                log.Printf("ollama stderr: %s", strings.TrimRight(line, "\n"))
            }
            if err != nil {
                return
            }
        }
    }()

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

	// Write input
	if _, err := io.WriteString(ow.stdinWriter, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

	// Read response with idle timeout collection using poll
	var b strings.Builder
	idleWindow := 600 * time.Millisecond
	var gotAny bool
	fd := int(ow.stdoutFile.Fd())
	pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLHUP}}
	buf := make([]byte, 8192)

	for {
		// Respect context cancelation
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		timeoutMs := int(idleWindow / time.Millisecond)
		n, err := unix.Poll(pollFds, timeoutMs)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return "", fmt.Errorf("poll stdout: %w", err)
		}
		if n == 0 {
			if gotAny {
				return strings.TrimSpace(b.String()), nil
			}
			// keep waiting
			continue
		}
		// Try to drain available bytes
		for {
			readN, rerr := ow.stdoutFile.Read(buf)
			if readN > 0 {
				b.Write(buf[:readN])
				gotAny = true
				// continue draining until EAGAIN
				continue
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					return strings.TrimSpace(b.String()), nil
				}
				// When non-blocking, EAGAIN means no more data
				var errno syscall.Errno
				if errors.As(rerr, &errno) && (errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK) {
					break
				}
				return "", fmt.Errorf("read stdout: %w", rerr)
			}
			break
		}
	}
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

	// Startup test request
	go func() {
		testInput := "hello, hello, hello, is there anybody in there"
		log.Printf("startup test query: %q", testInput)
		ctx, cancel := context.WithTimeout(rootCtx, 60*time.Second)
		defer cancel()
		resp, err := wrapper.query(ctx, testInput)
		if err != nil {
			log.Printf("startup test query error: %v", err)
			return
		}
		log.Printf("startup test response: %q", resp)
	}()

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
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Log the response obtained from ollama
		log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

---

**User**

what's the command to run the unit test?

---

**Cursor**

- From the project dir:
  ```bash
  cd /workspace/ollama-wrap
  go test -v
  ```
- If you need to specify the binary/model explicitly:
  ```bash
  cd /workspace/ollama-wrap
  OLLAMA_BIN=/usr/local/bin/ollama OLLAMA_MODEL=gpt-oss go test -v -run TestOllamaWrapper_Query
  ```

- To allow longer runs (optional):
  ```bash
  go test -v -timeout 10m
  ```

```shellscript
  cd /workspace/ollama-wrap
  go test -v
```

```shellscript
  cd /workspace/ollama-wrap
  OLLAMA_BIN=/usr/local/bin/ollama OLLAMA_MODEL=gpt-oss go test -v -run TestOllamaWrapper_Query
```

```shellscript
  go test -v -timeout 10m
```

---

**User**

make it so startOllamaInteractive logs the cmd its going to run to start ollama

---

**Cursor**

```go
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
	cmd          *exec.Cmd
	stdinWriter  io.WriteCloser
	stdoutReader *bufio.Reader
	stdoutFile   *os.File
	stderrReader *bufio.Reader
	ioMutex      sync.Mutex // serialize access to interactive session
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination
    log.Printf("starting ollama: %s", strings.Join(cmd.Args, " "))

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutRC, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ollama: %w", err)
	}

	// Extract *os.File for polling
	stdoutFile, ok := stdoutRC.(*os.File)
	if !ok {
		return nil, errors.New("unexpected stdout type; want *os.File")
	}
	// Try to set non-blocking mode for polling
	if err := unix.SetNonblock(int(stdoutFile.Fd()), true); err != nil {
		log.Printf("warning: failed to set nonblocking on stdout: %v", err)
	}

	wrapper := &OllamaWrapper{
		cmd:          cmd,
		stdinWriter:  stdin,
		stdoutReader: bufio.NewReader(stdoutFile),
		stdoutFile:   stdoutFile,
		stderrReader: bufio.NewReader(stderr),
	}

	// Continuously drain stderr to avoid blocking the child process (large pulls/logs)
	go func() {
		for {
			line, err := wrapper.stderrReader.ReadString('\n')
			if len(line) > 0 {
				log.Printf("ollama stderr: %s", strings.TrimRight(line, "\n"))
			}
			if err != nil {
				return
			}
		}
	}()

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

	// Write input
	if _, err := io.WriteString(ow.stdinWriter, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

	// Read response with idle timeout collection using poll
	var b strings.Builder
	idleWindow := 600 * time.Millisecond
	var gotAny bool
	fd := int(ow.stdoutFile.Fd())
	pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLHUP}}
	buf := make([]byte, 8192)

	for {
		// Respect context cancelation
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		timeoutMs := int(idleWindow / time.Millisecond)
		n, err := unix.Poll(pollFds, timeoutMs)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return "", fmt.Errorf("poll stdout: %w", err)
		}
		if n == 0 {
			if gotAny {
				return strings.TrimSpace(b.String()), nil
			}
			// keep waiting
			continue
		}
		// Try to drain available bytes
		for {
			readN, rerr := ow.stdoutFile.Read(buf)
			if readN > 0 {
				b.Write(buf[:readN])
				gotAny = true
				// continue draining until EAGAIN
				continue
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					return strings.TrimSpace(b.String()), nil
				}
				// When non-blocking, EAGAIN means no more data
				var errno syscall.Errno
				if errors.As(rerr, &errno) && (errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK) {
					break
				}
				return "", fmt.Errorf("read stdout: %w", rerr)
			}
			break
		}
	}
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

	// Startup test request
	go func() {
		testInput := "hello, hello, hello, is there anybody in there"
		log.Printf("startup test query: %q", testInput)
		ctx, cancel := context.WithTimeout(rootCtx, 60*time.Second)
		defer cancel()
		resp, err := wrapper.query(ctx, testInput)
		if err != nil {
			log.Printf("startup test query error: %v", err)
			return
		}
		log.Printf("startup test response: %q", resp)
	}()

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
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Log the response obtained from ollama
		log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

- Added a log line in `startOllamaInteractive` to print the exact command being executed:
  - Example: `starting ollama: /usr/local/bin/ollama run gpt-oss`

Rebuild and run:
- cd `/workspace/ollama-wrap`
- go build -o ollama-wrap
- ./ollama-wrap 2>&1 | cat

---

**User**

make sure that's also logged by the unit test.. how do I see the log when I run the unit test?

---

**Cursor**

```go
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
		model = "gpt-oss"
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

	rootCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		t.Fatalf("failed to start ollama interactive: %v", err)
	}

	// Perform the test query
	prompt := "hello, hello, hello, is there anybody there?"
	ctx, c := context.WithTimeout(rootCtx, 90*time.Second)
	defer c()
	resp, err := wrapper.query(ctx, prompt)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(resp) == 0 {
		t.Fatalf("empty response from ollama")
	}
	t.Logf("response: %q", resp)
}

```

---

**User**

here, I run the test manually on shell and this is what I get, including all output from ollama. With this knowledge and the unit test, iterate until you are able to get output from ollama wrapper. Here's what I got  on shell: /usr/local/bin/ollama run gpt-oss
>>> hello, hello, hello, is there anybody there?
Thinking...
The user says "hello, hello, hello, is there anybody there?" It's a prompt likely referencing the Beatles' "Hey Jude" or "Is there anybody out there?" Actually it's "hello, hello, hello, is there anybody there?" Possibly referencing the intro to 
"Help! The Beatles" or "Hey! I'm...". They might just want a greeting or they might be referencing a song. The user is likely greeting the assistant. They want an answer. So respond politely. Also could refer to something else? The phrase "Hello, 
Hello, Hello, is there anybody out there?" is an iconic line from "The Beatles" maybe. But we can simply greet them back.

We should ask if they want to talk about the Beatles. Or maybe they're quoting "Hello, hello, hello, is there anybody there?" from the opening of "The Beatles: Help!" Wait, the phrase is actually from "The Beatles" in "Help! The Beatles"? Let's 
check: The line "Hello, Hello, hello, is there anybody out there?" is the opening of "Help!" I recall the line "Hello, hello, hello, is there anybody out there?" appears in the opening of "Help! The Beatles" or "Help! (Song)". Actually "Help! The 
Beatles" starts with "Hello, hello, hello, is there anybody out there?" It's a Beatles reference. So we could respond referencing that. The user might just be greeting.

Thus answer: Hello! Yes, I'm here. What can I help you with? And maybe reference that line. Provide a playful response.

Also consider possible confusion: The user might be referencing a song lyric. The user might be messing with us. So respond accordingly.

Hence respond with friendly tone.
...done thinking.

Hey there! 👋 Yes, I’m here and ready to chat.  
(If you’re dropping that famous Beatles line—“Hello, hello, hello, is there anybody out there?”—I’m definitely listening.)

What’s on your mind today?

>>> Send a message (/? for help)


---

**Cursor**

```go
package main

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "strings"
    "sync"
    "syscall"
    "time"

    "github.com/creack/pty"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
    cmd          *exec.Cmd
    tty          *os.File      // PTY providing combined stdin/stdout
    reader       *bufio.Reader // buffered reader over PTY
    stderrReader *bufio.Reader
    ioMutex      sync.Mutex // serialize access to interactive session
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
    cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination
	log.Printf("starting ollama: %s", strings.Join(cmd.Args, " "))

    // Start under a PTY for proper interactive behavior
    tty, err := pty.Start(cmd)
	if err != nil {
        return nil, fmt.Errorf("start pty: %w", err)
	}
    // Separate stderr to avoid blocking
    stderr, err := cmd.StderrPipe()
    if err != nil {
        return nil, fmt.Errorf("stderr pipe: %w", err)
    }

	wrapper := &OllamaWrapper{
		cmd:          cmd,
        tty:          tty,
        reader:       bufio.NewReader(tty),
        stderrReader: bufio.NewReader(stderr),
	}

	// Continuously drain stderr to avoid blocking the child process (large pulls/logs)
	go func() {
		for {
			line, err := wrapper.stderrReader.ReadString('\n')
			if len(line) > 0 {
				log.Printf("ollama stderr: %s", strings.TrimRight(line, "\n"))
			}
			if err != nil {
				return
			}
		}
	}()

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

	// Write input
    if _, err := io.WriteString(ow.tty, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

    // Read lines until we hit the next interactive prompt line beginning with ">>>"
    // We skip prompt echo lines and return all content between input and the next prompt.
    var b strings.Builder
    sawContent := false

    lineCh := make(chan struct{
        s string
        err error
    }, 1)

    readLine := func() {
        s, err := ow.reader.ReadString('\n')
        lineCh <- struct{ s string; err error }{s, err}
    }

    for {
        go readLine()
        select {
        case res := <-lineCh:
            if len(res.s) > 0 {
                // Detect prompt lines
                trimmed := strings.TrimRight(res.s, "\r\n")
                if strings.HasPrefix(trimmed, ">>>") {
                    if sawContent {
                        return strings.TrimSpace(b.String()), nil
                    }
                    // skip initial/echo prompt and continue reading
                    continue
                }
                // Accumulate non-prompt output
                if len(strings.TrimSpace(trimmed)) > 0 {
                    sawContent = true
                }
                b.WriteString(res.s)
            }
            if res.err != nil {
                if errors.Is(res.err, io.EOF) {
                    return strings.TrimSpace(b.String()), nil
                }
                return "", fmt.Errorf("read from pty: %w", res.err)
            }
        case <-ctx.Done():
            return "", ctx.Err()
        }
    }
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

	// Startup test request
	go func() {
		testInput := "hello, hello, hello, is there anybody in there"
		log.Printf("startup test query: %q", testInput)
		ctx, cancel := context.WithTimeout(rootCtx, 60*time.Second)
		defer cancel()
		resp, err := wrapper.query(ctx, testInput)
		if err != nil {
			log.Printf("startup test query error: %v", err)
			return
		}
		log.Printf("startup test response: %q", resp)
	}()

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
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Log the response obtained from ollama
		log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

```go
package main

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "strings"
    "sync"
    "syscall"
    "time"

    "github.com/creack/pty"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
    cmd     *exec.Cmd
    tty     *os.File      // PTY providing combined stdin/stdout/stderr
    reader  *bufio.Reader // buffered reader over PTY
    ioMutex sync.Mutex    // serialize access to interactive session
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
    cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination
	log.Printf("starting ollama: %s", strings.Join(cmd.Args, " "))

    // Start under a PTY for proper interactive behavior (combines stdout & stderr)
    tty, err := pty.Start(cmd)
	if err != nil {
        return nil, fmt.Errorf("start pty: %w", err)
	}

	wrapper := &OllamaWrapper{
        cmd:    cmd,
        tty:    tty,
        reader: bufio.NewReader(tty),
	}

    // No separate stderr drain needed; PTY merges stdout/stderr

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

	// Write input
    if _, err := io.WriteString(ow.tty, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

    // Read lines until we hit the next interactive prompt line beginning with ">>>"
    // We skip prompt echo lines and return all content between input and the next prompt.
    var b strings.Builder
    sawContent := false

    lineCh := make(chan struct{
        s string
        err error
    }, 1)

    readLine := func() {
        s, err := ow.reader.ReadString('\n')
        lineCh <- struct{ s string; err error }{s, err}
    }

    for {
        go readLine()
        select {
        case res := <-lineCh:
            if len(res.s) > 0 {
                // Detect prompt lines
                trimmed := strings.TrimRight(res.s, "\r\n")
                if strings.HasPrefix(trimmed, ">>>") {
                    if sawContent {
                        return strings.TrimSpace(b.String()), nil
                    }
                    // skip initial/echo prompt and continue reading
                    continue
                }
                // Accumulate non-prompt output
                if len(strings.TrimSpace(trimmed)) > 0 {
                    sawContent = true
                }
                b.WriteString(res.s)
            }
            if res.err != nil {
                if errors.Is(res.err, io.EOF) {
                    return strings.TrimSpace(b.String()), nil
                }
                return "", fmt.Errorf("read from pty: %w", res.err)
            }
        case <-ctx.Done():
            return "", ctx.Err()
        }
    }
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

	// Startup test request
	go func() {
		testInput := "hello, hello, hello, is there anybody in there"
		log.Printf("startup test query: %q", testInput)
		ctx, cancel := context.WithTimeout(rootCtx, 60*time.Second)
		defer cancel()
		resp, err := wrapper.query(ctx, testInput)
		if err != nil {
			log.Printf("startup test query error: %v", err)
			return
		}
		log.Printf("startup test response: %q", resp)
	}()

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
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Log the response obtained from ollama
		log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

```go
package main

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "strings"
    "sync"
    "syscall"
    "time"

    "github.com/creack/pty"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
    cmd     *exec.Cmd
    tty     *os.File      // PTY providing combined stdin/stdout/stderr
    reader  *bufio.Reader // buffered reader over PTY
    ioMutex sync.Mutex    // serialize access to interactive session
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
    cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination
	log.Printf("starting ollama: %s", strings.Join(cmd.Args, " "))

    // Start under a PTY for proper interactive behavior (combines stdout & stderr)
    tty, err := pty.Start(cmd)
	if err != nil {
        return nil, fmt.Errorf("start pty: %w", err)
	}
    // Set non-blocking mode on PTY so reads can use idle window heuristics
    if err := syscall.SetNonblock(int(tty.Fd()), true); err != nil {
        log.Printf("warning: failed to set nonblocking on pty: %v", err)
    }

	wrapper := &OllamaWrapper{
        cmd:    cmd,
        tty:    tty,
        reader: bufio.NewReader(tty),
	}

    // No separate stderr drain needed; PTY merges stdout/stderr

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

    // Drain any pending output before sending the new input (e.g., initial prompts)
    _ = ow.drainPending()

    // Write input
    if _, err := io.WriteString(ow.tty, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

    // Read bytes until idle
    var b strings.Builder
    buf := make([]byte, 8192)
    idleWindow := 1 * time.Second
    lastRead := time.Now()
    var gotAny bool

    for {
        // Check for context cancel
        select {
        case <-ctx.Done():
            return "", ctx.Err()
        default:
        }
        n, err := ow.tty.Read(buf)
        if n > 0 {
            b.Write(buf[:n])
            gotAny = true
            lastRead = time.Now()
            continue
        }
        if err != nil {
            // In non-blocking mode, EAGAIN/EWOULDBLOCK just means no data
            var errno syscall.Errno
            if errors.As(err, &errno) && (errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK) {
                if gotAny && time.Since(lastRead) >= idleWindow {
                    out := strings.TrimSpace(b.String())
                    out = cleanInteractiveOutput(out)
                    return out, nil
                }
                // Briefly yield
                time.Sleep(50 * time.Millisecond)
                continue
            }
            if errors.Is(err, io.EOF) {
                out := strings.TrimSpace(b.String())
                out = cleanInteractiveOutput(out)
                return out, nil
            }
            return "", fmt.Errorf("read pty: %w", err)
        }
        // No bytes and no error shouldn't happen; small sleep to avoid spin
        if gotAny && time.Since(lastRead) >= idleWindow {
            out := strings.TrimSpace(b.String())
            out = cleanInteractiveOutput(out)
            return out, nil
        }
        time.Sleep(25 * time.Millisecond)
    }
}

// drainPending attempts to drain any available bytes from the PTY without blocking
func (ow *OllamaWrapper) drainPending() error {
    tmp := make([]byte, 8192)
    for {
        n, err := ow.tty.Read(tmp)
        if n == 0 {
            if err == nil {
                return nil
            }
            var errno syscall.Errno
            if errors.As(err, &errno) && (errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK) {
                return nil
            }
            if errors.Is(err, io.EOF) {
                return nil
            }
            return err
        }
        // If we read something, loop to try to drain more until EAGAIN
        if err != nil {
            return nil
        }
    }
}

// cleanInteractiveOutput removes prompt lines like ">>> ..." from the captured output
func cleanInteractiveOutput(s string) string {
    if s == "" {
        return s
    }
    lines := strings.Split(s, "\n")
    filtered := make([]string, 0, len(lines))
    for _, ln := range lines {
        t := strings.TrimRight(ln, "\r")
        if strings.HasPrefix(strings.TrimSpace(t), ">>>") {
            continue
        }
        filtered = append(filtered, ln)
    }
    out := strings.TrimSpace(strings.Join(filtered, "\n"))
    if out != "" {
        return out
    }
    return strings.TrimSpace(s)
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

	// Startup test request
	go func() {
		testInput := "hello, hello, hello, is there anybody in there"
		log.Printf("startup test query: %q", testInput)
		ctx, cancel := context.WithTimeout(rootCtx, 60*time.Second)
		defer cancel()
		resp, err := wrapper.query(ctx, testInput)
		if err != nil {
			log.Printf("startup test query error: %v", err)
			return
		}
		log.Printf("startup test response: %q", resp)
	}()

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
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Log the response obtained from ollama
		log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

```go
package main

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "strings"
    "sync"
    "syscall"
    "time"

    "github.com/creack/pty"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
    cmd     *exec.Cmd
    tty     *os.File      // PTY providing combined stdin/stdout/stderr
    reader  *bufio.Reader // buffered reader over PTY
    ioMutex sync.Mutex    // serialize access to interactive session

    ready    bool
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
    cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination
	log.Printf("starting ollama: %s", strings.Join(cmd.Args, " "))

    // Start under a PTY for proper interactive behavior (combines stdout & stderr)
    tty, err := pty.Start(cmd)
	if err != nil {
        return nil, fmt.Errorf("start pty: %w", err)
	}
    // Set non-blocking mode on PTY so reads can use idle window heuristics
    if err := syscall.SetNonblock(int(tty.Fd()), true); err != nil {
        log.Printf("warning: failed to set nonblocking on pty: %v", err)
    }

	wrapper := &OllamaWrapper{
        cmd:    cmd,
        tty:    tty,
        reader: bufio.NewReader(tty),
	}

    // Wait until we see the initial prompt (e.g., lines starting with ">>>")
    if err := wrapper.waitForPrompt(ctx, 45*time.Second); err != nil {
        return nil, fmt.Errorf("waiting for prompt: %w")
    }

    // No separate stderr drain needed; PTY merges stdout/stderr

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

    // Ensure an interactive prompt is ready
    if !ow.ready {
        if err := ow.waitForPrompt(ctx, 60*time.Second); err != nil {
            return "", err
        }
    }
    // Drain any pending output before sending the new input
    _ = ow.drainPending()

    // Write input (CRLF to mimic Enter)
    if !strings.HasSuffix(userInput, "\r\n") {
        userInput = strings.TrimRight(userInput, "\r\n") + "\r\n"
    }
    log.Printf("sending to ollama: %q", strings.TrimRight(userInput, "\r\n"))
    if _, err := io.WriteString(ow.tty, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

    // Read bytes until idle
    var b strings.Builder
    buf := make([]byte, 8192)
    idleWindow := 1 * time.Second
    lastRead := time.Now()
    var gotAny bool

    for {
        // Check for context cancel
        select {
        case <-ctx.Done():
            return "", ctx.Err()
        default:
        }
        n, err := ow.tty.Read(buf)
        if n > 0 {
            chunk := string(buf[:n])
            b.WriteString(chunk)
            // Debug echo
            // log.Printf("ollama out: %q", strings.TrimSpace(chunk))
            gotAny = true
            lastRead = time.Now()
            continue
        }
        if err != nil {
            // In non-blocking mode, EAGAIN/EWOULDBLOCK just means no data
            var errno syscall.Errno
            if errors.As(err, &errno) && (errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK) {
                if gotAny && time.Since(lastRead) >= idleWindow {
                    out := strings.TrimSpace(b.String())
                    out = cleanInteractiveOutput(out)
                    return out, nil
                }
                // Briefly yield
                time.Sleep(50 * time.Millisecond)
                continue
            }
            if errors.Is(err, io.EOF) {
                out := strings.TrimSpace(b.String())
                out = cleanInteractiveOutput(out)
                return out, nil
            }
            return "", fmt.Errorf("read pty: %w", err)
        }
        // No bytes and no error shouldn't happen; small sleep to avoid spin
        if gotAny && time.Since(lastRead) >= idleWindow {
            out := strings.TrimSpace(b.String())
            out = cleanInteractiveOutput(out)
            return out, nil
        }
        time.Sleep(25 * time.Millisecond)
    }
}

// drainPending attempts to drain any available bytes from the PTY without blocking
func (ow *OllamaWrapper) drainPending() error {
    tmp := make([]byte, 8192)
    for {
        n, err := ow.tty.Read(tmp)
        if n == 0 {
            if err == nil {
                return nil
            }
            var errno syscall.Errno
            if errors.As(err, &errno) && (errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK) {
                return nil
            }
            if errors.Is(err, io.EOF) {
                return nil
            }
            return err
        }
        // If we read something, loop to try to drain more until EAGAIN
        if err != nil {
            return nil
        }
    }
}

// cleanInteractiveOutput removes prompt lines like ">>> ..." from the captured output
func cleanInteractiveOutput(s string) string {
    if s == "" {
        return s
    }
    lines := strings.Split(s, "\n")
    filtered := make([]string, 0, len(lines))
    for _, ln := range lines {
        t := strings.TrimRight(ln, "\r")
        if strings.HasPrefix(strings.TrimSpace(t), ">>>") {
            continue
        }
        filtered = append(filtered, ln)
    }
    out := strings.TrimSpace(strings.Join(filtered, "\n"))
    if out != "" {
        return out
    }
    return strings.TrimSpace(s)
}

// waitForPrompt consumes output until a line beginning with ">>>" appears
func (ow *OllamaWrapper) waitForPrompt(ctx context.Context, maxWait time.Duration) error {
    deadline := time.Now().Add(maxWait)
    buf := make([]byte, 4096)
    var acc strings.Builder
    for {
        if time.Now().After(deadline) {
            return fmt.Errorf("prompt not detected within %s", maxWait)
        }
        // Non-blocking read attempts
        n, err := ow.tty.Read(buf)
        if n > 0 {
            acc.Write(buf[:n])
            text := acc.String()
            // Check for prompt marker on any line
            lines := strings.Split(text, "\n")
            for _, ln := range lines {
                if strings.HasPrefix(strings.TrimSpace(ln), ">>>") {
                    ow.ready = true
                    return nil
                }
            }
            // Keep reading
            continue
        }
        if err != nil {
            var errno syscall.Errno
            if errors.As(err, &errno) && (errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK) {
                // No data yet
                select {
                case <-ctx.Done():
                    return ctx.Err()
                default:
                    time.Sleep(50 * time.Millisecond)
                    continue
                }
            }
            if errors.Is(err, io.EOF) {
                return io.EOF
            }
            return err
        }
        // No data, brief wait
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
            time.Sleep(50 * time.Millisecond)
        }
    }
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

	// Startup test request
	go func() {
		testInput := "hello, hello, hello, is there anybody in there"
		log.Printf("startup test query: %q", testInput)
		ctx, cancel := context.WithTimeout(rootCtx, 60*time.Second)
		defer cancel()
		resp, err := wrapper.query(ctx, testInput)
		if err != nil {
			log.Printf("startup test query error: %v", err)
			return
		}
		log.Printf("startup test response: %q", resp)
	}()

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
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Log the response obtained from ollama
		log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

```go
package main

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "strings"
    "sync"
    "syscall"
    "time"

    "github.com/creack/pty"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
    cmd     *exec.Cmd
    tty     *os.File      // PTY providing combined stdin/stdout/stderr
    reader  *bufio.Reader // buffered reader over PTY
    ioMutex sync.Mutex    // serialize access to interactive session

    ready    bool
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
    cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination
	log.Printf("starting ollama: %s", strings.Join(cmd.Args, " "))

    // Start under a PTY for proper interactive behavior (combines stdout & stderr)
    tty, err := pty.Start(cmd)
	if err != nil {
        return nil, fmt.Errorf("start pty: %w", err)
	}
    // Set non-blocking mode on PTY so reads can use idle window heuristics
    if err := syscall.SetNonblock(int(tty.Fd()), true); err != nil {
        log.Printf("warning: failed to set nonblocking on pty: %v", err)
    }

	wrapper := &OllamaWrapper{
        cmd:    cmd,
        tty:    tty,
        reader: bufio.NewReader(tty),
	}

    // Wait until we see the initial prompt (e.g., lines starting with ">>>")
    if err := wrapper.waitForPrompt(ctx, 45*time.Second); err != nil {
        return nil, fmt.Errorf("waiting for prompt: %w", err)
    }

    // No separate stderr drain needed; PTY merges stdout/stderr

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

    // Ensure an interactive prompt is ready
    if !ow.ready {
        if err := ow.waitForPrompt(ctx, 60*time.Second); err != nil {
            return "", err
        }
    }
    // Drain any pending output before sending the new input
    _ = ow.drainPending()

    // Write input (CRLF to mimic Enter)
    if !strings.HasSuffix(userInput, "\r\n") {
        userInput = strings.TrimRight(userInput, "\r\n") + "\r\n"
    }
    log.Printf("sending to ollama: %q", strings.TrimRight(userInput, "\r\n"))
    if _, err := io.WriteString(ow.tty, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

    // Read bytes until idle
    var b strings.Builder
    buf := make([]byte, 8192)
    idleWindow := 1 * time.Second
    lastRead := time.Now()
    var gotAny bool

    for {
        // Check for context cancel
        select {
        case <-ctx.Done():
            return "", ctx.Err()
        default:
        }
        n, err := ow.tty.Read(buf)
        if n > 0 {
            chunk := string(buf[:n])
            b.WriteString(chunk)
            // Debug echo
            // log.Printf("ollama out: %q", strings.TrimSpace(chunk))
            gotAny = true
            lastRead = time.Now()
            continue
        }
        if err != nil {
            // In non-blocking mode, EAGAIN/EWOULDBLOCK just means no data
            var errno syscall.Errno
            if errors.As(err, &errno) && (errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK) {
                if gotAny && time.Since(lastRead) >= idleWindow {
                    out := strings.TrimSpace(b.String())
                    out = cleanInteractiveOutput(out)
                    return out, nil
                }
                // Briefly yield
                time.Sleep(50 * time.Millisecond)
                continue
            }
            if errors.Is(err, io.EOF) {
                out := strings.TrimSpace(b.String())
                out = cleanInteractiveOutput(out)
                return out, nil
            }
            return "", fmt.Errorf("read pty: %w", err)
        }
        // No bytes and no error shouldn't happen; small sleep to avoid spin
        if gotAny && time.Since(lastRead) >= idleWindow {
            out := strings.TrimSpace(b.String())
            out = cleanInteractiveOutput(out)
            return out, nil
        }
        time.Sleep(25 * time.Millisecond)
    }
}

// drainPending attempts to drain any available bytes from the PTY without blocking
func (ow *OllamaWrapper) drainPending() error {
    tmp := make([]byte, 8192)
    for {
        n, err := ow.tty.Read(tmp)
        if n == 0 {
            if err == nil {
                return nil
            }
            var errno syscall.Errno
            if errors.As(err, &errno) && (errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK) {
                return nil
            }
            if errors.Is(err, io.EOF) {
                return nil
            }
            return err
        }
        // If we read something, loop to try to drain more until EAGAIN
        if err != nil {
            return nil
        }
    }
}

// cleanInteractiveOutput removes prompt lines like ">>> ..." from the captured output
func cleanInteractiveOutput(s string) string {
    if s == "" {
        return s
    }
    lines := strings.Split(s, "\n")
    filtered := make([]string, 0, len(lines))
    for _, ln := range lines {
        t := strings.TrimRight(ln, "\r")
        if strings.HasPrefix(strings.TrimSpace(t), ">>>") {
            continue
        }
        filtered = append(filtered, ln)
    }
    out := strings.TrimSpace(strings.Join(filtered, "\n"))
    if out != "" {
        return out
    }
    return strings.TrimSpace(s)
}

// waitForPrompt consumes output until a line beginning with ">>>" appears
func (ow *OllamaWrapper) waitForPrompt(ctx context.Context, maxWait time.Duration) error {
    deadline := time.Now().Add(maxWait)
    buf := make([]byte, 4096)
    var acc strings.Builder
    for {
        if time.Now().After(deadline) {
            return fmt.Errorf("prompt not detected within %s", maxWait)
        }
        // Non-blocking read attempts
        n, err := ow.tty.Read(buf)
        if n > 0 {
            acc.Write(buf[:n])
            text := acc.String()
            // Check for prompt marker on any line
            lines := strings.Split(text, "\n")
            for _, ln := range lines {
                if strings.HasPrefix(strings.TrimSpace(ln), ">>>") {
                    ow.ready = true
                    return nil
                }
            }
            // Keep reading
            continue
        }
        if err != nil {
            var errno syscall.Errno
            if errors.As(err, &errno) && (errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK) {
                // No data yet
                select {
                case <-ctx.Done():
                    return ctx.Err()
                default:
                    time.Sleep(50 * time.Millisecond)
                    continue
                }
            }
            if errors.Is(err, io.EOF) {
                return io.EOF
            }
            return err
        }
        // No data, brief wait
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
            time.Sleep(50 * time.Millisecond)
        }
    }
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

	// Startup test request
	go func() {
		testInput := "hello, hello, hello, is there anybody in there"
		log.Printf("startup test query: %q", testInput)
		ctx, cancel := context.WithTimeout(rootCtx, 60*time.Second)
		defer cancel()
		resp, err := wrapper.query(ctx, testInput)
		if err != nil {
			log.Printf("startup test query error: %v", err)
			return
		}
		log.Printf("startup test response: %q", resp)
	}()

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
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Log the response obtained from ollama
		log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

```go
package main

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "strings"
    "sync"
    "syscall"
    "time"

    "github.com/creack/pty"
)

// OllamaWrapper manages a single interactive ollama process
type OllamaWrapper struct {
    cmd     *exec.Cmd
    tty     *os.File      // PTY providing combined stdin/stdout/stderr
    reader  *bufio.Reader // buffered reader over PTY
    ioMutex sync.Mutex    // serialize access to interactive session

    ready    bool
}

func startOllamaInteractive(ctx context.Context, binaryPath, model string) (*OllamaWrapper, error) {
    cmd := exec.CommandContext(ctx, binaryPath, "run", model)
	// Create a pseudo-terminal-like environment by disabling stdin buffering issues
	// We'll rely on newline termination
	log.Printf("starting ollama: %s", strings.Join(cmd.Args, " "))

    // Start under a PTY for proper interactive behavior (combines stdout & stderr)
    tty, err := pty.Start(cmd)
	if err != nil {
        return nil, fmt.Errorf("start pty: %w", err)
	}
    // Use blocking reads on the PTY; we'll layer timeouts via context and goroutines

	wrapper := &OllamaWrapper{
        cmd:    cmd,
        tty:    tty,
        reader: bufio.NewReader(tty),
	}

    // Wait until we see the initial prompt (e.g., lines starting with ">>>")
    if err := wrapper.waitForPrompt(ctx, 45*time.Second); err != nil {
        return nil, fmt.Errorf("waiting for prompt: %w", err)
    }

    // No separate stderr drain needed; PTY merges stdout/stderr

	return wrapper, nil
}

// query sends a single line to the interactive process and reads a response.
// Since ollama interactive does not emit a clear end marker via stdout, we use a short idle timeout heuristic:
// read until no bytes arrive for idleWindow after at least some output was received.
func (ow *OllamaWrapper) query(ctx context.Context, userInput string) (string, error) {
	ow.ioMutex.Lock()
	defer ow.ioMutex.Unlock()

	if ow.cmd.ProcessState != nil && ow.cmd.ProcessState.Exited() {
		return "", errors.New("ollama process not running")
	}

	// Ensure input ends with a newline
	if !strings.HasSuffix(userInput, "\n") {
		userInput += "\n"
	}

    // Ensure an interactive prompt is ready
    if !ow.ready {
        if err := ow.waitForPrompt(ctx, 60*time.Second); err != nil {
            return "", err
        }
    }

    // Write input (CRLF to mimic Enter)
    if !strings.HasSuffix(userInput, "\r\n") {
        userInput = strings.TrimRight(userInput, "\r\n") + "\r\n"
    }
    log.Printf("sending to ollama: %q", strings.TrimRight(userInput, "\r\n"))
    if _, err := io.WriteString(ow.tty, userInput); err != nil {
		return "", fmt.Errorf("write stdin: %w", err)
	}

    // Read lines until next prompt line appears
    var b strings.Builder
    sawContent := false
    for {
        line, err := ow.readLineWithCtx(ctx)
        if len(line) > 0 {
            trimmed := strings.TrimRight(line, "\r\n")
            if strings.HasPrefix(strings.TrimSpace(trimmed), ">>>") {
                if sawContent {
                    out := strings.TrimSpace(b.String())
                    out = cleanInteractiveOutput(out)
                    return out, nil
                }
                // skip initial prompt line if no content yet
                continue
            }
            if strings.TrimSpace(trimmed) != "" {
                sawContent = true
            }
            b.WriteString(line)
        }
        if err != nil {
            if errors.Is(err, io.EOF) {
                out := strings.TrimSpace(b.String())
                out = cleanInteractiveOutput(out)
                return out, nil
            }
            return "", fmt.Errorf("read from pty: %w", err)
        }
    }
}

// drainPending attempts to drain any available bytes from the PTY without blocking
func (ow *OllamaWrapper) drainPending() error {
    // Best-effort: read a few lines quickly with short timeouts
    ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
    defer cancel()
    for {
        line, err := ow.readLineWithCtx(ctx)
        _ = line
        if err != nil {
            return nil
        }
    }
}

// cleanInteractiveOutput removes prompt lines like ">>> ..." from the captured output
func cleanInteractiveOutput(s string) string {
    if s == "" {
        return s
    }
    lines := strings.Split(s, "\n")
    filtered := make([]string, 0, len(lines))
    for _, ln := range lines {
        t := strings.TrimRight(ln, "\r")
        if strings.HasPrefix(strings.TrimSpace(t), ">>>") {
            continue
        }
        filtered = append(filtered, ln)
    }
    out := strings.TrimSpace(strings.Join(filtered, "\n"))
    if out != "" {
        return out
    }
    return strings.TrimSpace(s)
}

// waitForPrompt consumes output until a line beginning with ">>>" appears
func (ow *OllamaWrapper) waitForPrompt(ctx context.Context, maxWait time.Duration) error {
    deadline := time.Now().Add(maxWait)
    for time.Now().Before(deadline) {
        line, err := ow.readLineWithCtx(ctx)
        if len(line) > 0 {
            if strings.HasPrefix(strings.TrimSpace(line), ">>>") {
                ow.ready = true
                return nil
            }
        }
        if err != nil {
            if errors.Is(err, io.EOF) {
                return io.EOF
            }
            // Keep waiting on transient read errors until deadline/context
            select {
            case <-ctx.Done():
                return ctx.Err()
            default:
                time.Sleep(50 * time.Millisecond)
            }
        }
    }
    return fmt.Errorf("prompt not detected within %s", maxWait)
}

// readLineWithCtx reads one line from the PTY honoring context cancellation
func (ow *OllamaWrapper) readLineWithCtx(ctx context.Context) (string, error) {
    type res struct {
        s   string
        err error
    }
    ch := make(chan res, 1)
    go func() {
        s, err := ow.reader.ReadString('\n')
        ch <- res{s: s, err: err}
    }()
    select {
    case r := <-ch:
        return r.s, r.err
    case <-ctx.Done():
        return "", ctx.Err()
    }
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wrapper, err := startOllamaInteractive(rootCtx, binary, model)
	if err != nil {
		log.Fatalf("failed to start ollama: %v", err)
	}

	// Startup test request
	go func() {
		testInput := "hello, hello, hello, is there anybody in there"
		log.Printf("startup test query: %q", testInput)
		ctx, cancel := context.WithTimeout(rootCtx, 60*time.Second)
		defer cancel()
		resp, err := wrapper.query(ctx, testInput)
		if err != nil {
			log.Printf("startup test query error: %v", err)
			return
		}
		log.Printf("startup test response: %q", resp)
	}()

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
		resp, err := wrapper.query(ctx, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Log the response obtained from ollama
		log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, resp)
	})

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if wrapper.cmd.Process != nil {
			_ = wrapper.cmd.Process.Kill()
		}
	}()

	log.Printf("server listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

```

- It’s logged now in both places: `startOllamaInteractive` logs the exact command, and `TestOllamaWrapper_Query` logs it via `t.Logf`.
- See logs by running tests with verbose flag:
  - cd /workspace/ollama-wrap
  - go test -v -run TestOllamaWrapper_Query
  - If needed, extend timeout: go test -v -run TestOllamaWrapper_Query -timeout 10m

- The test output includes both `t.Logf(...)` and `log.Printf(...)` lines.

---

**User**

this is still not working, let's change the code to use ollama in non-interactive mode. Example: /usr/local/bin/ollama run gpt-oss "hello, anybody out there?"

---

**Cursor**

I’m going to refactor the server to call Ollama per-request in non-interactive mode using `/usr/local/bin/ollama run gpt-oss "<prompt>"`, remove the interactive wrapper, update the unit test to call the new per-request function, and run tests/build.

Refactoring to use non-interactive per-request execution of Ollama, updating the server and unit test accordingly, then building and running tests.

```go
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
    text := strings.TrimSpace(string(output))
    if err != nil {
        // Include output in error for debugging
        return text, fmt.Errorf("ollama run failed: %w", err)
    }
    return text, nil
}

func main() {
	binary := "/usr/local/bin/ollama"
	model := "gpt-oss"
	if b := os.Getenv("OLLAMA_BIN"); b != "" {
		binary = b
	}
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		model = m
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Startup test request
	go func() {
        testInput := "hello, hello, hello, is there anybody there?"
		log.Printf("startup test query: %q", testInput)
        ctx, cancel := context.WithTimeout(rootCtx, 2*time.Minute)
		defer cancel()
        resp, err := runOllamaOnce(ctx, binary, model, testInput)
		if err != nil {
            log.Printf("startup test query error: %v\noutput: %s", err, resp)
			return
		}
		log.Printf("startup test response: %q", resp)
	}()

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
		log.Printf("/query response to %s: %q", r.RemoteAddr, resp)
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

```

```go
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
		model = "gpt-oss"
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

```

- Switched to per-request non-interactive execution: each `/query` now runs `/usr/local/bin/ollama run <model> "<prompt>"`.
- Removed the interactive wrapper and PTY handling. Added `runOllamaOnce()` and wired both the HTTP handler and startup test to it.
- Updated the unit test to call the new function. Build and tests pass.

How to run:
- Build and run:
  - cd /workspace/ollama-wrap
  - go build -o ollama-wrap
  - ./ollama-wrap 2>&1 | cat
- Query:
  - curl --get 'http://localhost:8080/query' --data-urlencode 'input=hello, anybody out there?'
- Run test:
  - cd /workspace/ollama-wrap
  - go test -v -run TestOllamaWrapper_Query -timeout 10m

Note:
- Ollama’s CLI may emit ANSI control sequences (spinner/progress). If you want those stripped, say the word and I’ll add a small sanitizer.

---

**User**

I'm seeing odd characters between parts of ollama's response, for example, this should be simply 'TESTING', but i get 'TEST[?25l[?25hING[?25l[?25h'. Let's improve it so we get rid of those characters

