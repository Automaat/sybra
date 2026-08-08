package config

import "path/filepath"

// AttemptLeasesDir holds the durable dispatch admission ledger.
func AttemptLeasesDir() string {
	return filepath.Join(HomeDir(), "attempt-leases")
}
