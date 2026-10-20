// Package agent is the protocol between the daemon and sandboxxing-agent, the
// process that runs inside a Lambda MicroVM.
//
// A MicroVM can only be reached through its HTTPS endpoint, which forwards
// HTTP and WebSocket traffic to a port inside the VM. The agent listens there
// and offers what the SSH server needs from a container: running commands
// with or without a terminal, connecting to a port of the VM and a channel of
// events. Each operation is one WebSocket connection.
//
// The agent also serves the lifecycle hooks of the MicroVM on a second port
// that the endpoint does not expose: /run receives the configuration of the
// instance and mounts its home directory, /terminate flushes it and tells the
// daemon that the VM is going away.
package agent

import "time"

// The paths served on the data port.
const (
	PathExec   = "/v1/exec"
	PathDial   = "/v1/dial"
	PathEvents = "/v1/events"
	PathPing   = "/v1/ping"
)

// HookPath is the prefix of the lifecycle hooks that Lambda calls on the hook
// port.
const HookPath = "/aws/lambda-microvms/runtime/v1/"

// The default ports of the agent. Only the data port is reachable through the
// MicroVM endpoint; the hook port is used by Lambda inside the VM.
const (
	DataPort = 8080
	HookPort = 9000
)

// SecretHeader carries the per instance secret that every request on the data
// port must present, on top of the authentication of the MicroVM endpoint.
const SecretHeader = "X-Sandboxxing-Secret"

// The first byte of a binary message on an exec connection names the stream
// that the rest of the message belongs to.
const (
	StreamStdin  byte = 0
	StreamStdout byte = 1
	StreamStderr byte = 2
)

// MaxChunk is the largest payload of one stream message.
const MaxChunk = 32 * 1024

// ReadLimit is the largest message either side accepts.
const ReadLimit = MaxChunk + 1024

// ExecRequest is the first message of an exec connection. It is sent as a
// text message.
type ExecRequest struct {
	Argv []string `json:"argv"`
	// Env is applied on top of the environment of the instance.
	Env []string `json:"env,omitempty"`
	// Dir is the working directory. It defaults to /.
	Dir string `json:"dir,omitempty"`
	// TTY runs the command on a pseudo terminal of the given size. Its output
	// arrives on the stdout stream.
	TTY  bool `json:"tty,omitempty"`
	Rows int  `json:"rows,omitempty"`
	Cols int  `json:"cols,omitempty"`
}

// The types of control messages.
const (
	// ControlResize changes the terminal size (client to agent).
	ControlResize = "resize"
	// ControlEOF closes the standard input of the command (client to agent).
	ControlEOF = "eof"
	// ControlExit reports how the command ended (agent to client). It is the
	// last message of an exec connection.
	ControlExit = "exit"
	// ControlTerminating announces that the VM is being terminated (agent to
	// client, on the events connection).
	ControlTerminating = "terminating"
)

// Control is a text message that is not part of a stream.
type Control struct {
	Type string `json:"type"`
	// Rows and Cols belong to ControlResize.
	Rows int `json:"rows,omitempty"`
	Cols int `json:"cols,omitempty"`
	// Code is the exit status of ControlExit. Error is set instead when the
	// command could not be run at all.
	Code  int    `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
	// Reason belongs to ControlTerminating.
	Reason string `json:"reason,omitempty"`
}

// The reasons of ControlTerminating.
const (
	// ReasonExpired means that the VM reached its maximum duration.
	ReasonExpired = "expired"
	// ReasonTerminated means that the VM was terminated before that.
	ReasonTerminated = "terminated"
)

// RunConfig is the run hook payload: the configuration of the instance that a
// MicroVM serves. The image is shared by every instance, so everything that is
// specific to one is handed over here.
type RunConfig struct {
	// Name is the container name, used as the host name.
	Name string `json:"name"`
	// Secret authenticates the daemon on the data port.
	Secret string `json:"secret"`
	// Env is the environment of the instance.
	Env []string `json:"env,omitempty"`
	// FileSystemID and AccessPointID select the EFS access point that holds
	// the home directory. The home directory is not mounted when they are
	// empty.
	FileSystemID  string `json:"fs,omitempty"`
	AccessPointID string `json:"ap,omitempty"`
	// Home is where the home directory is mounted.
	Home string `json:"home,omitempty"`
	// Shares are access points of the same file system that are mounted
	// after the home directory.
	Shares []ShareMount `json:"shares,omitempty"`
	// MaximumDuration is the maximum duration the VM was started with, which
	// tells an expiry apart from an early termination.
	MaximumDuration time.Duration `json:"max_duration,omitempty"`
}

// ShareMount is a shared directory of the file system.
type ShareMount struct {
	AccessPointID string `json:"ap"`
	Target        string `json:"target"`
	ReadOnly      bool   `json:"ro,omitempty"`
}

// RunRequest is the body that Lambda posts to the run hook.
type RunRequest struct {
	MicrovmID      string `json:"microvmId"`
	RunHookPayload string `json:"runHookPayload"`
}
