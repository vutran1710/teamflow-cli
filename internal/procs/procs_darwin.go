package procs

import (
	"time"

	"golang.org/x/sys/unix"
)

// Parent is pid's parent process id, or 0 when it can't be read.
func Parent(pid int) int {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k.Proc.P_pid != int32(pid) {
		return 0 // a pid that does not exist answers with an empty record
	}
	return int(k.Eproc.Ppid)
}

// StartTime is when pid started; false when it can't be read.
func StartTime(pid int) (time.Time, bool) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k.Proc.P_pid != int32(pid) {
		return time.Time{}, false
	}
	tv := k.Proc.P_starttime
	return time.Unix(int64(tv.Sec), int64(tv.Usec)*1000), true
}

// Name is pid's command name (its executable's base name, at most 16 bytes), or "" when it can't be read.
func Name(pid int) string {
	if pid <= 0 {
		return ""
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k.Proc.P_pid != int32(pid) {
		return ""
	}
	b := k.Proc.P_comm[:]
	n := 0
	for n < len(b) && b[n] != 0 {
		n++
	}
	out := make([]byte, n)
	for i := range n {
		out[i] = byte(b[i])
	}
	return string(out)
}
