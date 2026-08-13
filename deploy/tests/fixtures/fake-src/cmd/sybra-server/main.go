// Stand-in for cmd/sybra-server used only by deploy/tests: mirrors the two
// behaviors sybra-build.sh and sybra-healthcheck.sh actually drive
// (-check-config preflight, /health serving) without pulling in the real
// module's dependency graph, so the deploy integration tests build fast and
// hermetically.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
)

func main() {
	checkConfig := flag.Bool("check-config", false, "")
	flag.Parse()

	if *checkConfig {
		if os.Getenv("FAKE_CHECK_CONFIG") == "fail" {
			fmt.Fprintln(os.Stderr, "config: invalid: fake rejected key")
			os.Exit(1)
		}
		fmt.Println("config: ok")
		return
	}

	addr := os.Getenv("FAKE_LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:18080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if os.Getenv("FAKE_HEALTH_MODE") == "fail" {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	// A board that terminates TLS is the case sybra-healthcheck.sh has to
	// authenticate by pinning, so the stand-in has to be able to serve it.
	if cert, key := os.Getenv("FAKE_TLS_CERT"), os.Getenv("FAKE_TLS_KEY"); cert != "" && key != "" {
		if err := http.ListenAndServeTLS(addr, cert, key, mux); err != nil {
			fmt.Fprintln(os.Stderr, "listen tls:", err)
			os.Exit(1)
		}
		return
	}
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
}
