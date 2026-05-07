//go:build !darwin

package main

import "fmt"

// main is a no-op stub for non-darwin platforms. The v3 spike pins Wails v3
// alpha which requires gtk3/webkit2gtk-4.1 system headers on Linux that the
// CI runners do not have. Keeps `go build ./...` and govulncheck green on
// Linux while the real entry point in main.go remains macOS-gated.
func main() {
	fmt.Println("sybra-v3-poc is darwin-only; see docs/migrations/wails-v3.md")
}
