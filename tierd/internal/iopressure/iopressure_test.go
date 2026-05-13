package iopressure

import "testing"

func TestParsePSI(t *testing.T) {
	sample, err := Parse(`some avg10=31.52 avg60=30.93 avg300=33.09 total=123
full avg10=30.64 avg60=29.91 avg300=32.01 total=456
`)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if sample.SomeAvg10 != 31.52 || sample.FullAvg10 != 30.64 {
		t.Fatalf("unexpected avg10 values: %+v", sample)
	}
	if !sample.High(5) {
		t.Fatalf("sample should be high at threshold 5: %+v", sample)
	}
	if sample.High(40) {
		t.Fatalf("sample should not be high at threshold 40: %+v", sample)
	}
}

func TestParseRejectsMissingLines(t *testing.T) {
	if _, err := Parse("some avg10=0.00 avg60=0.00 avg300=0.00 total=1\n"); err == nil {
		t.Fatal("Parse succeeded without a full line")
	}
}
