//go:build !darwin

package main

import "fmt"

// main is a no-op stub for non-darwin platforms. Sybra desktop builds run on
// macOS only — Wails v3 alpha needs gtk3/webkit2gtk-4.1 system headers on
// Linux that CI runners don't have. Server-side and CLI builds use the
// dedicated cmd/sybra-server and cmd/sybra-cli binaries which are
// platform-agnostic.
func main() {
	fmt.Println("sybra desktop is darwin-only; use cmd/sybra-server or cmd/sybra-cli on other platforms")
}
