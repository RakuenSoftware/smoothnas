package smart

import "testing"

func TestParseDataHealth(t *testing.T) {
	cases := []struct {
		name          string
		json          string
		known, passed bool
	}{
		{"passed", `{"serial_number":"S1","smart_status":{"passed":true}}`, true, true},
		{"failed", `{"serial_number":"S1","smart_status":{"passed":false}}`, true, false},
		// A card reader or virtual device: smartctl answers, but with no
		// health result. This raised "SMART health check FAILED".
		{"no health result", `{"smartctl":{"exit_status":2,"messages":[{"string":"Unknown USB bridge","severity":"error"}]}}`, false, false},
	}
	for _, c := range cases {
		d, err := parseData("/dev/sdg", []byte(c.json))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if d.HealthKnown != c.known || d.HealthPassed != c.passed {
			t.Errorf("%s: known=%v passed=%v, want known=%v passed=%v", c.name, d.HealthKnown, d.HealthPassed, c.known, c.passed)
		}
	}
}
