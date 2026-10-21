package livefocus

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A maximized 390x844 window with 87px of browser UI above the page.
var phoneWindow = windowBounds{Left: 0, Top: 0, Width: 390, Height: 844}

func phoneViewport(dpr float64) *viewport {
	return &viewport{W: 390 / dpr, H: 757 / dpr, DPR: dpr, ScreenW: 390, ScreenH: 844}
}

func field(kind string, rect *Rect) *Field {
	return &Field{Type: kind, Rect: rect}
}

func TestMergeMapsThePageToTheScreen(t *testing.T) {
	top := frameState{tab: "tab", seq: 1, report: &frameReport{
		Top: true, Visible: true, Focused: true, Viewport: phoneViewport(1),
		Focus:  field("email", &Rect{X: 10, Y: 20, W: 100, H: 30}),
		Fields: []Field{*field("email", &Rect{X: 10, Y: 20, W: 100, H: 30})},
	}}
	state := merge([]frameState{top}, map[string]windowBounds{"tab": phoneWindow})
	require.Equal(t, &Rect{X: 0, Y: 87, W: 390, H: 757}, state.Viewport)
	require.True(t, state.PageFocused)
	require.Equal(t, &Size{W: 390, H: 844}, state.Screen)
	require.Equal(t, &Rect{X: 10, Y: 107, W: 100, H: 30}, state.Focus.Rect)
	require.Equal(t, []Field{*field("email", &Rect{X: 10, Y: 107, W: 100, H: 30})}, state.Fields)
}

func TestMergeScalesByPageZoom(t *testing.T) {
	top := frameState{tab: "tab", seq: 1, report: &frameReport{
		Top: true, Visible: true, Focused: true, Viewport: phoneViewport(1.25),
		Fields: []Field{*field("text", &Rect{X: 8, Y: 16, W: 80, H: 24})},
	}}
	state := merge([]frameState{top}, map[string]windowBounds{"tab": phoneWindow})
	require.Equal(t, &Rect{X: 0, Y: 87, W: 390, H: 757}, state.Viewport)
	require.Equal(t, &Rect{X: 10, Y: 107, W: 100, H: 30}, state.Fields[0].Rect)
}

func TestMergeTakesFocusFromTheFocusedFrame(t *testing.T) {
	top := frameState{tab: "tab", seq: 1, report: &frameReport{Top: true, Visible: true, Focused: true, Viewport: phoneViewport(1)}}
	child := frameState{tab: "tab", seq: 2, report: &frameReport{Focused: true, Focus: field("text", &Rect{X: 5, Y: 5, W: 10, H: 10})}}
	// a document that is not focused does not take keyboard input, whatever its activeElement
	stale := frameState{tab: "tab", seq: 3, report: &frameReport{Focus: field("password", nil)}}
	state := merge([]frameState{top, child, stale}, map[string]windowBounds{"tab": phoneWindow})
	require.Equal(t, "text", state.Focus.Type)
	require.Equal(t, &Rect{X: 5, Y: 92, W: 10, H: 10}, state.Focus.Rect)
}

func TestMergeLeavesACrossOriginFramesFieldWithoutARect(t *testing.T) {
	top := frameState{tab: "tab", seq: 1, report: &frameReport{Top: true, Visible: true, Focused: true, Viewport: phoneViewport(1)}}
	oopif := frameState{seq: 2, report: &frameReport{Focused: true, Focus: field("tel", nil)}}
	state := merge([]frameState{top, oopif}, map[string]windowBounds{"tab": phoneWindow})
	require.Equal(t, "tel", state.Focus.Type)
	require.Nil(t, state.Focus.Rect)
}

func TestMergeShowsTheVisibleTabInTheFocusedWindow(t *testing.T) {
	hidden := frameState{tab: "hidden", seq: 3, report: &frameReport{
		Top: true, Visible: false, Viewport: phoneViewport(1), Fields: []Field{*field("text", &Rect{W: 1, H: 1})},
	}}
	otherWindow := frameState{tab: "other", seq: 2, report: &frameReport{Top: true, Visible: true, Viewport: phoneViewport(1)}}
	shown := frameState{tab: "shown", seq: 1, report: &frameReport{
		Top: true, Visible: true, Focused: true, Viewport: phoneViewport(1), Fields: []Field{*field("search", &Rect{W: 1, H: 1})},
	}}
	bounds := map[string]windowBounds{"hidden": phoneWindow, "other": phoneWindow, "shown": phoneWindow}
	state := merge([]frameState{hidden, otherWindow, shown}, bounds)
	require.Len(t, state.Fields, 1)
	require.Equal(t, "search", state.Fields[0].Type)

	// with the address bar focused no page has focus, and the newest visible tab is shown
	shown.report.Focused = false
	state = merge([]frameState{hidden, otherWindow, shown}, bounds)
	require.False(t, state.PageFocused)
	require.Empty(t, state.Fields)
}

func TestMergeWithoutAWindowReportsNoPageArea(t *testing.T) {
	top := frameState{tab: "tab", seq: 1, report: &frameReport{
		Top: true, Visible: true, Focused: true, Viewport: phoneViewport(1), Focus: field("text", &Rect{W: 1, H: 1}),
	}}
	state := merge([]frameState{top}, nil)
	require.Nil(t, state.Viewport)
	require.Empty(t, state.Fields)
	require.NotNil(t, state.Fields)
	require.Equal(t, "text", state.Focus.Type)
	require.Nil(t, state.Focus.Rect)
}
