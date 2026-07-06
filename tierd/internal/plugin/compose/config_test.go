package compose

import "testing"

func TestConfigSchema_ParsesAndValidates(t *testing.T) {
	y := `name: p
services:
  s: { image: x }
x-smoothnas:
  config:
    - { key: LLM_ENDPOINT, label: LLM, type: url, default: "" }
    - { key: LLM_MODEL, type: string }
    - { key: WEBCHAT_PASSWORD, label: pw, secret: true }
`
	got, err := ConfigSchema([]byte(y))
	if err != nil {
		t.Fatalf("ConfigSchema: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Type != "url" || got[1].Type != "string" || !got[2].Secret {
		t.Errorf("schema = %+v", got)
	}
}

func TestConfigSchema_Rejects(t *testing.T) {
	cases := map[string]string{
		"dup key":             "name: p\nservices: {s: {image: x}}\nx-smoothnas:\n  config:\n    - {key: A}\n    - {key: A}\n",
		"unknown type":        "name: p\nservices: {s: {image: x}}\nx-smoothnas:\n  config:\n    - {key: A, type: blob}\n",
		"cred key not secret": "name: p\nservices: {s: {image: x}}\nx-smoothnas:\n  config:\n    - {key: API_TOKEN}\n",
		"default on secret":   "name: p\nservices: {s: {image: x}}\nx-smoothnas:\n  config:\n    - {key: PW, secret: true, default: hunter2}\n",
		"empty key":           "name: p\nservices: {s: {image: x}}\nx-smoothnas:\n  config:\n    - {label: nope}\n",
	}
	for name, y := range cases {
		if _, err := ConfigSchema([]byte(y)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestResolveConfigEnv_DefaultsAndSplit(t *testing.T) {
	schema := []ConfigDecl{
		{Key: "LLM_ENDPOINT", Type: "url", Default: "http://d:8080"},
		{Key: "LLM_MODEL", Type: "string", Default: "def"},
		{Key: "PW", Secret: true},
	}
	answers := map[string]string{"LLM_MODEL": "custom", "PW": "s3cr3t"}
	env, secretEnv, err := ResolveConfigEnv(schema, answers)
	if err != nil {
		t.Fatalf("ResolveConfigEnv: %v", err)
	}
	if env["LLM_ENDPOINT"] != "http://d:8080" { // default materialized
		t.Errorf("endpoint = %q, want default", env["LLM_ENDPOINT"])
	}
	if env["LLM_MODEL"] != "custom" { // operator answer wins
		t.Errorf("model = %q, want custom", env["LLM_MODEL"])
	}
	if _, inEnv := env["PW"]; inEnv { // secret never in .env
		t.Error("secret leaked into env")
	}
	if secretEnv["PW"] != "s3cr3t" {
		t.Errorf("secretEnv[PW] = %q", secretEnv["PW"])
	}
}

func TestResolveConfigEnv_ValidatesTypes(t *testing.T) {
	for _, tc := range []struct {
		typ, val string
	}{{"number", "abc"}, {"bool", "yes"}, {"url", "file:///etc"}, {"string", "a\nb"}} {
		schema := []ConfigDecl{{Key: "K", Type: tc.typ}}
		if _, _, err := ResolveConfigEnv(schema, map[string]string{"K": tc.val}); err == nil {
			t.Errorf("type %s val %q: expected error", tc.typ, tc.val)
		}
	}
}

func TestConfigSchema_MinMax(t *testing.T) {
	y := "name: p\nservices: {s: {image: x}}\nx-smoothnas:\n  config:\n    - {key: CTX, type: number, default: \"262144\", min: \"4096\", max: \"1048576\", unit: tokens}\n"
	got, err := ConfigSchema([]byte(y))
	if err != nil {
		t.Fatalf("ConfigSchema: %v", err)
	}
	if got[0].Min != "4096" || got[0].Max != "1048576" || got[0].Unit != "tokens" {
		t.Fatalf("bounds not parsed: %+v", got[0])
	}
	// below min / above max rejected; in-range accepted
	if _, _, err := ResolveConfigEnv(got, map[string]string{"CTX": "1000"}); err == nil {
		t.Error("expected reject below min")
	}
	if _, _, err := ResolveConfigEnv(got, map[string]string{"CTX": "9999999"}); err == nil {
		t.Error("expected reject above max")
	}
	if _, _, err := ResolveConfigEnv(got, map[string]string{"CTX": "8192"}); err != nil {
		t.Errorf("in-range rejected: %v", err)
	}
}

func TestResolveConfigEnv_AcceptsFloat(t *testing.T) {
	s := []ConfigDecl{{Key: "CPUS", Type: "number", Min: "0", Default: "0"}}
	if _, _, err := ResolveConfigEnv(s, map[string]string{"CPUS": "0.25"}); err != nil {
		t.Fatalf("float rejected: %v", err)
	}
	if _, _, err := ResolveConfigEnv(s, map[string]string{"CPUS": "-1"}); err == nil {
		t.Error("expected reject below min 0")
	}
}

func TestConfigSchema_Select(t *testing.T) {
	y := "name: p\nservices: {s: {image: x}}\nx-smoothnas:\n  config:\n    - {key: Q, type: select, default: q8_0, options: [{value: q8_0}, {value: q4_0}]}\n"
	got, err := ConfigSchema([]byte(y))
	if err != nil {
		t.Fatalf("ConfigSchema: %v", err)
	}
	if len(got[0].Options) != 2 {
		t.Fatalf("options not parsed: %+v", got[0])
	}
	if _, _, err := ResolveConfigEnv(got, map[string]string{"Q": "q4_0"}); err != nil {
		t.Errorf("valid option rejected: %v", err)
	}
	if _, _, err := ResolveConfigEnv(got, map[string]string{"Q": "q2_0"}); err == nil {
		t.Error("expected reject out-of-set value")
	}
	// select without options must fail schema
	if _, err := ConfigSchema([]byte("name: p\nservices: {s: {image: x}}\nx-smoothnas:\n  config:\n    - {key: Q, type: select}\n")); err == nil {
		t.Error("expected error: select without options")
	}
}

func TestConfigValue_RejectsNonFiniteAndAcceptsBooleanAlias(t *testing.T) {
	num := []ConfigDecl{{Key: "N", Type: "number"}}
	for _, bad := range []string{"NaN", "Inf", "-Inf"} {
		if _, _, err := ResolveConfigEnv(num, map[string]string{"N": bad}); err == nil {
			t.Errorf("expected reject %q", bad)
		}
	}
	if _, _, err := ResolveConfigEnv(num, map[string]string{"N": "262144"}); err != nil {
		t.Errorf("integer rejected: %v", err)
	}
	// native "boolean" spelling normalizes to bool
	s, err := ConfigSchema([]byte("name: p\nservices: {s: {image: x}}\nx-smoothnas:\n  config:\n    - {key: B, type: boolean, default: \"true\"}\n"))
	if err != nil {
		t.Fatalf("boolean alias: %v", err)
	}
	if s[0].Type != "bool" {
		t.Errorf("boolean not normalized to bool: %q", s[0].Type)
	}
}
