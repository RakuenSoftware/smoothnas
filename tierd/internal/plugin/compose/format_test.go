package compose

import "testing"

func TestIsComposeFormat(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want bool
	}{
		{"compose project", "name: aimee\nservices:\n  web:\n    image: nginx\n", true},
		{"compose no name", "services:\n  db:\n    image: postgres\n", true},
		{"smoothnas manifest", "apiVersion: smoothnas.io/v1\nkind: Plugin\nmetadata:\n  name: x\nservices:\n  - name: web\n", false},
		{"empty", "", false},
		{"garbage", "\t\tnot: [valid", false},
		{"services not a map (compose list-form is invalid)", "services:\n  - image: nginx\n", false},
	}
	for _, tc := range cases {
		if got := IsComposeFormat([]byte(tc.yaml)); got != tc.want {
			t.Errorf("%s: IsComposeFormat=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestProjectName(t *testing.T) {
	if got := ProjectName([]byte("name: wolf\nservices: {}\n")); got != "wolf" {
		t.Fatalf("ProjectName=%q", got)
	}
	if got := ProjectName([]byte("services: {}\n")); got != "" {
		t.Fatalf("ProjectName=%q want empty", got)
	}
}

func TestSpecFromSingle(t *testing.T) {
	s := SpecFromSingle("aimee", "services: {}\n", map[string]string{"K": "v"})
	if s.Name != "aimee" || s.Files["compose.yaml"] == "" || len(s.FileOrder) != 1 || s.Env["K"] != "v" {
		t.Fatalf("spec=%+v", s)
	}
}
