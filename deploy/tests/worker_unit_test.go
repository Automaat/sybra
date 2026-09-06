package deploy_test

import (
	"os"
	"strings"
	"testing"
)

func TestWorkerUnitsHaveNoBoardDependency(t *testing.T) {
	for _, path := range []string{"../systemd/sybra-agentd.service", "../systemd/sybra-agentd-standalone.conf"} {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			section := ""
			for line := range strings.SplitSeq(string(data), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "[") {
					section = line
					continue
				}
				if section != "[Unit]" || strings.HasPrefix(line, "#") {
					continue
				}
				key, value, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				switch strings.TrimSpace(key) {
				case "After", "Before", "PartOf", "Requires", "Wants", "BindsTo", "Requisite", "Conflicts", "Upholds":
					// Unlike service command lists, dependencies cannot be cleared
					// by empty assignments in a drop-in. Reject that false fix too.
					if strings.TrimSpace(value) == "" {
						t.Errorf("ineffective empty dependency assignment: %s", line)
					}
					for dependency := range strings.FieldsSeq(value) {
						if dependency == "sybra.service" {
							t.Errorf("worker depends on board: %s", line)
						}
					}
				}
			}
		})
	}
}
