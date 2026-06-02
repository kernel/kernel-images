package main

import (
	"os"
	"testing"
)

func TestNormalizeChromiumFlags(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no backslash is a no-op",
			in:   `--no-sandbox --disable-gpu`,
			want: `--no-sandbox --disable-gpu`,
		},
		{
			name: "escaped spaces (ukp 0.10.0)",
			in:   `--no-sandbox\ --disable-gpu`,
			want: `--no-sandbox --disable-gpu`,
		},
		{
			name: "escaped quotes and commas",
			in:   `--lang=\"en-US\,en;q=0.9\"\ --disable-features=A\,B\,C`,
			want: `--lang="en-US,en;q=0.9" --disable-features=A,B,C`,
		},
		{
			name: "doubled backslash collapses to one",
			in:   `--flag=a\\b`,
			want: `--flag=a\b`,
		},
		{
			name: "lone trailing backslash is preserved",
			in:   `--flag\`,
			want: `--flag\`,
		},
		{
			name: "empty",
			in:   ``,
			want: ``,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CHROMIUM_FLAGS", tt.in)
			normalizeChromiumFlags()
			if got := os.Getenv("CHROMIUM_FLAGS"); got != tt.want {
				t.Errorf("normalizeChromiumFlags() = %q, want %q", got, tt.want)
			}
		})
	}
}
