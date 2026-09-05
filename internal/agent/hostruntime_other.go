//go:build !darwin

package agent

// hostRuntimeReadRoots has nothing to add away from darwin: Linux hosts link
// against /usr and /lib, already system read roots, and Nix-backed tools are
// covered by /nix/store.
func hostRuntimeReadRoots() []string { return nil }
