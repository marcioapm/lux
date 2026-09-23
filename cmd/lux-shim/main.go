// Command lux-shim runs as PID 1 inside every lux container. The runner
// mounts it read-only at /.lux/bin/lux-shim and makes it the entrypoint;
// images need nothing lux-specific. See internal/shim.
package main

import (
	"os"

	"github.com/marcioapm/lux/internal/shim"
)

func main() {
	os.Exit(shim.Main())
}
