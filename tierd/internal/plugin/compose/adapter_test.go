package compose

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// fakeRunner records the last invocation and returns canned output/err.
type fakeRunner struct {
	gotArgs []string
	gotEnv  []string
	stdout  []byte
	stderr  []byte
	err     error
}

func (f *fakeRunner) Run(_ context.Context, env []string, args ...string) ([]byte, []byte, error) {
	f.gotArgs = args
	f.gotEnv = env
	return f.stdout, f.stderr, f.err
}

func proj() Project {
	return Project{
		Name:     "aimee",
		Files:    []string{"compose.yaml", "compose.gpu.yaml"},
		Profiles: []string{"gpu"},
		EnvFile:  "/etc/smoothnas/aimee.env",
	}
}

func TestBaseArgs_OrderAndFlags(t *testing.T) {
	a := New("", &fakeRunner{})
	got := a.baseArgs(proj())
	want := []string{"compose", "-p", "aimee",
		"-f", "compose.yaml", "-f", "compose.gpu.yaml",
		"--profile", "gpu", "--env-file", "/etc/smoothnas/aimee.env"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("baseArgs=\n %v\nwant\n %v", got, want)
	}
}

func TestUp_PullNever(t *testing.T) {
	f := &fakeRunner{}
	if err := New("", f).Up(context.Background(), proj()); err != nil {
		t.Fatal(err)
	}
	// The op must end in `up -d --pull never`.
	tail := f.gotArgs[len(f.gotArgs)-4:]
	if !reflect.DeepEqual(tail, []string{"up", "-d", "--pull", "never"}) {
		t.Fatalf("up args tail=%v", tail)
	}
}

func TestEnv_PinsSocketAndDropsInherited(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://evil:2375")
	a := New("/run/smoothnas-runtime/docker.sock", &fakeRunner{})
	env := a.env()
	if env[0] != "DOCKER_HOST=unix:///run/smoothnas-runtime/docker.sock" {
		t.Fatalf("env[0]=%q; want pinned socket first", env[0])
	}
	for _, kv := range env[1:] {
		if kv == "DOCKER_HOST=tcp://evil:2375" {
			t.Fatal("inherited operator DOCKER_HOST leaked into compose env")
		}
	}
}

func TestVersion(t *testing.T) {
	cases := []struct {
		name    string
		stdout  string
		wantMaj int
		wantErr bool
	}{
		{"v2 ok", `{"version":"v2.29.7"}`, 2, false},
		{"v2 no-prefix", `{"version":"2.30.0"}`, 2, false},
		{"v1 rejected", `{"version":"1.29.2"}`, 1, true},
		{"garbage", `not json`, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			maj, err := New("", &fakeRunner{stdout: []byte(tc.stdout)}).Version(context.Background())
			if tc.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatal(err)
			}
			if tc.wantMaj != 0 && maj != tc.wantMaj {
				t.Fatalf("major=%d want %d", maj, tc.wantMaj)
			}
			if tc.name == "v1 rejected" && !errors.Is(err, ErrComposeVersion) {
				t.Fatalf("want ErrComposeVersion, got %v", err)
			}
		})
	}
}

func TestParsePs_NDJSONAndArray(t *testing.T) {
	nd := []byte(`{"Name":"aimee-web-1","Service":"web","State":"running","Health":"healthy","Publishers":[{"PublishedPort":8080,"TargetPort":80,"Protocol":"tcp"}]}
{"Name":"aimee-db-1","Service":"db","State":"running","Health":""}`)
	entries, err := parsePs(nd)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Service != "web" || entries[0].Publishers[0].PublishedPort != 8080 {
		t.Fatalf("ndjson parse=%+v", entries)
	}
	arr := []byte(`[{"Name":"x","Service":"web","State":"exited"}]`)
	e2, err := parsePs(arr)
	if err != nil || len(e2) != 1 || e2[0].State != "exited" {
		t.Fatalf("array parse=%+v err=%v", e2, err)
	}
	if e, _ := parsePs([]byte("  \n")); e != nil {
		t.Fatalf("empty should be nil, got %+v", e)
	}
}
