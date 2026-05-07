// Package v3svc holds Wails v3 service ports for the Phase 1 migration spike.
//
// Lives in parallel to internal/sybra/svc_*.go so the v2 build on main is not
// disturbed. Phase 2 absorbs the rest of the services and retires this package.
package v3svc

import "github.com/Automaat/sybra/internal/version"

// VersionInfo mirrors internal/sybra.VersionInfo. Re-declared here so the v3
// service has zero coupling to the v2 svc_* types during the spike.
type VersionInfo struct {
	Server string `json:"server"`
}

// InfoService is the v3 port of internal/sybra.InfoService.
type InfoService struct{}

// NewInfoService is the v3 service-pattern constructor.
func NewInfoService() *InfoService {
	return &InfoService{}
}

// GetVersion returns the running binary's version string.
func (s *InfoService) GetVersion() VersionInfo {
	return VersionInfo{Server: version.Version}
}
