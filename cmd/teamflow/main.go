// Command teamflow is the Teamflow client: CLI, MCP server and Claude Code hooks.
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

const usage = "usage: teamflow --version\n"

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
	if len(args) > 0 && args[0] == "debug" { // hidden: not in the usage text
		return cli.Debug(args[1:], stdout, stderr)
	}
	if len(args) > 0 {
		fmt.Fprintf(stderr, "teamflow: unknown command %q\n", args[0])
	}
	fmt.Fprint(stderr, usage)
	return 2
}
