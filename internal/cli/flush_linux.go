package cli

import "golang.org/x/sys/unix"

// flushTerminalInput throws away what was typed on the terminal fd and not read yet (TCIFLUSH: the input queue).
func flushTerminalInput(fd int) error { return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH) }
