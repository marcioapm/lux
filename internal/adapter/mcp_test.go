package adapter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

const mcpToken = "Bearer s3cr3t-token"

func mcpConfig(command ...string) proto.ShimConfig {
	cfg := proto.ShimConfig{Command: command, MCPServers: []spec.MCPServer{
		{Name: "tools", URL: "http://10.0.0.5:8080/mcp", Headers: []spec.MCPHeader{{Name: "Authorization", Secret: "MCP_TOKEN"}}},
		{Name: "open", URL: "https://open.example/mcp"},
	}}
	cfg.ResolveMCP(map[string]string{"MCP_TOKEN": mcpToken})
	return cfg
}

func noSecret(t *testing.T, argv []string) {
	t.Helper()
	if strings.Contains(strings.Join(argv, " "), "s3cr3t") {
		t.Fatalf("a secret is in argv: %q", argv)
	}
}

func TestACPMCPServers(t *testing.T) {
	b, _ := json.Marshal(acpMCPServers(mcpConfig().MCP))
	want := `[{"headers":[{"name":"Authorization","value":"Bearer s3cr3t-token"}],"name":"tools","type":"http","url":"http://10.0.0.5:8080/mcp"},` +
		`{"headers":[],"name":"open","type":"http","url":"https://open.example/mcp"}]`
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
	if b, _ := json.Marshal(acpMCPServers(nil)); string(b) != "[]" {
		t.Fatalf("no servers must be an empty list, got %s", b)
	}
}

func TestClaudeMCPConfig(t *testing.T) {
	cfg := mcpConfig("claude")
	c := NewClaude()
	argv, err := c.Command(cfg)
	if err != nil {
		t.Fatal(err)
	}
	noSecret(t, argv)
	if i := slices.Index(argv, "--mcp-config"); i < 0 || argv[i+1] != proto.ShimSecretsDir+"/lux-claude-mcp.json" {
		t.Fatalf("argv %q", argv)
	}
	if slices.Contains(argv, "--strict-mcp-config") {
		t.Fatal("the user's own MCP config must stay")
	}
	files := c.CredentialFiles(cfg, nil, "/home/agent")
	var got map[string]map[string]struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(files[proto.ShimSecretsDir+"/lux-claude-mcp.json"], &got); err != nil {
		t.Fatal(err)
	}
	s := got["mcpServers"]["tools"]
	if s.Type != "http" || s.URL != "http://10.0.0.5:8080/mcp" || s.Headers["Authorization"] != mcpToken || got["mcpServers"]["open"].URL == "" {
		t.Fatalf("config %+v", got)
	}
	// None configured: no flag, no file.
	plain := proto.ShimConfig{Command: []string{"claude"}}
	if argv, _ := c.Command(plain); slices.Contains(argv, "--mcp-config") || c.CredentialFiles(plain, nil, "/h") != nil {
		t.Fatal("MCP config without servers")
	}
}

func TestCodexMCPOverrides(t *testing.T) {
	cfg := mcpConfig("codex")
	c := NewCodex()
	argv, err := c.Command(cfg)
	if err != nil {
		t.Fatal(err)
	}
	noSecret(t, argv)
	want := []string{"codex", "app-server",
		"-c", `mcp_servers.tools.url="http://10.0.0.5:8080/mcp"`,
		"-c", `mcp_servers.tools.env_http_headers={"Authorization"="LUX_MCP_0_0"}`,
		"-c", `mcp_servers.open.url="https://open.example/mcp"`}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv\n got %q\nwant %q", argv, want)
	}
	env := c.Environment(cfg)
	if len(env) != 1 || env["LUX_MCP_0_0"] != mcpToken {
		t.Fatalf("env %v", env)
	}
}

// A custom resume command is the spec's own: adapters add nothing to it.
func TestResumeCommandIsLeftAlone(t *testing.T) {
	for _, ad := range []Adapter{NewClaude(), NewCodex(), NewACP()} {
		cfg := mcpConfig("agent")
		cfg.Resume, cfg.ResumeCommand = true, []string{"my-resume"}
		if argv, _ := ad.Command(cfg); !slices.Equal(argv, []string{"my-resume"}) {
			t.Fatalf("%T: %q", ad, argv)
		}
	}
}

// The config file never carries a header value; the shim resolves it.
func TestShimConfigHoldsNoHeaderValue(t *testing.T) {
	b, _ := json.Marshal(mcpConfig("x"))
	if strings.Contains(string(b), "s3cr3t") || !strings.Contains(string(b), `"secret":"MCP_TOKEN"`) {
		t.Fatalf("config %s", b)
	}
}

type recSink struct {
	Sink
	events []string
}

func (r *recSink) Event(typ string, data any) {
	b, _ := json.Marshal(data)
	r.events = append(r.events, typ+" "+string(b))
}
func (r *recSink) Session(string) {}
func (r *recSink) Activity(bool)  {}

// handshakeWith runs the ACP handshake against a scripted agent whose
// initialize result is init; it returns the session/new or session/load
// params the agent got, and the sink.
func handshakeWith(t *testing.T, init string, cfg proto.ShimConfig) (map[string]json.RawMessage, *recSink) {
	t.Helper()
	toAgent, fromAdapter := io.Pipe()
	fromAgent, toAdapter := io.Pipe()
	a, sink := NewACP(), &recSink{}
	a.sink = sink
	a.rpc.attach(fromAdapter)
	go a.rpc.readLoop(fromAgent, func(rpcMsg) {}, func(rpcMsg) {}, func([]byte) {})
	got := make(chan map[string]json.RawMessage, 1)
	go func() {
		sc := bufio.NewScanner(toAgent)
		for sc.Scan() {
			var m struct {
				ID     int                        `json:"id"`
				Method string                     `json:"method"`
				Params map[string]json.RawMessage `json:"params"`
			}
			_ = json.Unmarshal(sc.Bytes(), &m)
			res := `{}`
			switch m.Method {
			case "initialize":
				res = init
			case "session/new", "session/load":
				got <- m.Params
				res = `{"sessionId":"s1"}`
			}
			fmt.Fprintf(toAdapter, `{"jsonrpc":"2.0","id":%d,"result":%s}`+"\n", m.ID, res)
		}
	}()
	if err := a.handshake(cfg); err != nil {
		t.Fatal(err)
	}
	return <-got, sink
}

func TestACPHandshakeMCP(t *testing.T) {
	withHTTP := `{"agentCapabilities":{"loadSession":true,"mcpCapabilities":{"http":true}}}`
	params, sink := handshakeWith(t, withHTTP, mcpConfig())
	if !strings.Contains(string(params["mcpServers"]), `"value":"Bearer s3cr3t-token"`) || len(sink.events) != 0 {
		t.Fatalf("session/new: %s %v", params["mcpServers"], sink.events)
	}
	// session/load gets them too.
	cfg := mcpConfig()
	cfg.Resume, cfg.SessionID = true, "s1"
	params, _ = handshakeWith(t, withHTTP, cfg)
	if string(params["sessionId"]) != `"s1"` || !strings.Contains(string(params["mcpServers"]), `"name":"tools"`) {
		t.Fatalf("session/load: %v", params)
	}
	// No HTTP capability: none sent, and a warning.
	params, sink = handshakeWith(t, `{"agentCapabilities":{"mcpCapabilities":{"sse":true}}}`, mcpConfig())
	if string(params["mcpServers"]) != "[]" || len(sink.events) != 1 || !strings.Contains(sink.events[0], "does not support HTTP MCP servers") {
		t.Fatalf("without http: %s %v", params["mcpServers"], sink.events)
	}
	// Nothing configured: no warning either way.
	params, sink = handshakeWith(t, `{}`, proto.ShimConfig{})
	if string(params["mcpServers"]) != "[]" || len(sink.events) != 0 {
		t.Fatalf("none: %s %v", params["mcpServers"], sink.events)
	}
}
