//go:build unix

package skills

import (
	"os"
	"syscall"
)

const skillReadFlags = os.O_RDONLY | syscall.O_NONBLOCK
