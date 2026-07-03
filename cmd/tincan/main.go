// Command tincan passes messages between AI coding agents over a local
// filesystem spool. See PROTOCOL.md in the repository root.
package main

import (
	"os"

	"github.com/c0ze/tincan/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
