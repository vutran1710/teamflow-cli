// Command teamflow is the Teamflow client: the CLI, the MCP server and the agents' hooks.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/vutran1710/teamflow-cli/internal/api"
	"github.com/vutran1710/teamflow-cli/internal/cli"
)

// version is set at build time: go build -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	api.Version = version // the Teamflow-Client header names this build
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 {
		switch args[0] {
		case "--version", "-version", "version":
			fmt.Fprintf(stdout, "teamflow %s\n", version)
			return 0
		}
	}
	return cli.Main(args, os.Stdin, stdout, stderr)
}
