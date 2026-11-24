package procs

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// statFields are the fields of /proc/<pid>/stat after "pid (comm)": state is [0], ppid [1], starttime [19]. comm
// may hold spaces and parentheses, so they are read after the last ')'.
func statFields(pid int) []string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return nil
	}
	return strings.Fields(s[i+1:])
}

// Parent is pid's parent process id, or 0 when it can't be read.
func Parent(pid int) int {
	f := statFields(pid)
	if len(f) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return 0
	}
	return ppid
}

// clockTicks is USER_HZ, the unit of /proc/<pid>/stat's starttime: 100 on every Linux the client supports.
const clockTicks = 100

// StartTime is when pid started; false when it can't be read.
func StartTime(pid int) (time.Time, bool) {
	f := statFields(pid)
	if len(f) < 20 {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			return time.Unix(boot, 0).Add(time.Duration(ticks) * time.Second / clockTicks), true
		}
	}
	return time.Time{}, false
}

// Name is pid's command name (/proc/<pid>/comm), or "" when it can't be read.
func Name(pid int) string {
	if pid <= 0 {
		return ""
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
