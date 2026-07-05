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
