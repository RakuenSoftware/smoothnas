// Package plugin implements the SmoothNAS plugin system. Phase 1 owns
// the manifest schema, the SQLite tables, and a thin CLI surface that
// records installed plugins without running them. Phase 2 wires the
// LXC2Docker runtime daemon and brings plugins to "running".
package plugin

import (
	"errors"
	"fmt"
	"strings"
)

// ErrPluginNotFound is returned when an operation references a plugin
// name that has no row in the plugins table.
var ErrPluginNotFound = errors.New("plugin not found")

// ErrPluginExists is returned when Install is called for a plugin
// whose name already exists. Phase 1 has no upgrade flow; the
// operator uninstalls and reinstalls.
var ErrPluginExists = errors.New("plugin already exists")

// ValidationError is returned by ValidateManifest when one or more
// fields fail validation. It collects every problem so operators
// see all errors at once instead of one-at-a-time.
type ValidationError struct {
	Issues []ValidationIssue
}

// ValidationIssue names a single field-level problem.
type ValidationIssue struct {
	Field   string
	Message string
}

func (v *ValidationError) Error() string {
	if v == nil || len(v.Issues) == 0 {
		return "manifest validation: no issues"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "manifest validation: %d issue(s):", len(v.Issues))
	for _, iss := range v.Issues {
		fmt.Fprintf(&b, "\n  - %s: %s", iss.Field, iss.Message)
	}
	return b.String()
}

// add records a validation issue. Used internally by ValidateManifest.
func (v *ValidationError) add(field, format string, args ...any) {
	v.Issues = append(v.Issues, ValidationIssue{
		Field:   field,
		Message: fmt.Sprintf(format, args...),
	})
}

// asError returns nil when there are no issues, the *ValidationError
// otherwise. Callers should use this rather than checking len(Issues)
// to keep the nil-error idiom intact.
func (v *ValidationError) asError() error {
	if v == nil || len(v.Issues) == 0 {
		return nil
	}
	return v
}
