//go:build !unix

package skills

import "os"

const skillReadFlags = os.O_RDONLY
