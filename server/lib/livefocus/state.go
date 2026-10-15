package livefocus

import "math"

// Rect is in screen pixels in a State, and in top-frame CSS pixels in a frame
// report.
type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type Size struct {
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Field describes an editable element: the attributes that choose a soft
// keyboard, and where it is. Rect is nil when its position is unknown, as for
// a field in a cross-origin frame.
type Field struct {
	// an input's type, "textarea", or "contenteditable"
	Type         string `json:"type"`
	InputMode    string `json:"inputMode,omitempty"`
	Autocomplete string `json:"autocomplete,omitempty"`
	EnterKeyHint string `json:"enterKeyHint,omitempty"`
	// the page's own attributes, when it sets them
	Autocapitalize *string `json:"autocapitalize,omitempty"`
	Autocorrect    *string `json:"autocorrect,omitempty"`
	Spellcheck     *string `json:"spellcheck,omitempty"`
	// a one-time code, by autocomplete="one-time-code" or by its name and size
	Code bool  `json:"code,omitempty"`
	Rect *Rect `json:"rect,omitempty"`
	// For the focused field: its frame's count of focus changes and presses,
	// which a blur request sends back so a blur from before a later tap is dropped.
	Seq int64 `json:"seq,omitempty"`
}

// State is what the live view receives on every change.
type State struct {
	// the focused editable element, or nil when keyboard input goes nowhere
	// that takes text
	Focus *Field `json:"focus"`
	// editable fields on screen that a tap would reach, so a client can raise
	// its keyboard inside the tap
	Fields []Field `json:"fields"`
	// the shown tab's page area on screen; nil when no tab is known
	Viewport *Rect `json:"viewport"`
	// The shown tab's page has keyboard focus. It does not while the address
	// bar or other browser UI has it, and gets it back when, for example, the
	// address bar navigates.
	PageFocused bool `json:"pageFocused"`
	// the screen the rects are measured in
	Screen *Size `json:"screen"`
}

func emptyState() State {
	return State{Fields: make([]Field, 0)}
}

type viewport struct {
	W       float64 `json:"w"`
	H       float64 `json:"h"`
	DPR     float64 `json:"dpr"`
	ScreenW float64 `json:"screenW"`
	ScreenH float64 `json:"screenH"`
	ScreenX float64 `json:"screenX"`
	ScreenY float64 `json:"screenY"`
}

// frameReport is what watcher.js reports for one frame.
type frameReport struct {
	Stopped  bool      `json:"stopped"`
	V        int64     `json:"v"`
	Top      bool      `json:"top"`
	Visible  bool      `json:"visible"`
	Focused  bool      `json:"focused"`
	Frames   int       `json:"frames"`
	Focus    *Field    `json:"focus"`
	Viewport *viewport `json:"viewport"`
	Fields   []Field   `json:"fields"`
}

// frameState is a watched frame's latest report. Tab is the page target the
// frame's rects are relative to, empty for an out-of-process iframe whose
// position in its page is unknown.
type frameState struct {
	report *frameReport
	seq    int64
	tab    string
}

// windowBounds is a browser window on screen, from Browser.getWindowForTarget.
type windowBounds struct {
	Left   float64 `json:"left"`
	Top    float64 `json:"top"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// merge combines frame reports into what the live view needs. The shown tab is
// the visible top frame, preferring the focused window, and the focused field
// is the newest report of an editable element in a focused document.
func merge(frames []frameState, bounds map[string]windowBounds) State {
	state := emptyState()
	tops := make(map[string]frameState)
	var shown, focused *frameState
	for i := range frames {
		f := &frames[i]
		r := f.report
		if r == nil {
			continue
		}
		if r.Top && r.Viewport != nil && f.tab != "" {
			tops[f.tab] = *f
			if r.Visible && (shown == nil || r.Focused && !shown.report.Focused || r.Focused == shown.report.Focused && f.seq > shown.seq) {
				shown = f
			}
		}
		if r.Focus != nil && r.Focused && (focused == nil || f.seq > focused.seq) {
			focused = f
		}
	}

	if focused != nil {
		field := *focused.report.Focus
		field.Rect = nil
		if top, ok := tops[focused.tab]; ok && focused.report.Focus.Rect != nil {
			if window, ok := bounds[focused.tab]; ok {
				field.Rect = toScreen(*focused.report.Focus.Rect, window, top.report.Viewport)
			}
		}
		state.Focus = &field
	}

	if shown == nil {
		return state
	}
	window, ok := bounds[shown.tab]
	if !ok {
		return state
	}
	vp := shown.report.Viewport
	state.Viewport = toScreen(Rect{W: vp.W, H: vp.H}, window, vp)
	state.PageFocused = shown.report.Focused
	state.Screen = &Size{W: vp.ScreenW, H: vp.ScreenH}
	for _, field := range shown.report.Fields {
		if field.Rect == nil {
			continue
		}
		field.Rect = toScreen(*field.Rect, window, vp)
		state.Fields = append(state.Fields, field)
	}
	return state
}

// toScreen maps a rect in the page's CSS pixels to the screen. The page sits
// at the bottom of its window, below the tab strip, toolbar and any infobars,
// and CSS pixels are devicePixelRatio screen pixels apart (the screen itself
// is not scaled, so this is the page zoom).
func toScreen(r Rect, window windowBounds, vp *viewport) *Rect {
	dpr := vp.DPR
	if dpr <= 0 {
		dpr = 1
	}
	left := window.Left
	top := window.Top + window.Height - vp.H*dpr
	return &Rect{
		X: math.Round(left + r.X*dpr),
		Y: math.Round(top + r.Y*dpr),
		W: math.Round(r.W * dpr),
		H: math.Round(r.H * dpr),
	}
}
