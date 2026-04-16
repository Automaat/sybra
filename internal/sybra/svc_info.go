package sybra

import "github.com/Automaat/sybra/internal/version"

// VersionInfo holds version strings for the server and client.
type VersionInfo struct {
	Server string `json:"server"`
}

// InfoService exposes build metadata to the frontend.
type InfoService struct{}

// GetVersion returns version information for the running server binary.
func (s *InfoService) GetVersion() VersionInfo {
	return VersionInfo{Server: version.Version}
}
