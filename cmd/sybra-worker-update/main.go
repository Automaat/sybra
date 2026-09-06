// sybra-worker-update is a root deployment helper, never a board or scheduler.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Automaat/sybra/internal/workerupdate"
)

func main() {
	path := flag.String("config", "/etc/sybra/worker-update.yaml", "root-owned updater configuration")
	check := flag.Bool("check-config", false, "validate configuration without deployment")
	retry := flag.Bool("retry-quarantined", false, "explicitly retry the quarantined candidate after repair")
	flag.Parse()
	cfg, err := workerupdate.LoadConfig(*path)
	if err == nil && !*check {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		var status string
		status, err = workerupdate.RunOnce(ctx, cfg, *retry)
		if status != "" {
			fmt.Println(status)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
