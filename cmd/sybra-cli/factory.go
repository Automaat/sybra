package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/audit"
)

func cmdFactory(api *apiClient, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("factory", flag.ContinueOnError)
	since := fs.String("since", "7d", "start (duration, date, or RFC3339)")
	until := fs.String("until", "", "exclusive end (date or RFC3339; default now)")
	release := fs.String("release", "", "full leader revision, unknown, or mixed")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if fs.NArg() != 0 {
		return fatal(jsonOut, "factory accepts flags only")
	}
	now := time.Now().UTC()
	start, err := factoryTime(*since, now, true)
	if err != nil {
		return fatal(jsonOut, "since: %v", err)
	}
	end := now
	if *until != "" {
		end, err = factoryTime(*until, now, false)
		if err != nil {
			return fatal(jsonOut, "until: %v", err)
		}
	}
	q := audit.FactoryQuery{Since: start, Until: end, Release: *release}
	if err := q.Validate(); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	report, err := callAPI[audit.FactoryReport](api, auditServiceName, "GetFactoryReport", q)
	if err != nil {
		return fatal(jsonOut, "factory report: %v", err)
	}
	return printJSON(report)
}

func factoryTime(raw string, now time.Time, duration bool) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if at, err := time.Parse(layout, raw); err == nil {
			return at, nil
		}
	}
	if duration {
		if days, ok := strings.CutSuffix(raw, "d"); ok {
			n, err := strconv.Atoi(days)
			if err == nil && n > 0 && n <= 31 {
				return now.Add(-time.Duration(n) * 24 * time.Hour), nil
			}
		} else if d, err := time.ParseDuration(raw); err == nil && d > 0 && d <= audit.FactoryMaxWindow {
			return now.Add(-d), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time boundary %q", raw)
}
