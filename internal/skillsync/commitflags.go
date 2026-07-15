package skillsync

import "strings"

var commitFlagDowngrader = strings.NewReplacer(
	"-s -S", "-s",
	"-sS", "-s",
	"-S -s", "-s",
)

func (s *Syncer) transform(data []byte) []byte {
	if !s.downgrade {
		return data
	}
	return []byte(commitFlagDowngrader.Replace(string(data)))
}
