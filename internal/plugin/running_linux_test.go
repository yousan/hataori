package plugin

import (
	"os"
	"strconv"
	"strings"
)

// running reports whether process pid runs: it exists and is no zombie.
func running(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	return len(f) > 0 && f[0] != "Z"
}
