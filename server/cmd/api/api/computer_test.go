package api

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
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

func TestValidationError(t *testing.T) {
	ve := &validationError{msg: "bad input"}
	assert.Equal(t, "bad input", ve.Error())
	assert.True(t, isValidationErr(ve))

	// Wrapped validation error should still be detected
	wrapped := fmt.Errorf("context: %w", ve)
	assert.True(t, isValidationErr(wrapped))
}

func TestExecutionError(t *testing.T) {
	ee := &executionError{msg: "xdotool failed"}
	assert.Equal(t, "xdotool failed", ee.Error())
	assert.False(t, isValidationErr(ee))

	// A plain error is not a validation error
	plain := errors.New("something went wrong")
	assert.False(t, isValidationErr(plain))
}

func TestIsValidationErr_Nil(t *testing.T) {
	assert.False(t, isValidationErr(nil))
}

func TestGaussianDelay(t *testing.T) {
	const n = 10000
	meanMs := 10

	var sum, sumSq float64
	minVal, maxVal := math.MaxFloat64, -math.MaxFloat64

	for i := 0; i < n; i++ {
		d := float64(gaussianDelay(meanMs, 3))
		sum += d
		sumSq += d * d
		if d < minVal {
			minVal = d
		}
		if d > maxVal {
			maxVal = d
		}
	}

	avg := sum / n
	variance := sumSq/n - avg*avg

	assert.InDelta(t, float64(meanMs), avg, float64(meanMs)*0.15,
		"average delay should be near %dms, got %.1fms", meanMs, avg)

	assert.Greater(t, variance, 5.0,
		"variance should be substantial for human-like timing, got %.1f", variance)

	assert.GreaterOrEqual(t, minVal, 3.0, "delay must not go below floor")

	assert.LessOrEqual(t, maxVal, float64(meanMs*3), "delay must not exceed 3x mean")
}

func TestGaussianDelay_VarianceMuchHigherThanUniform(t *testing.T) {
	const n = 5000
	meanMs := 10

	var gSum, gSumSq float64
	for i := 0; i < n; i++ {
		d := float64(gaussianDelay(meanMs, 3))
		gSum += d
		gSumSq += d * d
	}
	gAvg := gSum / n
	gVariance := gSumSq/n - gAvg*gAvg

	// Old uniform: meanMs + rand.Intn(5) - 2, variance of {-2,-1,0,1,2} = 2.0
	uniformVariance := 2.0

	assert.Greater(t, gVariance, uniformVariance*3,
		"Gaussian variance (%.1f) should be much larger than old uniform variance (%.1f)",
		gVariance, uniformVariance)
}

// welford implements Welford's online algorithm for computing running variance.
// This is the same algorithm used by browser fingerprinting systems to evaluate
// whether mouse movement timing looks human or automated.
type welford struct {
	n    int
	mean float64
	m2   float64
}

func (w *welford) add(v float64) {
	w.n++
	delta := v - w.mean
	w.mean += delta / float64(w.n)
	w.m2 += delta * (v - w.mean)
}

func (w *welford) variance() float64 {
	if w.n < 2 {
		return 0
	}
	return w.m2 / float64(w.n-1)
}

func TestGaussianDelay_WelfordVelocityVariance(t *testing.T) {
	// Simulate a mouse trajectory: 50 points with varying pixel distances
	// (as produced by a Bezier curve), timed with gaussianDelay intervals.
	// Compute velocity = distance / delay for each step and measure Welford
	// variance. Human-like velocity variance should be well above 5.
	const steps = 50
	meanDelayMs := 10

	// Pixel distances per step typical of a Bezier curve across ~400px.
	// Real trajectories vary: small moves near endpoints, larger in the middle.
	rng := rand.New(rand.NewSource(42))
	distances := make([]float64, steps)
	for i := range distances {
		t_norm := float64(i) / float64(steps)
		base := 5.0 + 15.0*math.Sin(t_norm*math.Pi) // 5-20px, peaked in middle
		distances[i] = base + rng.Float64()*3.0     // small random variation
	}

	// Gaussian delays → velocity variance
	var gaussianVelVar welford
	for i := 0; i < steps; i++ {
		delay := float64(gaussianDelay(meanDelayMs, 3))
		velocity := distances[i] / delay
		gaussianVelVar.add(velocity)
	}

	// Uniform delays (old approach) → velocity variance
	var uniformVelVar welford
	for i := 0; i < steps; i++ {
		delay := float64(meanDelayMs + rand.Intn(5) - 2)
		if delay < 3 {
			delay = 3
		}
		velocity := distances[i] / delay
		uniformVelVar.add(velocity)
	}

	t.Logf("Gaussian velocity variance: %.4f (n=%d)", gaussianVelVar.variance(), gaussianVelVar.n)
	t.Logf("Uniform velocity variance:  %.4f (n=%d)", uniformVelVar.variance(), uniformVelVar.n)

	assert.Greater(t, gaussianVelVar.variance(), uniformVelVar.variance(),
		"Gaussian timing should produce higher velocity variance than uniform")

	assert.Greater(t, gaussianVelVar.variance(), 0.05,
		"Gaussian velocity variance should be well above near-zero")
}

func TestClampPoints(t *testing.T) {
	tests := []struct {
		name     string
		points   [][2]int
		w, h     int
		expected [][2]int
	}{
		{
			name:   "no clamping needed",
			points: [][2]int{{10, 20}, {50, 50}, {100, 80}},
			w:      200, h: 200,
			expected: [][2]int{{10, 20}, {50, 50}, {100, 80}},
		},
		{
			name:   "clamp negative x and y",
			points: [][2]int{{-10, -20}, {50, 50}},
			w:      200, h: 200,
			expected: [][2]int{{0, 0}, {50, 50}},
		},
		{
			name:   "clamp exceeding screen bounds",
			points: [][2]int{{50, 50}, {250, 300}},
			w:      200, h: 200,
			expected: [][2]int{{50, 50}, {199, 199}},
		},
		{
			name:   "clamp both directions",
			points: [][2]int{{-5, 250}, {300, -10}, {100, 100}},
			w:      200, h: 200,
			expected: [][2]int{{0, 199}, {199, 0}, {100, 100}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clampPoints(tt.points, tt.w, tt.h)
			require.Equal(t, tt.expected, tt.points)
		})
	}
}
