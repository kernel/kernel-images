package x11

import "testing"

func TestSupportingWindowID(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
		ok   bool
	}{
		{
			name: "property set by a running WM",
			out:  "_NET_SUPPORTING_WM_CHECK(WINDOW): window id # 0x20000e\n",
			want: "0x20000e",
			ok:   true,
		},
		{
			// xprop exits 0 in this case, so the exit code alone is not enough
			// to tell "no WM" from "WM present".
			name: "no window manager has claimed the display",
			out:  "_NET_SUPPORTING_WM_CHECK:  no such atom on any window.\n",
			ok:   false,
		},
		{
			name: "zero window ID",
			out:  "_NET_SUPPORTING_WM_CHECK(WINDOW): window id # 0x0\n",
			ok:   false,
		},
		{
			name: "empty output",
			out:  "",
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := supportingWindowID(tt.out)
			if got != tt.want || ok != tt.ok {
				t.Errorf("got (%q, %v), want (%q, %v) for %q", got, ok, tt.want, tt.ok, tt.out)
			}
		})
	}
}

func TestRootSize(t *testing.T) {
	tests := []struct {
		name   string
		out    string
		width  int
		height int
		ok     bool
	}{
		{
			name: "root dump",
			out: `xwininfo: Window id: 0x572 (the root window) (has no name)

  Absolute upper-left X:  0
  Absolute upper-left Y:  0
  Width: 1920
  Height: 1080
  Depth: 24
`,
			width:  1920,
			height: 1080,
			ok:     true,
		},
		{
			name:   "xwininfo failure output",
			out:    "xwininfo: unable to open display :1\n",
			ok:     false,
		},
		{
			// Both dimensions must be present; a partial dump is not a size.
			name: "width without height",
			out: `  Width: 1920
`,
			ok: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, h, ok := rootSize(tt.out)
			if ok != tt.ok || (ok && (w != tt.width || h != tt.height)) {
				t.Errorf("got (%d, %d, %v), want (%d, %d, %v) for %q", w, h, ok, tt.width, tt.height, tt.ok, tt.out)
			}
		})
	}
}

func TestActiveWindowID(t *testing.T) {
	tests := []struct {
		name string
		out  string
		id   string
		ok   bool
	}{
		{
			name: "mapped window",
			out:  "_NET_ACTIVE_WINDOW(WINDOW): window id # 0x800003\n",
			id:   "0x800003",
			ok:   true,
		},
		{
			// openbox lists the focus window alongside the active one.
			name: "window list",
			out:  "_NET_ACTIVE_WINDOW(WINDOW): window id # 0x800003, 0x0\n",
			id:   "0x800003",
			ok:   true,
		},
		{
			name: "no client window mapped yet",
			out:  "_NET_ACTIVE_WINDOW(WINDOW): window id # 0x0\n",
			ok:   false,
		},
		{
			name: "property unset",
			out:  "_NET_ACTIVE_WINDOW:  not found.\n",
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := activeWindowID(tt.out)
			if ok != tt.ok || (ok && id != tt.id) {
				t.Errorf("got (%q, %v), want (%q, %v) for %q", id, ok, tt.id, tt.ok, tt.out)
			}
		})
	}
}

func TestWindowCovered(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		covered bool
		ok      bool
	}{
		{
			name:    "maximized",
			line:    "_NET_WM_STATE(ATOM) = _NET_WM_STATE_MAXIMIZED_VERT, _NET_WM_STATE_MAXIMIZED_HORZ",
			covered: true,
			ok:      true,
		},
		{
			name: "demoted",
			line: "_NET_WM_STATE(ATOM) = ",
			ok:   true,
		},
		{
			// Half a maximize leaves desktop beside the window, which is the
			// shape this guard exists to correct.
			name: "maximized vertically only",
			line: "_NET_WM_STATE(ATOM) = _NET_WM_STATE_MAXIMIZED_VERT",
			ok:   true,
		},
		{
			name:    "kiosk",
			line:    "_NET_WM_STATE(ATOM) = _NET_WM_STATE_FULLSCREEN",
			covered: true,
			ok:      true,
		},
		{
			name: "unrelated output",
			line: "xprop:  unable to open display",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			covered, ok := windowCovered(tt.line)
			if ok != tt.ok || covered != tt.covered {
				t.Errorf("got (%v, %v), want (%v, %v) for %q", covered, ok, tt.covered, tt.ok, tt.line)
			}
		})
	}
}
