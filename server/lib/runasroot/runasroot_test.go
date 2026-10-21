package runasroot

import "testing"

func TestEnabled(t *testing.T) {
	for _, value := range []string{"", "false", "0", "invalid", "true", " TRUE "} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("RUN_AS_ROOT", value)
			want := value == "true" || value == " TRUE "
			if got := Enabled(); got != want {
				t.Fatalf("Enabled() = %t, want %t", got, want)
			}
		})
	}
}
