package procs

import "golang.org/x/sys/unix"

// Parent is pid's parent process id, or 0 when it can't be read.
func Parent(pid int) int {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k.Proc.P_pid != int32(pid) {
		return 0 // a pid that does not exist answers with an empty record
	}
	return int(k.Eproc.Ppid)
}
