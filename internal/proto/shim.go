package proto

import (
	"strconv"

	"github.com/marcioapm/lux/internal/spec"
)

// Runner ↔ shim.
//
// The shim is PID 1 in every container. It reads ShimConfig from
// /.lux/run/config.json (written by the runner before the container
// starts; no secrets), listens on /.lux/run/shim.sock, and waits for the
// runner to connect and send "start" with the secret values. Nothing secret
// is ever written to the host's disk or to Podman's container config.
//
// Output does not go over the socket: the shim appends records to
// /.lux/run/output-<epoch>.jsonl (after redacting secrets), so a runner
// restart loses nothing. The runner tails that file to relay live output
// and to pick up adapter events (session id, idle/busy, input acks).
//
// When the workload is done the shim writes /.lux/run/exit-<epoch>.json
// and exits with the workload's code.

const (
	ShimDir        = "/.lux"
	ShimRunDir     = "/.lux/run"
	ShimBinary     = "/.lux/bin/lux-shim"
	ShimSocket     = "/.lux/run/shim.sock"
	ShimConfigFile = "/.lux/run/config.json"
	ShimSecretsDir = "/.lux/secrets"
)

func OutputFile(epoch int) string { return "output-" + strconv.Itoa(epoch) + ".jsonl" }
func ExitFile(epoch int) string   { return "exit-" + strconv.Itoa(epoch) + ".json" }

type ShimConfig struct {
	RunID    string            `json:"runId"`
	Epoch    int               `json:"epoch"`
	Adapter  string            `json:"adapter"`
	Command  []string          `json:"command"`
	Prompt   string            `json:"prompt,omitempty"`
	Workdir  string            `json:"workdir,omitempty"`
	User     string            `json:"user,omitempty"` // name or uid[:gid]; empty: root
	TTY      bool              `json:"tty,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Init     string            `json:"init,omitempty"`
	GraceSec float64           `json:"graceSec"`
	// Resume is set when this is not the Run's first placement.
	Resume        bool     `json:"resume,omitempty"`
	ResumeCommand []string `json:"resumeCommand,omitempty"`
	SessionID     string   `json:"sessionId,omitempty"`
	// Paths of mounted volumes: their roots are handed to the workload user
	// when empty (a fresh volume is owned by root).
	VolumePaths []string `json:"volumePaths,omitempty"`
	// Secrets by name: how each is exposed (values arrive with "start").
	Secrets []spec.Secret `json:"secrets,omitempty"`
	// ArtifactsDir is watched for on-demand artifacts ($LUX_ARTIFACTS).
	ArtifactsDir string `json:"artifactsDir,omitempty"`
	// Nested: the workload keeps CAP_SETUID and CAP_SETGID as ambient
	// capabilities, which rootless Podman inside it needs to map its
	// containers' ids (newuidmap cannot gain them: no-new-privileges).
	Nested bool `json:"nested,omitempty"`
}

// ShimMsg is one line on the shim socket, either direction.
type ShimMsg struct {
	Type string `json:"type"`
	// start
	Secrets map[string]string `json:"secrets,omitempty"`
	Input   *Input            `json:"input,omitempty"` // start (first input), input
	// stop
	Reason string `json:"reason,omitempty"`
	// exec / attach: the stream's first message, then its data
	Stream   *StreamOpen `json:"stream,omitempty"`
	Data     []byte      `json:"data,omitempty"`
	Channel  string      `json:"ch,omitempty"` // stdout | stderr, for output
	EOF      bool        `json:"eof,omitempty"`
	Rows     int         `json:"rows,omitempty"` // a resize
	Cols     int         `json:"cols,omitempty"`
	ExitCode *int        `json:"exitCode,omitempty"`
	// replies
	Error string `json:"error,omitempty"`
	OK    bool   `json:"ok,omitempty"`
}

const (
	ShimStart     = "start"
	ShimInput     = "input"
	ShimInterrupt = "interrupt"
	ShimStop      = "stop"
	ShimPing      = "ping"
	// A connection that starts with stream (exec, attach) carries only that
	// stream: data both ways, then exit.
	ShimStream = "stream"
	ShimData   = "data"
	ShimExit   = "exit"
)

// Event types the shim writes as ch=event records. The runner forwards the
// lux.* ones to luxd as adapter events; all are visible in the output.
const (
	EvSession  = "lux.session"  // {"sessionId"}
	EvActivity = "lux.activity" // {"activity": "idle" | "busy"}
	EvInputAck = "lux.input"    // {"requestId", "error"?}
	EvInit     = "lux.init"     // {"phase": "start" | "done", "exitCode"?}
	EvWorkload = "lux.workload" // {"phase": "start", "pid"}
	EvStop     = "lux.stop"     // {"reason"}
	EvWarning  = "lux.warning"  // {"message"}
	EvArtifact = "lux.artifact" // {"path"}
)

// ExitInfo is the shim's account of how the workload ended.
type ExitInfo struct {
	ExitCode int    `json:"exitCode"`
	Signal   string `json:"signal,omitempty"`
	// exited | stopped | init-failed | start-failed
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
	LastSeq int64  `json:"lastSeq"`
}
