package router

import (
	"errors"
	"fmt"
)

// TransportError is a network/transport-level failure (connection refused,
// timeout, HTTP 5xx, auth session lost). It is retryable.
type TransportError struct {
	Op  string
	Err error
}

func (e *TransportError) Error() string { return fmt.Sprintf("transport %s: %v", e.Op, e.Err) }
func (e *TransportError) Unwrap() error { return e.Err }

// CommandError is the router rejecting a command. It is never retried.
type CommandError struct {
	Command string
	Message string
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("router rejected %q: %s", e.Command, e.Message)
}

// VerifyError is returned when, after applying a plan, the re-read state still
// differs from the desired state (typically a silent no-op on the router).
type VerifyError struct {
	Remaining *Plan
}

func (e *VerifyError) Error() string {
	return fmt.Sprintf("verification failed: %d command(s) still pending after apply", len(e.Remaining.Commands))
}

// ErrLocked is returned when another apply holds the lock.
var ErrLocked = errors.New("router is locked by another operation")

// IsRetryable reports whether err should be retried by the client.
func IsRetryable(err error) bool {
	var te *TransportError
	return errors.As(err, &te)
}
