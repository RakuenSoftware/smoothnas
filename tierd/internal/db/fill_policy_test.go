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
		{"target equals full", 95, 95, true},
		{"target above full", 96, 95, true},
		{"negative target", -1, 95, true},
		{"target over 100", 101, 95, true},
		{"full zero is invalid", 0, 0, true},
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
