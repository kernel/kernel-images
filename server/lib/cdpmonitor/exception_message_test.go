package cdpmonitor

import "testing"

func TestExceptionMessage(t *testing.T) {
	cases := []struct {
		name     string
		exc      string
		fallback string
		want     string
	}{
		{"error description first line only", `{"className":"Error","description":"Error: boom\n    at <anonymous>:1:7"}`, "Uncaught", "Error: boom"},
		{"description without newline", `{"description":"TypeError: x is not a function"}`, "Uncaught", "TypeError: x is not a function"},
		{"thrown string value", `{"type":"string","value":"just a string"}`, "Uncaught", "just a string"},
		{"thrown number value", `{"type":"number","value":42}`, "Uncaught", "42"},
		{"empty exception falls back to text", ``, "Uncaught (in promise)", "Uncaught (in promise)"},
		{"malformed json falls back to text", `not json`, "Uncaught", "Uncaught"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exceptionMessage([]byte(tc.exc), tc.fallback); got != tc.want {
				t.Fatalf("exceptionMessage(%q) = %q, want %q", tc.exc, got, tc.want)
			}
		})
	}
}
