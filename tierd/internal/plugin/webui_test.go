package plugin

import (
	"fmt"
	"testing"
)

func TestComposeDirectWebUI(t *testing.T) {
	for _, tc := range []struct {
		scheme, path, port, published string
		valid                         bool
	}{
		{"https", "/", "8007", "8007:8007", true},
		{"http", "/web", "8080", "8080:80", true},
		{"javascript", "/", "8007", "8007:8007", false},
		{"https", "//other-host/", "8007", "8007:8007", false},
		{"https", `/\\other-host`, "8007", "8007:8007", false},
		{"https", "/", "0", "8007:8007", false},
		{"https", "/", "65536", "8007:8007", false},
		{"https", "/", "8007", "8080:8007", false},
		{"https", "/", "8007", "8007:8007/udp", false},
	} {
		t.Run(fmt.Sprintf("%s-%s-%s-%s", tc.scheme, tc.path, tc.port, tc.published), func(t *testing.T) {
			project := fmt.Sprintf("name: web\nservices:\n  app:\n    image: nginx\n    ports: ['%s']\nx-smoothnas:\n  webUI:\n    scheme: %s\n    port: %s\n    path: '%s'\n", tc.published, tc.scheme, tc.port, tc.path)
			m, err := ParseManifest([]byte(project))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, err=%v", tc.valid, err)
			}
			if tc.valid && (m.WebUI == nil || m.UI != nil) {
				t.Fatal("direct web UI must not create an iframe target")
			}
		})
	}
}
