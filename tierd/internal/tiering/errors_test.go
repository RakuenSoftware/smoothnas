package tiering_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/tiering"
)

func TestAdapterErrorKindsCoverage(t *testing.T) {
	allKinds := []tiering.AdapterErrorKind{
		tiering.ErrTransient,
		tiering.ErrPermanent,
		tiering.ErrCapabilityViolation,
		tiering.ErrStaleRevision,
		tiering.ErrBackendDegraded,
	}

	for _, kind := range allKinds {
		err := &tiering.AdapterError{Kind: kind, Message: "test message"}

		// Must implement the error interface.
		var _ error = err

		// Error string must contain the kind.
		if !strings.Contains(err.Error(), string(kind)) {
			t.Errorf("Error() for kind %q does not contain kind string: %q", kind, err.Error())
		}
		// Error string must contain the message.
		if !strings.Contains(err.Error(), "test message") {
			t.Errorf("Error() for kind %q does not contain message: %q", kind, err.Error())
		}
	}
}

func TestAdapterErrorUnwrap(t *testing.T) {
	sentinel := errors.New("root cause")
	err := &tiering.AdapterError{
		Kind:    tiering.ErrTransient,
		Message: "backend unavailable",
		Cause:   sentinel,
	}

	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is(err, sentinel) = false; Unwrap must return Cause")
	}
	if !strings.Contains(err.Error(), sentinel.Error()) {
		t.Fatalf("Error() = %q; want it to include cause %q", err.Error(), sentinel.Error())
	}
}

func TestAdapterErrorNilCause(t *testing.T) {
	err := &tiering.AdapterError{Kind: tiering.ErrPermanent, Message: "no cause"}
	// Must not panic and must not include extra colon suffix.
	s := err.Error()
	if strings.HasSuffix(s, ": <nil>") {
		t.Fatalf("Error() should not include nil cause: %q", s)
	}
}
