package xdg

import (
	"fmt"
	"os"
	"testing"
)

func TestRuntimeDir(t *testing.T) {
	for _, tc := range []struct{ name, env, want string }{
		{"set", "/run/user/elsewhere", "/run/user/elsewhere"},
		{"unset", "", fmt.Sprintf("/run/user/%d", os.Getuid())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", tc.env)
			if tc.env == "" {
				os.Unsetenv("XDG_RUNTIME_DIR")
			}
			if got := RuntimeDir(); got != tc.want {
				t.Errorf("RuntimeDir() = %q, want %q", got, tc.want)
			}
		})
	}
}
