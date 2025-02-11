package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/spiral/goridge/v3/pkg/relay"
	"github.com/spiral/roadrunner/v2/payload"
)

// State represents WorkerProcess status and updated time.
type State interface {
	fmt.Stringer
	// Value returns StateImpl value
	Value() int64
	// Set sets the StateImpl
	Set(value int64)
	// NumExecs shows how many times WorkerProcess was invoked
	NumExecs() uint64
	// IsActive returns true if WorkerProcess not Inactive or Stopped
	IsActive() bool
	// RegisterExec using to registering php executions
	RegisterExec()
	// SetLastUsed sets worker last used time
	SetLastUsed(lu uint64)
	// LastUsed return worker last used time
	LastUsed() uint64
}

type BaseProcess interface {
	fmt.Stringer

	// Pid returns worker pid.
	Pid() int64

	// Created returns time worker was created at.
	Created() time.Time

	// State return receive-only WorkerProcess state object, state can be used to safely access
	// WorkerProcess status, time when status changed and number of WorkerProcess executions.
	State() State

	// Start used to run Cmd and immediately return
	Start() error

	// Wait must be called once for each WorkerProcess, call will be released once WorkerProcess is
	// complete and will return process error (if any), if stderr is presented it's value
	// will be wrapped as WorkerError. Method will return error code if php process fails
	// to find or Start the script.
	Wait() error

	// Stop sends soft termination command to the WorkerProcess and waits for process completion.
	Stop() error

	// Kill kills underlying process, make sure to call Wait() func to gather
	// error log from the stderr. Does not waits for process completion!
	Kill() error

	// Relay returns attached to worker goridge relay
	Relay() relay.Relay

	// AttachRelay used to attach goridge relay to the worker process
	AttachRelay(rl relay.Relay)
}

type SyncWorker interface {
	// BaseProcess provides basic functionality for the SyncWorker
	BaseProcess
	// Exec used to execute payload on the SyncWorker, there is no TIMEOUTS
	Exec(rqs *payload.Payload) (*payload.Payload, error)
	// ExecWithTTL used to handle Exec with TTL
	ExecWithTTL(ctx context.Context, p *payload.Payload) (*payload.Payload, error)
}
