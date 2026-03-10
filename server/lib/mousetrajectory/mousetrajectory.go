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

// GetPointsInt returns the trajectory as integer coordinates suitable for xdotool.
func (t *HumanizeMouseTrajectory) GetPointsInt() [][2]int {
	out := make([][2]int, len(t.points))
	for i, p := range t.points {
		out[i][0] = int(math.Round(p[0]))
		out[i][1] = int(math.Round(p[1]))
	}
	return out
}

// MultiSegmentResult holds the generated trajectory and the per-step delay.
type MultiSegmentResult struct {
	Points      [][2]int
	StepDelayMs int
}

const defaultStepDelayMs = 10

// GenerateMultiSegmentTrajectory creates a human-like Bezier trajectory through
// a sequence of waypoints. Each consecutive pair gets its own Bezier curve, with
// point counts distributed proportionally to segment distance. The resulting
// points are clamped to [0, screenW-1] x [0, screenH-1].
func GenerateMultiSegmentTrajectory(waypoints [][2]int, screenW, screenH int, totalDurationMs *int) MultiSegmentResult {
	if len(waypoints) < 2 {
		return MultiSegmentResult{Points: waypoints, StepDelayMs: defaultStepDelayMs}
	}

	segDistances := make([]float64, len(waypoints)-1)
	var totalDist float64
	for i := 1; i < len(waypoints); i++ {
		dx := float64(waypoints[i][0] - waypoints[i-1][0])
		dy := float64(waypoints[i][1] - waypoints[i-1][1])
		d := math.Sqrt(dx*dx + dy*dy)
		segDistances[i-1] = d
		totalDist += d
	}

	// Determine total number of points across all segments.
	var totalPoints int
	if totalDurationMs != nil && *totalDurationMs > 0 {
		totalPoints = *totalDurationMs / defaultStepDelayMs
		if totalPoints < MinPoints {
			totalPoints = MinPoints
		}
	} else {
		totalPoints = int(math.Min(
			float64(defaultMaxPoints)*float64(len(waypoints)-1),
			math.Max(float64(MinPoints), math.Pow(totalDist, 0.25)*pathLengthScale*float64(len(waypoints)-1))))
	}

	var allPoints [][2]int

	for i := 0; i < len(waypoints)-1; i++ {
		// Distribute points proportionally to segment distance.
		var segPoints int
		if totalDist > 0 {
			segPoints = int(math.Round(float64(totalPoints) * segDistances[i] / totalDist))
		} else {
			segPoints = totalPoints / (len(waypoints) - 1)
		}
		if segPoints < MinPoints {
			segPoints = MinPoints
		}
		if segPoints > MaxPoints {
			segPoints = MaxPoints
		}

		opts := &Options{MaxPoints: segPoints}
		traj := NewHumanizeMouseTrajectoryWithOptions(
			float64(waypoints[i][0]), float64(waypoints[i][1]),
			float64(waypoints[i+1][0]), float64(waypoints[i+1][1]),
			opts,
		)
		segPts := traj.GetPointsInt()

		if i == 0 {
			allPoints = append(allPoints, segPts...)
		} else {
			// Skip first point of subsequent segments to avoid duplicates at junctions.
			if len(segPts) > 1 {
				allPoints = append(allPoints, segPts[1:]...)
			}
		}
	}

	// Clamp to screen bounds.
	clampPoints(allPoints, screenW, screenH)

	stepDelay := defaultStepDelayMs
	if totalDurationMs != nil && len(allPoints) > 1 {
		stepDelay = *totalDurationMs / (len(allPoints) - 1)
		if stepDelay < 3 {
			stepDelay = 3
		}
	}

	return MultiSegmentResult{Points: allPoints, StepDelayMs: stepDelay}
}

// clampPoints constrains each point to [0, screenW-1] x [0, screenH-1].
func clampPoints(points [][2]int, screenW, screenH int) {
	maxX := screenW - 1
	maxY := screenH - 1
	for i := range points {
		if points[i][0] < 0 {
			points[i][0] = 0
		} else if points[i][0] > maxX {
			points[i][0] = maxX
		}
		if points[i][1] < 0 {
			points[i][1] = 0
		} else if points[i][1] > maxY {
			points[i][1] = maxY
		}
	}
}

const (
	// Bounds padding for Bezier control point region (pixels beyond start/end).
	boundsPadding = 80
	// Number of internal knots for the Bezier curve (more = curvier).
	knotsCount = 2
	// Distortion parameters for human-like jitter: mean, stdev, frequency.
	distortionMean  = 1.0
	distortionStDev = 1.0
	distortionFreq  = 0.5
)

const (
	defaultMaxPoints = 150 // Upper bound for auto-computed point count
	defaultMinPoints = 0   // Lower bound for auto-computed point count (before clamp to MinPoints)
	pathLengthScale  = 20  // Multiplier for path-length-based point count
	// MinPoints is the minimum number of trajectory points.
	MinPoints = 5
	// MaxPoints is the maximum number of trajectory points.
	MaxPoints = 80
)

func (t *HumanizeMouseTrajectory) generateCurve(opts *Options) {
	left := math.Min(t.fromX, t.toX) - boundsPadding
	right := math.Max(t.fromX, t.toX) + boundsPadding
	down := math.Min(t.fromY, t.toY) - boundsPadding
	up := math.Max(t.fromY, t.toY) + boundsPadding

	knots := t.generateInternalKnots(left, right, down, up, knotsCount)
	curvePoints := t.generatePoints(knots)
	curvePoints = t.distortPoints(curvePoints, distortionMean, distortionStDev, distortionFreq)
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
			x += delta
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

func (t *HumanizeMouseTrajectory) tweenPoints(points [][2]float64, opts *Options) [][2]float64 {
	var totalLength float64
	for i := 1; i < len(points); i++ {
		dx := points[i][0] - points[i-1][0]
		dy := points[i][1] - points[i-1][1]
		totalLength += math.Sqrt(dx*dx + dy*dy)
	}

	targetPoints := int(math.Min(
		float64(defaultMaxPoints),
		math.Max(float64(defaultMinPoints+2), math.Pow(totalLength, 0.25)*pathLengthScale)))

	if opts != nil && opts.MaxPoints > 0 {
		maxPts := opts.MaxPoints
		if maxPts < MinPoints {
			maxPts = MinPoints
		}
		if maxPts > MaxPoints {
			maxPts = MaxPoints
		}
		targetPoints = maxPts
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
