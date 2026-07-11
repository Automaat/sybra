package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	servicesGoRel = "internal/sybra/services.go"
	apiTSRel      = "frontend/src/lib/api.ts"
	apiHTTPTSRel  = "frontend/src/lib/api-http.ts"
	bindingDirRel = "frontend/bindings/github.com/Automaat/sybra/internal/sybra"
	bindingBase   = "internal/sybra"
	importPrefix  = "../../bindings/github.com/Automaat/sybra/"
)

func main() {
	root := flag.String("root", ".", "repo root (defaults to the current directory)")
	checkOnly := flag.Bool("check", false, "exit non-zero when shims are missing instead of writing them")
	flag.Parse()

	if err := run(*root, *checkOnly); err != nil {
		fmt.Fprintln(os.Stderr, "gen-api-shim:", err)
		os.Exit(1)
	}
}

func run(root string, checkOnly bool) error {
	services, err := parseServices(filepath.Join(root, servicesGoRel))
	if err != nil {
		return err
	}

	apiTSPath := filepath.Join(root, apiTSRel)
	apiHTTPPath := filepath.Join(root, apiHTTPTSRel)
	apiTS, err := os.ReadFile(apiTSPath)
	if err != nil {
		return err
	}
	apiHTTP, err := os.ReadFile(apiHTTPPath)
	if err != nil {
		return err
	}

	nextTS, addedTS, err := fillAPITS(string(apiTS), services)
	if err != nil {
		return err
	}
	nextHTTP, addedHTTP, err := fillAPIHTTP(string(apiHTTP), services, filepath.Join(root, bindingDirRel))
	if err != nil {
		return err
	}

	if len(addedTS) == 0 && len(addedHTTP) == 0 {
		return nil
	}
	if checkOnly {
		return fmt.Errorf("missing shims (run `go run ./cmd/gen-api-shim`): api.ts=%v api-http.ts=%v", addedTS, addedHTTP)
	}

	if len(addedTS) > 0 {
		if err := os.WriteFile(apiTSPath, []byte(nextTS), 0o644); err != nil {
			return err
		}
		fmt.Printf("api.ts: added %s\n", strings.Join(addedTS, ", "))
	}
	if len(addedHTTP) > 0 {
		if err := os.WriteFile(apiHTTPPath, []byte(nextHTTP), 0o644); err != nil {
			return err
		}
		fmt.Printf("api-http.ts: added %s\n", strings.Join(addedHTTP, ", "))
	}
	return nil
}

type service struct {
	name    string
	methods []string
}

var (
	serviceHeaderRe = regexp.MustCompile(`^\s*"([A-Za-z][A-Za-z0-9]*)":\s*httpapi\.NewService\(`)
	methodLineRe    = regexp.MustCompile(`^\s*"([A-Z][A-Za-z0-9_]*)",?\s*$`)
)

func parseServices(path string) ([]service, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var services []service
	var cur *service
	for line := range strings.SplitSeq(string(data), "\n") {
		if m := serviceHeaderRe.FindStringSubmatch(line); m != nil {
			services = append(services, service{name: m[1]})
			cur = &services[len(services)-1]
			continue
		}
		if cur == nil {
			continue
		}
		if trimmed := strings.TrimSpace(line); trimmed == ")," || trimmed == ")" {
			cur = nil
			continue
		}
		if m := methodLineRe.FindStringSubmatch(line); m != nil {
			cur.methods = append(cur.methods, m[1])
		}
	}
	if len(services) == 0 {
		return nil, fmt.Errorf("no services parsed from %s — check the extraction regex", path)
	}
	return services, nil
}

func bindingModuleBase(serviceName string) string {
	return strings.ToLower(serviceName)
}

func insertAfter(lines []string, inserts map[int][]string) []string {
	if len(inserts) == 0 {
		return lines
	}
	out := make([]string, 0, len(lines)+len(inserts))
	for i, line := range lines {
		out = append(out, line)
		out = append(out, inserts[i]...)
	}
	return out
}
