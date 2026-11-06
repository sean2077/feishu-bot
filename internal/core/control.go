package core

import "fmt"

// ControlState describes what the host can prove about a native control write.
// A missing acknowledgement is not evidence that the native process rejected it.
type ControlState string

const (
	ControlNotSent     ControlState = "not_sent"
	ControlRejected    ControlState = "rejected"
	ControlUnconfirmed ControlState = "unconfirmed"
)

// ControlFailure preserves the transport outcome without changing Worker.Rename.
// Callers must not replay an unconfirmed control as if it had never been sent.
type ControlFailure struct {
	State ControlState
	Err   error
}

func (e *ControlFailure) Error() string {
	if e.Err == nil {
		return "native control " + string(e.State)
	}
	return fmt.Sprintf("native control %s: %v", e.State, e.Err)
}

func (e *ControlFailure) Unwrap() error { return e.Err }
