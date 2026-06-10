module github.com/Automaat/sybra

go 1.26.4

require (
	github.com/fsnotify/fsnotify v1.10.1
	github.com/google/uuid v1.6.0
	github.com/prometheus/client_golang v1.23.2
	github.com/wailsapp/wails/v3 v3.0.0-alpha.98-tui
	go.opentelemetry.io/otel v1.43.0
	go.opentelemetry.io/otel/exporters/prometheus v0.65.0 // no v1; OTel metric exporters stabilize with OTel core
	go.opentelemetry.io/otel/metric v1.43.0
	go.opentelemetry.io/otel/sdk v1.43.0
	go.opentelemetry.io/otel/sdk/metric v1.43.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	dario.cat/mergo v1.0.2 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/ProtonMail/go-crypto v1.3.0 // indirect
	github.com/adrg/xdg v0.5.3 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/cyphar/filepath-securejoin v0.6.1 // indirect; no v1 released
	github.com/ebitengine/purego v0.10.0 // indirect; no v1 released
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/go-git/gcfg v1.5.1-0.20230307220236-3a3c6141e376 // indirect
	github.com/go-git/go-billy/v5 v5.9.0 // indirect
	github.com/go-git/go-git/v5 v5.19.1 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect; no v1; unmaintained (go-git dep)
	github.com/jbenet/go-context v0.0.0-20150711004518-d14ea06fba99 // indirect; no v1; abandoned (go-git dep)
	github.com/jchv/go-winloader v0.0.0-20250406163304-c1995be93bd1 // indirect; no v1; Windows-only Wails dep
	github.com/kevinburke/ssh_config v1.4.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect; no v1 released
	github.com/mattn/go-isatty v0.0.22 // indirect; no v1 released
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect; no v1; unmaintained (Prometheus dep)
	github.com/pjbgf/sha1cd v0.6.0 // indirect; no v1 released
	github.com/prometheus/client_model v0.6.2 // indirect; no v1 released
	github.com/prometheus/common v0.67.5 // indirect; no v1 released
	github.com/prometheus/otlptranslator v1.0.0 // indirect
	github.com/prometheus/procfs v0.20.1 // indirect; no v1 released
	github.com/sergi/go-diff v1.4.0 // indirect
	github.com/skeema/knownhosts v1.3.2 // indirect
	github.com/wailsapp/wails/webview2 v1.0.24 // indirect
	github.com/xanzy/ssh-agent v0.3.3 // indirect; no v1 released
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/trace v1.43.0 // indirect
	go.yaml.in/yaml/v2 v2.4.4 // indirect
	golang.org/x/crypto v0.50.0 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect; intentionally v0 by module name
)

require go.uber.org/goleak v1.3.0
