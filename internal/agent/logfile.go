package agent

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Automaat/sybra/internal/artifact"
)

// ParseLogFile reads an NDJSON agent log written by the headless runner and
// returns up to maxEvents StreamEvents (the last N if the file exceeds the
// cap). Headless logs persist the raw provider stream-json envelope, so we
// must run the same envelope→StreamEvent conversion the live runner uses;
// a flat json.Unmarshal into StreamEvent silently drops the nested
// `message.content[]` and the rendered UI shows labeled bubbles with empty
// text (the bug fixed alongside the interactive history rewrite).
//
// `provider` selects the parser ("claude", "codex", or "copilot"). Empty
// means the default Claude parser. Pass the value from AgentRun.Provider.
//
// Malformed lines are skipped silently.
func ParseLogFile(path string, maxEvents int, provider string) ([]StreamEvent, error) {
	return ParseLogFileWithArtifacts(path, maxEvents, provider, "", "", nil)
}

// ParseLogFileWithArtifacts mirrors ParseLogFile but applies the same bounded
// tool-result rewrite the live runner uses, optionally persisting oversized
// raw outputs into the task artifact store while it replays history.
func ParseLogFileWithArtifacts(path string, maxEvents int, provider, taskID, producerRole string, store *artifact.Store) ([]StreamEvent, error) {
	f, err := openAgentLogReader(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	prov, providerErr := lookupProvider(provider)
	if providerErr != nil {
		return nil, providerErr
	}
	var events []StreamEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		ev, parseErr := parseHeadlessEvent(line, prov)
		if parseErr != nil {
			continue
		}
		if ev.Type == "" {
			continue
		}
		ev = bindToolResultEvent(taskID, producerRole, store, ev)
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if maxEvents > 0 && len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}
	return events, nil
}

// ParseConvoLogFile reads an NDJSON log written by an interactive agent
// (Claude stream-json wire format) and returns up to maxEvents ConvoEvents.
//
// Interactive agents persist raw Anthropic-envelope lines
// (`{"type":"assistant","message":{"content":[...]}}`). Unmarshaling those
// directly into StreamEvent (flat Content string) silently drops the
// message body — the rendered UI shows labeled bubbles with empty text.
// This function unwraps the envelope via ParseClaudeLine +
// claudeEventToConvoEvent so history replay shows the same structure live
// agents do.
//
// Malformed lines are logged at debug level and skipped. `logger` may be
// nil (falls back to slog.Default) for callers that do not carry one.
func ParseConvoLogFile(path string, maxEvents int, logger *slog.Logger) ([]ConvoEvent, error) {
	if logger == nil {
		logger = slog.Default()
	}

	f, err := openAgentLogReader(path)
	if err != nil {
		return nil, fmt.Errorf("open convo log: %w", err)
	}
	defer func() { _ = f.Close() }()

	var (
		events  []ConvoEvent
		skipped int
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		ce, parseErr := ParseClaudeLine(line)
		if parseErr != nil {
			skipped++
			logger.Debug("convo.log.parse-skip", "path", path, "err", parseErr)
			continue
		}
		if ce.Type == "" {
			skipped++
			continue
		}
		events = append(events, claudeEventToConvoEvent(ce))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan convo log: %w", err)
	}
	if skipped > 0 {
		logger.Info("convo.log.parsed",
			"path", path, "events", len(events), "skipped", skipped)
	}
	if maxEvents > 0 && len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}
	return events, nil
}

// FindLogFile locates the NDJSON log for agentID inside logsDir/agents/ by
// globbing "{agentID}-*.ndjson". Retention may have gzip-compressed an aged
// log (see logging.EnforceAgentLogRetention), so a plain match is preferred
// but "{agentID}-*.ndjson.gz" is used as a fallback. Returns the first match
// or an error.
func FindLogFile(logsDir, agentID string) (string, error) {
	pattern := filepath.Join(logsDir, "agents", agentID+"-*.ndjson")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	if len(matches) > 0 {
		return matches[0], nil
	}
	gzMatches, err := filepath.Glob(pattern + ".gz")
	if err != nil {
		return "", err
	}
	if len(gzMatches) > 0 {
		return gzMatches[0], nil
	}
	return "", fmt.Errorf("no log file for agent %s", agentID)
}

// openAgentLogReader opens an agent NDJSON log for reading, transparently
// handling the gzip compression applied by retention: aged .ndjson files are
// rewritten to a sibling .ndjson.gz with the original removed (see
// logging.EnforceAgentLogRetention). Resolution order: the path as given,
// then "<path>.gz" when the plain .ndjson is gone. A .gz input — matched
// either way — is wrapped in a gzip.Reader so callers scan decompressed
// NDJSON. The returned ReadCloser closes both the decompressor and the
// underlying file.
func openAgentLogReader(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) && !strings.HasSuffix(path, ".gz") {
			if gzFile, gzErr := os.Open(path + ".gz"); gzErr == nil {
				return newGzipReadCloser(gzFile)
			}
		}
		return nil, err
	}
	if strings.HasSuffix(path, ".gz") {
		return newGzipReadCloser(f)
	}
	return f, nil
}

// gzipReadCloser couples a gzip.Reader to its backing file so both are closed
// together.
type gzipReadCloser struct {
	gz *gzip.Reader
	f  *os.File
}

func (g *gzipReadCloser) Read(p []byte) (int, error) { return g.gz.Read(p) }

func (g *gzipReadCloser) Close() error {
	err := g.gz.Close()
	if cerr := g.f.Close(); err == nil {
		err = cerr
	}
	return err
}

func newGzipReadCloser(f *os.File) (io.ReadCloser, error) {
	gz, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &gzipReadCloser{gz: gz, f: f}, nil
}
