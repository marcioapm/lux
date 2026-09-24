package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// An MCP client, just enough for mcp-call: streamable HTTP, JSON-RPC.
// Each protocol front end fills agent.mcp from what its client gave.

type mcpServer struct {
	URL     string
	Headers map[string]string
}

// toolCall is one mcp-call, as the front ends report it: started (Done
// false), then done with Result or Err.
type toolCall struct {
	ID, Server, Tool string
	Args             map[string]any
	Done             bool
	Result, Err      string
	Started          time.Time
}

func (a *agent) setMCP(servers map[string]mcpServer) {
	a.mu.Lock()
	a.mcp = servers
	a.mu.Unlock()
}

// mcpCall calls a tool and returns what to say: its text, or an error.
func (a *agent) mcpCall(server, tool, text string) string {
	a.mu.Lock()
	s, ok := a.mcp[server]
	a.mu.Unlock()
	if !ok {
		return "error: no MCP server " + server
	}
	call := toolCall{ID: fmt.Sprintf("call-%d", time.Now().UnixNano()), Server: server, Tool: tool,
		Args: map[string]any{"text": text}, Started: time.Now()}
	a.tool(call)
	res, err := s.callTool(tool, call.Args)
	call.Done, call.Result = true, res
	if err != nil {
		call.Err = err.Error()
		a.tool(call)
		return "error: mcp " + server + ": " + call.Err
	}
	a.tool(call)
	return res
}

// callTool runs the MCP handshake (initialize, notifications/initialized)
// and tools/call, and returns the result's text.
func (s mcpServer) callTool(tool string, args map[string]any) (string, error) {
	c := &http.Client{Timeout: 30 * time.Second}
	session, version := "", ""
	post := func(id int, method string, params any) (json.RawMessage, error) {
		msg := map[string]any{"jsonrpc": "2.0", "method": method}
		if params != nil {
			msg["params"] = params
		}
		if id > 0 {
			msg["id"] = id
		}
		body, _ := json.Marshal(msg)
		req, err := http.NewRequest("POST", s.URL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range s.Headers {
			req.Header.Set(k, v)
		}
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		if version != "" {
			req.Header.Set("MCP-Protocol-Version", version)
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("%s: HTTP %d", method, resp.StatusCode)
		}
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			session = sid
		}
		if id == 0 {
			return nil, nil // a notification: 202, no body
		}
		return rpcResult(resp, id)
	}
	res, err := post(1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "lux-fake", "version": "1"}})
	if err != nil {
		return "", err
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(res, &init)
	version = init.ProtocolVersion
	if _, err := post(0, "notifications/initialized", nil); err != nil {
		return "", err
	}
	res, err = post(2, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", err
	}
	var r struct {
		Content textBlocks `json:"content"`
		IsError bool       `json:"isError"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return "", fmt.Errorf("tools/call: %v", err)
	}
	if r.IsError {
		return "", fmt.Errorf("tool error: %s", r.Content)
	}
	return r.Content.String(), nil
}

// rpcResult reads the response to request id: a JSON body, or an SSE
// stream (text/event-stream) whose events carry JSON-RPC messages.
func rpcResult(resp *http.Response, id int) (json.RawMessage, error) {
	var msgs [][]byte
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var data []string
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			line := sc.Text()
			if d, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(d, " "))
			} else if line == "" && len(data) > 0 {
				msgs = append(msgs, []byte(strings.Join(data, "\n")))
				data = nil
			}
		}
		if len(data) > 0 {
			msgs = append(msgs, []byte(strings.Join(data, "\n")))
		}
	} else {
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, b)
	}
	for _, b := range msgs {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &m) != nil || string(m.ID) != fmt.Sprint(id) {
			continue
		}
		if m.Error != nil {
			return nil, fmt.Errorf("%s", m.Error.Message)
		}
		return m.Result, nil
	}
	return nil, fmt.Errorf("no response to request %d", id)
}

// ---- configuration, per protocol ------------------------------------------

// acpMCP reads ACP's session/new and session/load mcpServers (HTTP ones).
func acpMCP(raw json.RawMessage) map[string]mcpServer {
	var list []struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		URL     string `json:"url"`
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
	}
	_ = json.Unmarshal(raw, &list)
	out := map[string]mcpServer{}
	for _, s := range list {
		if s.Type != "http" {
			continue
		}
		m := mcpServer{URL: s.URL, Headers: map[string]string{}}
		for _, h := range s.Headers {
			m.Headers[h.Name] = h.Value
		}
		out[s.Name] = m
	}
	return out
}

// claudeMCP reads Claude Code's --mcp-config files.
func claudeMCP(args []string) map[string]mcpServer {
	out := map[string]mcpServer{}
	for i, a := range args {
		if a != "--mcp-config" || i+1 >= len(args) {
			continue
		}
		b, err := os.ReadFile(args[i+1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcp config:", err)
			continue
		}
		var cfg struct {
			MCPServers map[string]struct {
				Type    string            `json:"type"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			fmt.Fprintln(os.Stderr, "mcp config:", err)
			continue
		}
		for name, s := range cfg.MCPServers {
			if s.Type == "http" {
				out[name] = mcpServer{URL: s.URL, Headers: s.Headers}
			}
		}
	}
	return out
}

// codexMCP reads Codex's -c mcp_servers.<name>.url="…" and
// -c mcp_servers.<name>.env_http_headers={"Header"="VAR"} overrides; header
// values come from the variables they name.
func codexMCP(args []string) map[string]mcpServer {
	out := map[string]mcpServer{}
	for i, a := range args {
		if a != "-c" || i+1 >= len(args) {
			continue
		}
		key, val, _ := strings.Cut(args[i+1], "=")
		rest, ok := strings.CutPrefix(key, "mcp_servers.")
		if !ok {
			continue
		}
		name, field, _ := strings.Cut(rest, ".")
		s := out[name]
		if s.Headers == nil {
			s.Headers = map[string]string{}
		}
		switch field {
		case "url":
			s.URL = tomlStrings(val)[0]
		case "env_http_headers":
			kv := tomlStrings(val)
			for j := 0; j+1 < len(kv); j += 2 {
				s.Headers[kv[j]] = os.Getenv(kv[j+1])
			}
		}
		out[name] = s
	}
	return out
}

// tomlStrings is the strings and bare keys in a TOML value, in order:
// enough for a string and an inline table of strings.
func tomlStrings(v string) []string {
	var out []string
	for i := 0; i < len(v); {
		switch c := v[i]; {
		case c == '"':
			j := i + 1
			for j < len(v) && v[j] != '"' {
				if v[j] == '\\' {
					j++
				}
				j++
			}
			var s string
			_ = json.Unmarshal([]byte(v[i:min(j+1, len(v))]), &s)
			out = append(out, s)
			i = j + 1
		case c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z':
			j := i
			for j < len(v) && (v[j] == '_' || v[j] == '-' || v[j] >= '0' && v[j] <= '9' || v[j] >= 'A' && v[j] <= 'Z' || v[j] >= 'a' && v[j] <= 'z') {
				j++
			}
			out = append(out, v[i:j])
			i = j
		default:
			i++
		}
	}
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}
