package guardpower

import (
	"os"
	"runtime"
	"strings"
)

// currentBootID identifies the running boot. A host power-off that was
// journaled on an earlier boot must not suppress protection on this one.
func currentBootID() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(raw))
	if len(id) > 64 {
		return ""
	}
	return id
}
