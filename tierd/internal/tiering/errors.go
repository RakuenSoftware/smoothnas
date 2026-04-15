package tiering

import "fmt"

// AdapterErrorKind classifies the type of error returned by a TieringAdapter
// method. The control plane uses the kind to decide whether to retry, escalate,
// or surface the error to the operator.
type AdapterErrorKind string

const (
	// ErrTransient indicates a temporary backend condition. The control plane
	// may retry the operation after a backoff period.
	ErrTransient AdapterErrorKind = "transient"

	// ErrPermanent indicates that operator action is required before the
	// operation can succeed.
	ErrPermanent AdapterErrorKind = "permanent"

	// ErrCapabilityViolation indicates the adapter does not support the
	// requested operation given its declared capabilities.
	ErrCapabilityViolation AdapterErrorKind = "capability_violation"

	// ErrStaleRevision indicates a policy or intent revision mismatch. The
	// caller should replan before retrying.
	ErrStaleRevision AdapterErrorKind = "stale_revision"

	// ErrBackendDegraded indicates the backend cannot safely execute the
	// operation in its current health state.
	ErrBackendDegraded AdapterErrorKind = "backend_degraded"
)

// AdapterError is the structured error type returned by TieringAdapter methods.
// Adapters must return *AdapterError (not a plain error) so the control plane
// can classify the failure by kind without string matching.
type AdapterError struct {
	Kind    AdapterErrorKind
	Message string
	Cause   error
}

// Error implements the error interface.
func (e *AdapterError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("adapter error [%s]: %s: %v", e.Kind, e.Message, e.Cause)
	}
	return fmt.Sprintf("adapter error [%s]: %s", e.Kind, e.Message)
}

// Unwrap returns the underlying cause so errors.Is/As can traverse the chain.
func (e *AdapterError) Unwrap() error {
	return e.Cause
}
