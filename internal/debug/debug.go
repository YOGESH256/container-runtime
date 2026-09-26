// Package debug prints timestamped progress lines when MYCRT_DEBUG is set,
// to see where the time goes while a container starts and stops.
package debug

import (
	"fmt"
	"os"
	"time"
)

// Enabled is set from MYCRT_DEBUG. The container's init process does not
// inherit the environment, so it gets the setting through its config.
var Enabled = os.Getenv("MYCRT_DEBUG") != ""

// Logf prints a line prefixed with wall-clock time and the process role.
func Logf(role, format string, args ...any) {
	if !Enabled {
		return
	}
	fmt.Fprintf(os.Stderr, "%s %-6s "+format+"\n",
		append([]any{time.Now().Format("15:04:05.000"), role}, args...)...)
}
