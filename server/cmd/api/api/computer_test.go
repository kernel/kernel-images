package api

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func sumSteps(steps [][2]int) (int, int) {
	sx, sy := 0, 0
	for _, s := range steps {
		sx += s[0]
		sy += s[1]
	}
	return sx, sy
}

func countSteps(steps [][2]int) int { return len(steps) }

func TestGenerateRelativeSteps_Zero(t *testing.T) {
	steps := generateRelativeSteps(0, 0, 5)
	require.Len(t, steps, 0, "expected 0 steps")
}

func TestGenerateRelativeSteps_AxisAligned(t *testing.T) {
	cases := []struct {
		dx, dy int
	}{
		{5, 0}, {-7, 0}, {0, 9}, {0, -3},
	}
	for _, c := range cases {
		steps := generateRelativeSteps(c.dx, c.dy, 5)
		sx, sy := sumSteps(steps)
		require.Equal(t, c.dx, sx, "sum mismatch dx")
		require.Equal(t, c.dy, sy, "sum mismatch dy")
		require.Equal(t, 5, countSteps(steps), "count mismatch")
	}
}

func TestGenerateRelativeSteps_DiagonalsAndSlopes(t *testing.T) {
	cases := []struct{ dx, dy int }{
		{5, 5}, {-4, -4}, {8, 3}, {3, 8}, {-9, 2}, {2, -9},
	}
	for _, c := range cases {
		steps := generateRelativeSteps(c.dx, c.dy, 5)
		sx, sy := sumSteps(steps)
		require.Equal(t, c.dx, sx, "sum mismatch dx")
		require.Equal(t, c.dy, sy, "sum mismatch dy")
		require.Equal(t, 5, countSteps(steps), "count mismatch")
	}
}

// TestParseMousePosition tests the parseMousePosition helper function
func TestParseMousePosition(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		expectX     int
		expectY     int
		expectError bool
	}{
		{
			name:        "valid output",
			output:      "X=100\nY=200\nSCREEN=0\nWINDOW=12345\n",
			expectX:     100,
			expectY:     200,
			expectError: false,
		},
		{
			name:        "valid output with extra whitespace",
			output:      "  X=512  \n  Y=384  \n  SCREEN=0  \n  WINDOW=67890  \n",
			expectX:     512,
			expectY:     384,
			expectError: false,
		},
		{
			name:        "missing Y coordinate",
			output:      "X=100\nSCREEN=0\nWINDOW=12345\n",
			expectError: true,
		},
		{
			name:        "missing X coordinate",
			output:      "Y=200\nSCREEN=0\nWINDOW=12345\n",
			expectError: true,
		},
		{
			name:        "empty output",
			output:      "",
			expectError: true,
		},
		{
			name:        "whitespace only",
			output:      "   \n  \t  \n",
			expectError: true,
		},
		{
			name:        "non-numeric X value",
			output:      "X=abc\nY=200\nSCREEN=0\nWINDOW=12345\n",
			expectError: true,
		},
		{
			name:        "non-numeric Y value",
			output:      "X=100\nY=xyz\nSCREEN=0\nWINDOW=12345\n",
			expectError: true,
		},
		{
			name:        "zero coordinates",
			output:      "X=0\nY=0\nSCREEN=0\nWINDOW=12345\n",
			expectX:     0,
			expectY:     0,
			expectError: false,
		},
		{
			name:        "negative coordinates",
			output:      "X=-50\nY=-100\nSCREEN=0\nWINDOW=12345\n",
			expectX:     -50,
			expectY:     -100,
			expectError: false,
		},
		{
			name:        "large coordinates",
			output:      "X=3840\nY=2160\nSCREEN=0\nWINDOW=12345\n",
			expectX:     3840,
			expectY:     2160,
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x, y, err := parseMousePosition(tt.output)

			if tt.expectError {
				require.Error(t, err, "expected parsing to fail")
			} else {
				require.NoError(t, err, "expected successful parsing")
				require.Equal(t, tt.expectX, x, "X coordinate mismatch")
				require.Equal(t, tt.expectY, y, "Y coordinate mismatch")
			}
		})
	}
}
