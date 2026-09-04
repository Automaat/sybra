// Stand-in for cmd/sybra-cli used only by deploy/tests — see cmd/sybra-server
// in this fixture for why a trimmed-down module exists at all.
//
// It implements -check-config because the deploy preflight validates the live
// config against BOTH binaries. FAKE_CHECK_CONFIG_CLI is separate from
// FAKE_CHECK_CONFIG so a scenario can make the server accept a config the CLI
// rejects — the drift this preflight exists to catch, where the real CLI
// tolerated an unknown key by falling back to a direct task store while the
// server ran happily on it.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	checkConfig := flag.Bool("check-config", false, "")
	flag.Parse()

	if *checkConfig {
		if os.Getenv("FAKE_CHECK_CONFIG_CLI") == "fail" {
			fmt.Fprintln(os.Stderr, "config: invalid: fake CLI rejected key")
			os.Exit(1)
		}
		fmt.Println("config: ok")
		return
	}
}
