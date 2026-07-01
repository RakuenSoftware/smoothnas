package compose

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// scriptRunner dispatches by the compose subcommand so one runner can serve a
// full Materialise (version -> pull) + Start (up) + Status (ps) sequence.
type scriptRunner struct {
	psOut []byte
	calls [][]string
}

func (r *scriptRunner) Run(_ context.Context, _ []string, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, args)
	switch {
	case has(args, "version"):
		return []byte(`{"version":"v2.29.7"}`), nil, nil
	case has(args, "ps"):
		return r.psOut, nil, nil
	default:
		return nil, nil, nil
	}
}

func has(args []string, sub string) bool {
	for _, a := range args {
		if a == sub {
			return true
		}
	}
	return false
}

func spec(name string) ProjectSpec {
	return ProjectSpec{
		Name:      name,
		Files:     map[string]string{"compose.yaml": "services:\n  web:\n    image: nginx\n"},
		FileOrder: []string{"compose.yaml"},
		Env:       map[string]string{"FOO": "bar", "AAA": "1"},
	}
}

func TestBackend_MaterialiseWritesFilesAndPulls(t *testing.T) {
	root := t.TempDir()
	r := &scriptRunner{}
	b := NewBackend(New("", r), root)

	if err := b.Materialise(context.Background(), spec("aimee")); err != nil {
		t.Fatal(err)
	}
	// compose.yaml + .env written under <root>/aimee/
	if _, err := os.Stat(filepath.Join(root, "aimee", "compose.yaml")); err != nil {
		t.Fatalf("compose.yaml not written: %v", err)
	}
	envBytes, err := os.ReadFile(filepath.Join(root, "aimee", ".env"))
	if err != nil {
		t.Fatalf(".env not written: %v", err)
	}
	if got := string(envBytes); got != "AAA=1\nFOO=bar\n" { // sorted keys
		t.Fatalf(".env=%q", got)
	}
	// Must have asserted version then pulled (explicit pre-pull).
	if len(r.calls) != 2 || !has(r.calls[0], "version") || !has(r.calls[1], "pull") {
		t.Fatalf("calls=%v; want version then pull", r.calls)
	}
}

func TestBackend_TeardownRemovesDir(t *testing.T) {
	root := t.TempDir()
	b := NewBackend(New("", &scriptRunner{}), root)
	s := spec("wolf")
	if err := b.Materialise(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err := b.Teardown(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.dir("wolf")); !os.IsNotExist(err) {
		t.Fatalf("project dir should be gone, stat err=%v", err)
	}
}

func TestBackend_StatusDerivesOverall(t *testing.T) {
	root := t.TempDir()
	r := &scriptRunner{psOut: []byte(
		`{"Name":"a","Service":"web","State":"running","Health":"healthy"}` + "\n" +
			`{"Name":"b","Service":"db","State":"running","Health":""}`)}
	b := NewBackend(New("", r), root)
	st, err := b.Status(context.Background(), spec("aimee"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Overall != StateRunning || len(st.Services) != 2 {
		t.Fatalf("status=%+v", st)
	}
}

func TestDerive(t *testing.T) {
	cases := []struct {
		name string
		in   []PsEntry
		want Overall
	}{
		{"empty", nil, StateStopped},
		{"all running", []PsEntry{{State: "running"}, {State: "running", Health: "healthy"}}, StateRunning},
		{"one exited", []PsEntry{{State: "running"}, {State: "exited"}}, StateFailed},
		{"unhealthy is degraded", []PsEntry{{State: "running", Health: "unhealthy"}, {State: "running"}}, StateDegraded},
		{"one not-yet-running", []PsEntry{{State: "running"}, {State: "created"}}, StateDegraded},
	}
	for _, tc := range cases {
		if got := derive(tc.in); got != tc.want {
			t.Errorf("%s: derive=%s want %s", tc.name, got, tc.want)
		}
	}
}
