// Command lux-fake is a scripted agent for tests. It speaks ACP (JSON-RPC
// 2.0 over stdio) like a real agent, keeps its conversation in a
// transcript under $HOME/.lux-fake/<session>.jsonl, and resumes from it
// with session/load — so steering, stop and resume on another host can be
// tested without a model.
//
// Each prompt is a script, one command per line:
//
//	echo <text>            reply with text
//	write <file> <text>    write text to a file (relative to cwd)
//	append <file> <text>   append a line
//	read <file>            reply with the file's contents
//	sleep <seconds>        take a while (cancellable)
//	print-secret <NAME>    reply with an environment variable
//	stderr <text>          write to stderr
//	history                reply with every prompt so far in this session
//	exit <code>            exit the process
//	ask                    request a permission; reply with the outcome
//
// Anything else is echoed back as "you said: …".
//
// Run with no arguments for ACP. `lux-fake plain` is a line-oriented
// generic workload: each stdin line is a script line.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type msg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type agent struct {
	// streamJSON: speak Claude Code's stream-json instead of ACP.
	streamJSON bool
	out        *json.Encoder
	outMu      sync.Mutex
	mu         sync.Mutex
	session    string
	cwd        string
	cancel     chan struct{}
	pending    map[string]chan json.RawMessage
	nextID     int
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "plain" {
		plain()
		return
	}
	if slices.Contains(os.Args, "stream-json") {
		streamJSON()
		return
	}
	a := &agent{out: json.NewEncoder(os.Stdout), pending: map[string]chan json.RawMessage{}}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var m msg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if m.Method == "" && len(m.ID) > 0 {
			a.mu.Lock()
			ch := a.pending[string(m.ID)]
			delete(a.pending, string(m.ID))
			a.mu.Unlock()
			if ch != nil {
				ch <- m.Result
			}
			continue
		}
		go a.handle(m)
	}
}

func (a *agent) send(v any) {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	_ = a.out.Encode(v)
}

func (a *agent) reply(id json.RawMessage, result any) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (a *agent) replyErr(id json.RawMessage, code int, message string) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func (a *agent) update(kind string, text string) {
	a.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
		"sessionId": a.session,
		"update":    map[string]any{"sessionUpdate": kind, "content": map[string]string{"type": "text", "text": text}},
	}})
}

func transcriptDir() string {
	home, _ := os.UserHomeDir()
	if h := os.Getenv("HOME"); h != "" {
		home = h
	}
	return filepath.Join(home, ".lux-fake")
}

type entry struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

func (a *agent) record(role, text string) {
	_ = os.MkdirAll(transcriptDir(), 0o755)
	f, err := os.OpenFile(filepath.Join(transcriptDir(), a.session+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(entry{role, text})
	f.Write(append(b, '\n'))
}

func (a *agent) history() []entry {
	b, err := os.ReadFile(filepath.Join(transcriptDir(), a.session+".jsonl"))
	if err != nil {
		return nil
	}
	var out []entry
	for _, l := range strings.Split(string(b), "\n") {
		var e entry
		if json.Unmarshal([]byte(l), &e) == nil && e.Role != "" {
			out = append(out, e)
		}
	}
	return out
}

func (a *agent) handle(m msg) {
	switch m.Method {
	case "initialize":
		a.reply(m.ID, map[string]any{
			"protocolVersion":   1,
			"agentCapabilities": map[string]any{"loadSession": true},
			"agentInfo":         map[string]string{"name": "lux-fake", "version": "1"},
		})
	case "session/new":
		var p struct {
			Cwd string `json:"cwd"`
		}
		_ = json.Unmarshal(m.Params, &p)
		a.mu.Lock()
		a.session = fmt.Sprintf("fake-%d", time.Now().UnixNano())
		a.cwd = p.Cwd
		a.mu.Unlock()
		a.record("system", "session started")
		a.reply(m.ID, map[string]any{"sessionId": a.session})
	case "session/load":
		var p struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if _, err := os.Stat(filepath.Join(transcriptDir(), p.SessionID+".jsonl")); err != nil {
			a.replyErr(m.ID, -32002, "session not found: "+p.SessionID)
			return
		}
		a.mu.Lock()
		a.session, a.cwd = p.SessionID, p.Cwd
		a.mu.Unlock()
		for _, e := range a.history() {
			kind := "agent_message_chunk"
			if e.Role == "user" {
				kind = "user_message_chunk"
			}
			a.update(kind, e.Text+"\n")
		}
		a.record("system", "session loaded")
		a.reply(m.ID, map[string]any{})
	case "session/prompt":
		var p struct {
			Prompt []struct {
				Text string `json:"text"`
			} `json:"prompt"`
		}
		_ = json.Unmarshal(m.Params, &p)
		var text strings.Builder
		for _, b := range p.Prompt {
			text.WriteString(b.Text)
		}
		a.mu.Lock()
		a.cancel = make(chan struct{})
		cancel := a.cancel
		a.mu.Unlock()
		a.record("user", text.String())
		stop := a.runScript(text.String(), cancel)
		a.reply(m.ID, map[string]any{"stopReason": stop})
	case "session/cancel":
		a.mu.Lock()
		if a.cancel != nil {
			select {
			case <-a.cancel:
			default:
				close(a.cancel)
			}
		}
		a.mu.Unlock()
	default:
		if len(m.ID) > 0 {
			a.replyErr(m.ID, -32601, "unknown method "+m.Method)
		}
	}
}

func (a *agent) say(s string) {
	if a.streamJSON {
		a.send(map[string]any{"type": "assistant", "session_id": a.session,
			"message": map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": s}}}})
	} else {
		a.update("agent_message_chunk", s+"\n")
	}
	a.record("agent", s)
}

func (a *agent) path(p string) string {
	if filepath.IsAbs(p) || a.cwd == "" {
		return p
	}
	return filepath.Join(a.cwd, p)
}

func (a *agent) runScript(script string, cancel chan struct{}) string {
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cmd, rest, _ := strings.Cut(line, " ")
		switch cmd {
		case "echo":
			a.say(rest)
		case "write", "append":
			file, text, _ := strings.Cut(rest, " ")
			flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
			if cmd == "append" {
				flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
			}
			text += "\n"
			_ = os.MkdirAll(filepath.Dir(a.path(file)), 0o755)
			f, err := os.OpenFile(a.path(file), flag, 0o644)
			if err != nil {
				a.say("error: " + err.Error())
				continue
			}
			f.WriteString(text)
			f.Close()
			a.say("wrote " + file)
		case "read":
			b, err := os.ReadFile(a.path(rest))
			if err != nil {
				a.say("error: " + err.Error())
				continue
			}
			a.say(strings.TrimRight(string(b), "\n"))
		case "sleep":
			secs, _ := strconv.ParseFloat(rest, 64)
			select {
			case <-time.After(time.Duration(secs * float64(time.Second))):
			case <-cancel:
				a.say("cancelled")
				return "cancelled"
			}
		case "print-secret":
			a.say(os.Getenv(rest))
		case "stderr":
			fmt.Fprintln(os.Stderr, rest)
		case "history":
			var parts []string
			for _, e := range a.history() {
				if e.Role == "user" {
					parts = append(parts, e.Text)
				}
			}
			a.say("history: " + strings.Join(parts, " | "))
		case "exit":
			code, _ := strconv.Atoi(rest)
			os.Exit(code)
		case "ask":
			a.mu.Lock()
			a.nextID++
			id := fmt.Sprintf("%d", 1000+a.nextID)
			ch := make(chan json.RawMessage, 1)
			a.pending[id] = ch
			a.mu.Unlock()
			a.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": "session/request_permission", "params": map[string]any{
				"sessionId": a.session,
				"options": []map[string]string{
					{"optionId": "no", "name": "Reject", "kind": "reject_once"},
					{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
				},
			}})
			res := <-ch
			a.say("permission: " + string(res))
		default:
			a.say("you said: " + line)
		}
		select {
		case <-cancel:
			a.say("cancelled")
			return "cancelled"
		default:
		}
	}
	return "end_turn"
}

// streamJSON speaks Claude Code's stream-json protocol (the subset lux
// uses; see docs/agent-protocols.md): run with
//
//	lux-fake -p --input-format stream-json --output-format stream-json --verbose [--resume <id>]
//
// User messages are queued and run one turn at a time; each turn ends
// with a "result" event. A control_request interrupt cancels the running
// turn. SIGINT ends the turn cleanly and exits.
func streamJSON() {
	a := &agent{out: json.NewEncoder(os.Stdout), streamJSON: true, cwd: "."}
	if wd, err := os.Getwd(); err == nil {
		a.cwd = wd
	}
	for i, arg := range os.Args {
		if arg == "--resume" && i+1 < len(os.Args) {
			a.session = os.Args[i+1]
		}
	}
	if a.session == "" {
		a.session = fmt.Sprintf("fake-%d", time.Now().UnixNano())
		a.record("system", "session started")
	} else if _, err := os.Stat(filepath.Join(transcriptDir(), a.session+".jsonl")); err != nil {
		fmt.Fprintf(os.Stderr, "No conversation found with session ID: %s\n", a.session)
		os.Exit(1)
	} else {
		a.record("system", "session resumed")
	}
	a.send(map[string]any{"type": "system", "subtype": "init", "session_id": a.session, "cwd": a.cwd,
		"capabilities": []string{"interrupt_receipt_v1"}})

	turns := make(chan string, 64)
	var cancelMu sync.Mutex
	var cancel chan struct{}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	go func() {
		<-sigs
		cancelMu.Lock()
		if cancel != nil {
			close(cancel)
			cancel = nil
		}
		cancelMu.Unlock()
		time.Sleep(100 * time.Millisecond)
		os.Exit(130)
	}()
	go func() {
		for text := range turns {
			c := make(chan struct{})
			cancelMu.Lock()
			cancel = c
			cancelMu.Unlock()
			a.record("user", text)
			stop := a.runScript(text, c)
			reason := "completed"
			if stop == "cancelled" {
				reason = "aborted_streaming"
			}
			cancelMu.Lock()
			cancel = nil
			cancelMu.Unlock()
			a.send(map[string]any{"type": "result", "subtype": "success", "session_id": a.session,
				"terminal_reason": reason, "queued_turn_count": len(turns)})
		}
	}()
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var m struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "user":
			var text strings.Builder
			for _, c := range m.Message.Content {
				text.WriteString(c.Text)
			}
			turns <- text.String()
		case "control_request":
			if m.Request.Subtype == "interrupt" {
				cancelMu.Lock()
				if cancel != nil {
					close(cancel)
					cancel = nil
				}
				cancelMu.Unlock()
			}
			a.send(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "success", "request_id": m.RequestID, "response": map[string]any{"still_queued": []string{}}}})
		}
	}
	close(turns)
	// stdin closed: finish queued turns, like the real CLI in -p mode.
	time.Sleep(200 * time.Millisecond)
}

// plain is a line-oriented workload for the generic adapter.
func plain() {
	a := &agent{cwd: "."}
	a.out = json.NewEncoder(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		cmd, rest, _ := strings.Cut(line, " ")
		switch cmd {
		case "echo":
			fmt.Println(rest)
		case "exit":
			code, _ := strconv.Atoi(rest)
			os.Exit(code)
		case "print-secret":
			fmt.Println(os.Getenv(rest))
		case "write":
			file, text, _ := strings.Cut(rest, " ")
			os.WriteFile(file, []byte(text), 0o644)
			fmt.Println("wrote", file)
		default:
			fmt.Println("you said:", line)
		}
	}
}
