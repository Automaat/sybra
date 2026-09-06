package deploy_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkerReleaseDependsOnEveryCIGate(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	type job struct {
		If          string            `yaml:"if"`
		Needs       []string          `yaml:"needs"`
		Permissions map[string]string `yaml:"permissions"`
	}
	var workflow struct {
		Jobs map[string]job `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	release, ok := workflow.Jobs["worker-release"]
	if !ok {
		t.Fatal("no worker publisher")
	}
	if release.If != "github.event_name == 'push' && github.ref == 'refs/heads/main'" {
		t.Fatal("untrusted event can publish", release.If)
	}
	covered := map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		if covered[id] {
			return
		}
		covered[id] = true
		for _, dependency := range workflow.Jobs[id].Needs {
			visit(dependency)
		}
	}
	visit("worker-release")
	for id := range workflow.Jobs {
		if !covered[id] {
			t.Errorf("release can precede CI gate %s", id)
		}
	}
	if release.Permissions["id-token"] != "write" || release.Permissions["attestations"] != "write" {
		t.Fatal("missing provenance permissions")
	}
}

func TestWorkerUpdaterOnlyOwnsDeployment(t *testing.T) {
	data, err := os.ReadFile("../systemd/sybra-worker-update.service")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "sybra.service") {
			t.Error("updater starts or depends on board", line)
		}
	}
	for _, required := range []string{"User=root", "StateDirectoryMode=0700", "NoNewPrivileges=true", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"} {
		if !strings.Contains(string(data), required) {
			t.Error("missing deployment constraint", required)
		}
	}
	unit, err := os.ReadFile("../systemd/sybra-worker-update.timer")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(strings.Split(string(unit), "\n"), "Unit=sybra-worker-update.service") {
		t.Fatal("timer targets wrong service")
	}
}
