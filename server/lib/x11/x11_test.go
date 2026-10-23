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
