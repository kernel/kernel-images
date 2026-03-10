package mousetrajectory

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHumanizeMouseTrajectory_DeterministicWithSeed(t *testing.T) {
	traj := NewHumanizeMouseTrajectoryWithSeed(0, 0, 100, 100, 42)
	points1 := traj.GetPointsInt()

	traj2 := NewHumanizeMouseTrajectoryWithSeed(0, 0, 100, 100, 42)
	points2 := traj2.GetPointsInt()

	require.Len(t, points1, len(points2))
	for i := range points1 {
		assert.Equal(t, points1[i], points2[i], "point %d should match", i)
	}
}

func TestHumanizeMouseTrajectory_StartAndEnd(t *testing.T) {
	traj := NewHumanizeMouseTrajectoryWithSeed(50, 50, 200, 150, 123)
	points := traj.GetPointsInt()

	require.GreaterOrEqual(t, len(points), 2, "should have at least 2 points")
	assert.Equal(t, 50, points[0][0])
	assert.Equal(t, 50, points[0][1])
	assert.Equal(t, 200, points[len(points)-1][0])
	assert.Equal(t, 150, points[len(points)-1][1])
}

func TestHumanizeMouseTrajectory_WithStepsOverride(t *testing.T) {
	opts := &Options{MaxPoints: 15}
	traj := NewHumanizeMouseTrajectoryWithOptions(0, 0, 100, 100, opts)
	points := traj.GetPointsInt()

	assert.Len(t, points, 15, "should have exactly 15 points when MaxPoints=15")
}

func TestHumanizeMouseTrajectory_ZeroLengthPath(t *testing.T) {
	// Same start and end: should produce at least 2 points, both at (0,0)
	traj := NewHumanizeMouseTrajectoryWithSeed(0, 0, 0, 0, 42)
	points := traj.GetPointsInt()

	require.GreaterOrEqual(t, len(points), 2, "zero-length path should have at least 2 points")
	assert.Equal(t, 0, points[0][0])
	assert.Equal(t, 0, points[0][1])
	assert.Equal(t, 0, points[len(points)-1][0])
	assert.Equal(t, 0, points[len(points)-1][1])
}

func TestHumanizeMouseTrajectory_MaxPointsClampedToMin(t *testing.T) {
	// MaxPoints below MinPoints should be clamped up to MinPoints
	opts := &Options{MaxPoints: 2}
	traj := NewHumanizeMouseTrajectoryWithOptions(0, 0, 100, 100, opts)
	points := traj.GetPointsInt()

	assert.Len(t, points, MinPoints, "MaxPoints below MinPoints should clamp to MinPoints")
}

func TestHumanizeMouseTrajectory_MaxPointsClampedToMax(t *testing.T) {
	// MaxPoints above MaxPoints should be clamped down to MaxPoints
	opts := &Options{MaxPoints: 200}
	traj := NewHumanizeMouseTrajectoryWithOptions(0, 0, 100, 100, opts)
	points := traj.GetPointsInt()

	assert.Len(t, points, MaxPoints, "MaxPoints above MaxPoints should clamp to MaxPoints")
}

func TestHumanizeMouseTrajectory_NearEdgeCanProduceNegativeCoords(t *testing.T) {
	// When the start is near (0,0), boundsPadding=80 places Bezier control
	// knots into negative territory so intermediate curve points can have
	// negative coordinates. Consumers that use relative moves must clamp
	// the trajectory to screen bounds to avoid X11 edge-clamping errors.
	foundNegative := false
	for seed := int64(0); seed < 100; seed++ {
		traj := NewHumanizeMouseTrajectoryWithSeed(10, 10, 200, 200, seed)
		for _, p := range traj.GetPointsInt() {
			if p[0] < 0 || p[1] < 0 {
				foundNegative = true
				break
			}
		}
		if foundNegative {
			break
		}
	}
	assert.True(t, foundNegative, "expected at least one seed to produce negative coordinates near screen edge")
}

func TestHumanizeMouseTrajectory_CurvedPath(t *testing.T) {
	traj := NewHumanizeMouseTrajectoryWithSeed(0, 0, 100, 0, 999)
	points := traj.GetPointsInt()

	// For a horizontal move, the Bezier adds control points, so the path may curve
	// Middle points should not all lie exactly on the line y=0 (curved path)
	require.GreaterOrEqual(t, len(points), 3)
	allOnLine := true
	for i := 1; i < len(points)-1; i++ {
		if points[i][1] != 0 {
			allOnLine = false
			break
		}
	}
	assert.False(t, allOnLine, "path should be curved, not a straight line")
}

func TestGenerateMultiSegmentTrajectory_ThreeWaypoints(t *testing.T) {
	waypoints := [][2]int{{100, 100}, {500, 300}, {900, 100}}
	result := GenerateMultiSegmentTrajectory(waypoints, 1920, 1080, nil)

	require.GreaterOrEqual(t, len(result.Points), MinPoints*2, "multi-segment should produce enough points")
	assert.Equal(t, 100, result.Points[0][0], "first point X should match first waypoint")
	assert.Equal(t, 100, result.Points[0][1], "first point Y should match first waypoint")
	assert.Equal(t, 900, result.Points[len(result.Points)-1][0], "last point X should match last waypoint")
	assert.Equal(t, 100, result.Points[len(result.Points)-1][1], "last point Y should match last waypoint")
}

func TestGenerateMultiSegmentTrajectory_TwoWaypoints(t *testing.T) {
	waypoints := [][2]int{{0, 0}, {200, 200}}
	result := GenerateMultiSegmentTrajectory(waypoints, 1920, 1080, nil)

	require.GreaterOrEqual(t, len(result.Points), MinPoints)
	assert.Equal(t, 0, result.Points[0][0])
	assert.Equal(t, 0, result.Points[0][1])
	assert.Equal(t, 200, result.Points[len(result.Points)-1][0])
	assert.Equal(t, 200, result.Points[len(result.Points)-1][1])
}

func TestGenerateMultiSegmentTrajectory_WithDurationMs(t *testing.T) {
	waypoints := [][2]int{{100, 100}, {500, 300}, {900, 100}}
	dur := 2000
	result := GenerateMultiSegmentTrajectory(waypoints, 1920, 1080, &dur)

	require.GreaterOrEqual(t, len(result.Points), MinPoints)
	assert.Greater(t, result.StepDelayMs, 0)

	totalMs := result.StepDelayMs * (len(result.Points) - 1)
	assert.InDelta(t, 2000, totalMs, 500, "total duration should be approximately 2000ms")
}

func TestGenerateMultiSegmentTrajectory_PointsClampedToScreen(t *testing.T) {
	waypoints := [][2]int{{5, 5}, {50, 50}, {95, 95}}
	result := GenerateMultiSegmentTrajectory(waypoints, 100, 100, nil)

	for i, p := range result.Points {
		assert.GreaterOrEqual(t, p[0], 0, "point %d X should be >= 0", i)
		assert.GreaterOrEqual(t, p[1], 0, "point %d Y should be >= 0", i)
		assert.LessOrEqual(t, p[0], 99, "point %d X should be <= screenW-1", i)
		assert.LessOrEqual(t, p[1], 99, "point %d Y should be <= screenH-1", i)
	}
}

func TestGenerateMultiSegmentTrajectory_SinglePoint(t *testing.T) {
	waypoints := [][2]int{{100, 100}}
	result := GenerateMultiSegmentTrajectory(waypoints, 1920, 1080, nil)

	assert.Len(t, result.Points, 1)
	assert.Equal(t, 100, result.Points[0][0])
	assert.Equal(t, 100, result.Points[0][1])
}

func TestGenerateMultiSegmentTrajectory_ContinuousPath(t *testing.T) {
	waypoints := [][2]int{{100, 100}, {500, 500}, {900, 100}, {1300, 500}}
	result := GenerateMultiSegmentTrajectory(waypoints, 1920, 1080, nil)

	for i := 1; i < len(result.Points); i++ {
		dx := result.Points[i][0] - result.Points[i-1][0]
		dy := result.Points[i][1] - result.Points[i-1][1]
		dist := math.Sqrt(float64(dx*dx + dy*dy))
		assert.Less(t, dist, 200.0, "consecutive points %d-%d should not jump too far (dist=%.1f)", i-1, i, dist)
	}
}
