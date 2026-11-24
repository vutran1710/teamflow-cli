// Command gen regenerates package protocol's types, constants and agent texts from the exported protocol.
//
//	go run ./tools/gen                      # from the repo root
//	go run ./tools/gen -protocol P -out D   # elsewhere
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/vutran1710/teamflow-cli/internal/protocolgen"
)

func main() {
	protocolDir := flag.String("protocol", "protocol", "the exported protocol folder")
	out := flag.String("out", filepath.Join("internal", "protocol"), "where the generated files go")
	flag.Parse()
	if err := run(*protocolDir, *out); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(protocolDir, out string) error {
	return protocolgen.WriteAll(protocolDir, out)
}
