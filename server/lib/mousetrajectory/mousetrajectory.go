package mousetrajectory

import (
	"math"
	"math/rand"
)

// HumanizeMouseTrajectory generates human-like mouse movement points from (fromX, fromY)
// to (toX, toY) using Bezier curves with randomized control points, distortion, and easing.
//
// Ported from Camoufox MouseTrajectories.hpp, which was adapted from:
// https://github.com/riflosnake/HumanCursor/blob/main/humancursor/utilities/human_curve_generator.py
type HumanizeMouseTrajectory struct {
	fromX, fromY float64
	toX, toY     float64
	points       [][2]float64
	rng          *rand.Rand
}

// Options configures trajectory generation.
type Options struct {
	// MaxPoints overrides the auto-computed point count. 0 = auto. Range 5-80.
	MaxPoints int
}

// NewHumanizeMouseTrajectory creates a trajectory from (fromX, fromY) to (toX, toY).
// Uses the default entropy source for randomization.
func NewHumanizeMouseTrajectory(fromX, fromY, toX, toY float64) *HumanizeMouseTrajectory {
	return NewHumanizeMouseTrajectoryWithOptions(fromX, fromY, toX, toY, nil)
}

// NewHumanizeMouseTrajectoryWithOptions creates a trajectory with optional overrides.
func NewHumanizeMouseTrajectoryWithOptions(fromX, fromY, toX, toY float64, opts *Options) *HumanizeMouseTrajectory {
	t := &HumanizeMouseTrajectory{
		fromX: fromX, fromY: fromY,
		toX: toX, toY: toY,
		rng: rand.New(rand.NewSource(rand.Int63())),
	}
	t.generateCurve(opts)
	return t
}

// NewHumanizeMouseTrajectoryWithSeed creates a trajectory with a fixed seed (for tests).
func NewHumanizeMouseTrajectoryWithSeed(fromX, fromY, toX, toY float64, seed int64) *HumanizeMouseTrajectory {
	t := &HumanizeMouseTrajectory{
		fromX: fromX, fromY: fromY,
		toX: toX, toY: toY,
		rng: rand.New(rand.NewSource(seed)),
	}
	t.generateCurve(nil)
	return t
}

// GetPoints returns the trajectory as a slice of [x, y] pairs (floats, caller rounds).
func (t *HumanizeMouseTrajectory) GetPoints() [][2]float64 {
	return t.points
}

// GetPointsInt returns the trajectory as integer coordinates suitable for xdotool.
func (t *HumanizeMouseTrajectory) GetPointsInt() [][2]int {
	out := make([][2]int, len(t.points))
	for i, p := range t.points {
		out[i][0] = int(math.Round(p[0]))
		out[i][1] = int(math.Round(p[1]))
	}
	return out
}

func (t *HumanizeMouseTrajectory) generateCurve(opts *Options) {
	left := math.Min(t.fromX, t.toX) - 80
	right := math.Max(t.fromX, t.toX) + 80
	down := math.Min(t.fromY, t.toY) - 80
	up := math.Max(t.fromY, t.toY) + 80

	knots := t.generateInternalKnots(left, right, down, up, 2)
	curvePoints := t.generatePoints(knots)
	curvePoints = t.distortPoints(curvePoints, 1.0, 1.0, 0.5)
	t.points = t.tweenPoints(curvePoints, opts)
}

func (t *HumanizeMouseTrajectory) generateInternalKnots(l, r, d, u float64, knotsCount int) [][2]float64 {
	knotsX := t.randomChoiceDoubles(l, r, knotsCount)
	knotsY := t.randomChoiceDoubles(d, u, knotsCount)
	knots := make([][2]float64, knotsCount)
	for i := 0; i < knotsCount; i++ {
		knots[i] = [2]float64{knotsX[i], knotsY[i]}
	}
	return knots
}

func (t *HumanizeMouseTrajectory) randomChoiceDoubles(min, max float64, size int) []float64 {
	out := make([]float64, size)
	for i := 0; i < size; i++ {
		out[i] = min + t.rng.Float64()*(max-min)
	}
	return out
}

func factorial(n int) int64 {
	if n < 0 {
		return -1
	}
	result := int64(1)
	for i := 2; i <= n; i++ {
		result *= int64(i)
	}
	return result
}

func binomial(n, k int) float64 {
	return float64(factorial(n)) / (float64(factorial(k)) * float64(factorial(n-k)))
}

func bernsteinPolynomialPoint(x float64, i, n int) float64 {
	return binomial(n, i) * math.Pow(x, float64(i)) * math.Pow(1-x, float64(n-i))
}

func bernsteinPolynomial(points [][2]float64, t float64) [2]float64 {
	n := len(points) - 1
	var x, y float64
	for i := 0; i <= n; i++ {
		bern := bernsteinPolynomialPoint(t, i, n)
		x += points[i][0] * bern
		y += points[i][1] * bern
	}
	return [2]float64{x, y}
}

func (t *HumanizeMouseTrajectory) generatePoints(knots [][2]float64) [][2]float64 {
	midPtsCnt := int(math.Max(math.Max(math.Abs(t.fromX-t.toX), math.Abs(t.fromY-t.toY)), 2))
	controlPoints := make([][2]float64, 0, len(knots)+2)
	controlPoints = append(controlPoints, [2]float64{t.fromX, t.fromY})
	controlPoints = append(controlPoints, knots...)
	controlPoints = append(controlPoints, [2]float64{t.toX, t.toY})

	curvePoints := make([][2]float64, midPtsCnt)
	for i := 0; i < midPtsCnt; i++ {
		tt := float64(i) / float64(midPtsCnt-1)
		curvePoints[i] = bernsteinPolynomial(controlPoints, tt)
	}
	return curvePoints
}

func (t *HumanizeMouseTrajectory) distortPoints(points [][2]float64, distortionMean, distortionStDev, distortionFreq float64) [][2]float64 {
	if len(points) < 3 {
		return points
	}
	distorted := make([][2]float64, len(points))
	distorted[0] = points[0]

	for i := 1; i < len(points)-1; i++ {
		x, y := points[i][0], points[i][1]
		if t.rng.Float64() < distortionFreq {
			delta := math.Round(normalDist(t.rng, distortionMean, distortionStDev))
			y += delta
		}
		distorted[i] = [2]float64{x, y}
	}
	distorted[len(points)-1] = points[len(points)-1]
	return distorted
}

func normalDist(rng *rand.Rand, mean, stdDev float64) float64 {
	// Box-Muller transform
	u1 := rng.Float64()
	u2 := rng.Float64()
	if u1 <= 0 {
		u1 = 1e-10
	}
	return mean + stdDev*math.Sqrt(-2*math.Log(u1))*math.Cos(2*math.Pi*u2)
}

func (t *HumanizeMouseTrajectory) easeOutQuad(n float64) float64 {
	return -n * (n - 2)
}

const (
	defaultMaxTime = 150
	defaultMinTime = 0
)

func (t *HumanizeMouseTrajectory) tweenPoints(points [][2]float64, opts *Options) [][2]float64 {
	var totalLength float64
	for i := 1; i < len(points); i++ {
		dx := points[i][0] - points[i-1][0]
		dy := points[i][1] - points[i-1][1]
		totalLength += math.Sqrt(dx*dx + dy*dy)
	}

	targetPoints := int(math.Min(
		float64(defaultMaxTime),
		math.Max(float64(defaultMinTime+2), math.Pow(totalLength, 0.25)*20)))

	if opts != nil && opts.MaxPoints > 0 {
		if opts.MaxPoints < 5 {
			opts.MaxPoints = 5
		}
		if opts.MaxPoints > 80 {
			opts.MaxPoints = 80
		}
		targetPoints = opts.MaxPoints
	}

	if targetPoints < 2 {
		targetPoints = 2
	}

	res := make([][2]float64, targetPoints)
	for i := 0; i < targetPoints; i++ {
		tt := float64(i) / float64(targetPoints-1)
		easedT := t.easeOutQuad(tt)
		idx := int(easedT * float64(len(points)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(points) {
			idx = len(points) - 1
		}
		res[i] = points[idx]
	}
	return res
}
