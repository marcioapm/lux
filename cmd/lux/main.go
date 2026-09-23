// Command lux is the lux CLI. See `lux --help` and docs/cli.md.
package main

import (
	"os"

	"github.com/marcioapm/lux/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
