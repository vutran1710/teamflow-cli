package procs

import (
	"os"
	"strconv"
	"strings"
)

// Parent is pid's parent process id, or 0 when it can't be read.
func Parent(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// pid (comm) state ppid …; comm may hold spaces and parentheses, so read after the last ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return 0
	}
	return ppid
}
