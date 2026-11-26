package cli

import "golang.org/x/sys/unix"

// flushTerminalInput throws away what was typed on the terminal fd and not read yet (FREAD: the input queue).
func flushTerminalInput(fd int) error { return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, 1) }
