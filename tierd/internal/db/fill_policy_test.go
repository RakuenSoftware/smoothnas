package db

import "testing"

func TestValidateFillPolicy(t *testing.T) {
	cases := []struct {
		name    string
		target  int
		full    int
		wantErr bool
	}{
		{"write-cache tier (target 0)", 0, 95, false},
		{"typical warm-fill", 65, 95, false},
		{"target just below full", 94, 95, false},
		{"target equals full", 95, 95, false},
		{"fully evacuated tier (0/0)", 0, 0, false},
		{"target above full", 96, 95, true},
		{"full below target", 50, 40, true},
		{"negative target", -1, 95, true},
		{"negative full", 0, -1, true},
		{"target over 100", 101, 95, true},
		{"full over 100", 0, 101, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateFillPolicy(c.target, c.full)
			if c.wantErr && err == nil {
				t.Fatalf("validateFillPolicy(%d, %d) = nil, want error", c.target, c.full)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("validateFillPolicy(%d, %d) = %v, want nil", c.target, c.full, err)
			}
		})
	}
}
