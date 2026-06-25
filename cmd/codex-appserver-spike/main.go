// codex-appserver-spike exercises codex app-server --stdio via JSON-RPC 2.0.
// It sends one simple turn ("say hello") and prints every received message,
// then summarises token usage and compares field coverage to codex exec --json.
//
// Run: go run ./cmd/codex-appserver-spike
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"time"
)

// ── JSON-RPC envelope types ──────────────────────────────────────────────────

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// ── Protocol params ──────────────────────────────────────────────────────────

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeParams struct {
	ClientInfo clientInfo `json:"clientInfo"`
}

type userInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type threadStartParams struct {
	// WorkingDirectory is optional but helps codex find the right config.
	WorkingDirectory *string `json:"workingDirectory,omitempty"`
}

type turnStartParams struct {
	ThreadID string      `json:"threadId"`
	Input    []userInput `json:"input"`
}

// ── Spike state ──────────────────────────────────────────────────────────────

var (
	reqID     atomic.Int64
	threadID  string
	startedAt time.Time
)

// eventLog collects all server messages for the summary.
type eventLog struct {
	method string
	params json.RawMessage
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	fmt.Println("=== codex app-server --stdio spike ===")
	fmt.Println()

	cmd := exec.CommandContext(ctx, "codex", "app-server", "--stdio")
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	must(err, "stdin pipe")
	stdout, err := cmd.StdoutPipe()
	must(err, "stdout pipe")
	must(cmd.Start(), "start codex")
	startedAt = time.Now()

	// Receive goroutine: print and collect every server message.
	var logs []eventLog
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 4*1024*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			var msg rpcMessage
			if err := json.Unmarshal(line, &msg); err != nil {
				fmt.Printf("[raw] %s\n", line)
				continue
			}
			printMessage(msg)
			if msg.Method != "" {
				logs = append(logs, eventLog{method: msg.Method, params: msg.Params})
			}
			if msg.ID != nil && msg.Method == "" && msg.Error == nil {
				// Response to one of our requests — extract threadId from thread/start.
				if threadID == "" {
					var res struct {
						Thread struct {
							ID string `json:"id"`
						} `json:"thread"`
					}
					if json.Unmarshal(msg.Result, &res) == nil && res.Thread.ID != "" {
						threadID = res.Thread.ID
					}
				}
			}
			// Detect turn completion.
			if msg.Method == "turn/completed" {
				time.AfterFunc(500*time.Millisecond, cancel)
			}
		}
	}()

	// ── 1. initialize ─────────────────────────────────────────────────────────
	send(stdin, rpcRequest{
		JSONRPC: "2.0",
		ID:      nextID(),
		Method:  "initialize",
		Params:  initializeParams{ClientInfo: clientInfo{Name: "sybra-spike", Version: "0.0.1"}},
	})

	// Give the server a moment to respond before sending thread/start.
	time.Sleep(500 * time.Millisecond)

	// ── 2. thread/start ───────────────────────────────────────────────────────
	cwd, _ := os.Getwd()
	send(stdin, rpcRequest{
		JSONRPC: "2.0",
		ID:      nextID(),
		Method:  "thread/start",
		Params:  threadStartParams{WorkingDirectory: &cwd},
	})

	// Wait until we have a threadId (or 3 s timeout).
	deadline := time.Now().Add(3 * time.Second)
	for threadID == "" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if threadID == "" {
		fmt.Println("[spike] no threadId received — aborting")
		cancel()
		<-done
		return
	}

	// ── 3. turn/start ─────────────────────────────────────────────────────────
	send(stdin, rpcRequest{
		JSONRPC: "2.0",
		ID:      nextID(),
		Method:  "turn/start",
		Params: turnStartParams{
			ThreadID: threadID,
			Input:    []userInput{{Type: "text", Text: "Reply with exactly: hello from app-server"}},
		},
	})

	<-done
	elapsed := time.Since(startedAt)

	_ = cmd.Wait()

	// ── Summary ───────────────────────────────────────────────────────────────
	fmt.Println()
	fmt.Println("=== spike summary ===")
	fmt.Printf("elapsed: %s\n", elapsed.Round(time.Millisecond))
	fmt.Println()
	printEventCounts(logs)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func nextID() int {
	return int(reqID.Add(1))
}

func send(w io.Writer, req rpcRequest) {
	b, _ := json.Marshal(req)
	fmt.Printf("[→] %s %s\n", req.Method, string(b))
	_, _ = fmt.Fprintf(w, "%s\n", b)
}

func printMessage(msg rpcMessage) {
	switch {
	case msg.Method != "":
		// Notification or server request.
		fmt.Printf("[←] %s %s\n", msg.Method, string(msg.Params))
	case msg.ID != nil && msg.Error != nil:
		fmt.Printf("[←] ERROR id=%d %s\n", *msg.ID, string(msg.Error))
	case msg.ID != nil:
		fmt.Printf("[←] RESPONSE id=%d %s\n", *msg.ID, string(msg.Result))
	}
}

func printEventCounts(logs []eventLog) {
	counts := map[string]int{}
	for _, l := range logs {
		counts[l.method]++
	}
	fmt.Println("server notifications/requests received:")
	for m, n := range counts {
		fmt.Printf("  %-52s %d\n", m, n)
	}
	fmt.Println()

	// Check for token usage.
	for _, l := range logs {
		if l.method == "thread/tokenUsage/updated" {
			var p struct {
				TokenUsage struct {
					Last struct {
						InputTokens           int `json:"inputTokens"`
						CachedInputTokens     int `json:"cachedInputTokens"`
						OutputTokens          int `json:"outputTokens"`
						ReasoningOutputTokens int `json:"reasoningOutputTokens"`
						TotalTokens           int `json:"totalTokens"`
					} `json:"last"`
				} `json:"tokenUsage"`
			}
			if json.Unmarshal(l.params, &p) == nil {
				u := p.TokenUsage.Last
				fmt.Printf("token usage (last turn): input=%d cached=%d output=%d reasoning=%d total=%d\n",
					u.InputTokens, u.CachedInputTokens, u.OutputTokens, u.ReasoningOutputTokens, u.TotalTokens)
			}
		}
	}
}

func must(err error, msg string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal %s: %v\n", msg, err)
		os.Exit(1)
	}
}
