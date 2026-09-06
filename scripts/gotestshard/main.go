// gotestshard partitions the application E2E suite without maintaining a test
// allowlist. Discovery uses the same build flags as execution, including the
// race detector. Every top-level test, example, and fuzz seed belongs to exactly
// one shard; subtests stay with their parent and retain their fixture lifetime.
package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

var testName = regexp.MustCompile(`^(Test|Example|Fuzz)[\pL\pN_]*$`)

func selectTests(output string, index, total int) ([]string, error) {
	if total < 1 || index < 0 || index >= total {
		return nil, fmt.Errorf("invalid shard %d of %d", index, total)
	}
	seen := make(map[string]bool)
	var selected []string
	for line := range strings.SplitSeq(output, "\n") {
		name := strings.TrimSpace(line)
		if !testName.MatchString(name) {
			continue // go test also prints the package summary and TestMain logs.
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate discovered test %q", name)
		}
		seen[name] = true
		digest := sha256.Sum256([]byte(name))
		// Accumulate modulo total rather than narrowing total to a byte.
		bucket := 0
		for _, b := range digest {
			bucket = (bucket*256 + int(b)) % total
		}
		if bucket == index {
			selected = append(selected, name)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("shard %d of %d selected no tests (discovered %d)", index, total, len(seen))
	}
	slices.Sort(selected)
	return selected, nil
}

func runPattern(names []string) string {
	escaped := make([]string, len(names))
	for i, name := range names {
		escaped[i] = regexp.QuoteMeta(name)
	}
	return "^(" + strings.Join(escaped, "|") + ")$"
}

func run(index, total int) error {
	if total < 1 || total > 256 || index < 0 || index >= total {
		return fmt.Errorf("shard must be in [0, total), with total in [1, 256]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	args := []string{"test", "-race", "-tags", "e2e", "-p", "1", "-timeout", "20m"}
	listArgs := append(slices.Clone(args), "-list", ".", "./internal/sybra")
	list := exec.CommandContext(ctx, "go", listArgs...)
	list.Stderr = os.Stderr
	output, err := list.Output()
	if err != nil {
		return fmt.Errorf("discover E2E tests: %w", err)
	}
	names, err := selectTests(string(output), index, total)
	if err != nil {
		return err
	}
	fmt.Printf("E2E shard %d/%d: %d top-level tests (subtests included)\n%s\n", index, total, len(names), strings.Join(names, "\n"))
	args = append(args, "-count=1", "-run", runPattern(names), "./internal/sybra")
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("execute E2E shard: %w", err)
	}
	return nil
}

func main() {
	index := flag.Int("index", -1, "zero-based shard index")
	total := flag.Int("total", 4, "number of shards")
	flag.Parse()
	if err := run(*index, *total); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
