// Package runenv certifies the concrete environment an agent will use before
// provider tokens are spent. It deliberately owns no task or project stores;
// mutation, repair, quarantine, and audit are narrow callbacks supplied by the
// application control plane.
package runenv

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/textutil"
)

const (
	defaultTTL = 2 * time.Minute
	failureTTL = 30 * time.Second
)

// Observation is immutable evidence for one required capability.
type Observation struct {
	Capability autonomy.Capability `json:"capability"`
	Scope      string              `json:"scope"`
	Satisfied  bool                `json:"satisfied"`
	Available  bool                `json:"available"`
	Contained  bool                `json:"contained"`
	Code       string              `json:"code,omitempty"`
	Evidence   string              `json:"evidence,omitempty"`
	Repairable bool                `json:"repairable"`
	ObservedAt time.Time           `json:"observedAt"`
}

// Certificate is the time-bounded admission proof for one action and exact
// run environment fingerprint.
type Certificate struct {
	ID           string        `json:"id"`
	Fingerprint  string        `json:"fingerprint"`
	TaskID       string        `json:"taskId"`
	ProjectID    string        `json:"projectId,omitempty"`
	Action       string        `json:"action"`
	Observations []Observation `json:"observations"`
	ObservedAt   time.Time     `json:"observedAt"`
	ExpiresAt    time.Time     `json:"expiresAt"`
	Repaired     bool          `json:"repaired"`
}

func (c Certificate) Current(now time.Time) bool {
	return c.ID != "" && now.Before(c.ExpiresAt) && !slices.ContainsFunc(c.Observations, func(o Observation) bool { return !o.Satisfied })
}

// Request describes the actual paths and policy selected for a pending run.
type Request struct {
	TaskID               string
	ProjectID            string
	Action               string
	WorkDir              string
	ReadRoots            []string
	GitRoots             []string
	ScratchRoots         []string
	CloneDir             string
	TaskBranch           string
	Provider             string
	SandboxMode          string
	SigningPolicy        project.SigningPolicy
	TaskMutationIdentity string
	Requirements         []autonomy.CapabilityRequirement
	ConfigVersion        string
	CloneGeneration      string
}

// ProbeResult is returned by application-owned probes.
type ProbeResult struct {
	Available bool
	Contained bool
	Code      string
	Evidence  string
}

// CertificationError is the stable machine-owned reason passed to quarantine policy.
type CertificationError struct {
	TaskID     string
	ProjectID  string
	Action     string
	Scope      string
	Code       string
	Capability autonomy.Capability
	Cause      error
}

func (f CertificationError) Error() string {
	if f.Cause != nil {
		return fmt.Sprintf("run environment certification failed: %s (%s): %v", f.Code, f.Capability, f.Cause)
	}
	return fmt.Sprintf("run environment certification failed: %s (%s)", f.Code, f.Capability)
}

func (f CertificationError) Unwrap() error { return f.Cause }

// MachineFailureCode lets workflow admission classify certification failures
// as machine-owned without importing this App-scoped package and creating an
// import cycle.
func (f CertificationError) MachineFailureCode() string { return f.Code }

// Deps keeps state-changing authority outside this package.
type Deps struct {
	ProbeSandbox      func(context.Context, string) (ProbeResult, error)
	ProbeProvider     func(context.Context, string) (ProbeResult, error)
	ProbeNetwork      func(context.Context, string) (ProbeResult, error)
	ProbeTaskMutation func(context.Context, string) (ProbeResult, error)
	Repair            func(context.Context, Request, []Observation) error
	Quarantine        func(context.Context, CertificationError)
	Audit             func(string, Certificate, *CertificationError)
	Now               func() time.Time
	TTL               time.Duration
}

type cacheEntry struct{ cert Certificate }
type quarantineEntry struct {
	failure   CertificationError
	expiresAt time.Time
}

// Service serializes certification and repair per shared project clone.
type Service struct {
	deps        Deps
	mu          sync.Mutex
	cache       map[string]cacheEntry
	locks       map[string]*sync.Mutex
	quarantined map[string]quarantineEntry
}

func New(deps Deps) *Service {
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.TTL <= 0 {
		deps.TTL = defaultTTL
	}
	return &Service{deps: deps, cache: map[string]cacheEntry{}, locks: map[string]*sync.Mutex{}, quarantined: map[string]quarantineEntry{}}
}

// InvalidateTask drops otherwise-current evidence after a provider reports a
// filesystem, object, or signing failure. The next retry must re-observe and
// may repair before another provider process starts.
func (s *Service) InvalidateTask(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.cache {
		if s.cache[key].cert.TaskID == taskID {
			delete(s.cache, key)
		}
	}
}

// IsEnvironmentFailure recognizes provider/start errors that invalidate a
// certificate. It is diagnostic only; admission policy still branches on the
// typed observation produced by the following certification pass.
func IsEnvironmentFailure(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"read-only file system", "operation not permitted", "bad object", "invalid object", "missing blob", "missing commit", "unable to read tree", "gpg failed to sign", "signer unavailable"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// Certify returns a current certificate or a typed failure. At most one safe
// repair is attempted, under the same clone-scoped lock, then every required
// capability is observed again from scratch.
func (s *Service) Certify(ctx context.Context, req Request) (Certificate, error) {
	if err := validate(req); err != nil {
		return Certificate{}, err
	}
	lock := s.keyLock(firstNonEmpty(req.CloneDir, req.ProjectID, "global"))
	lock.Lock()
	defer lock.Unlock()
	key, err := fingerprint(ctx, req)
	if err != nil {
		return Certificate{}, err
	}
	now := s.deps.Now()
	s.mu.Lock()
	s.pruneExpiredLocked(now)
	entry, ok := s.cache[key]
	s.mu.Unlock()
	if ok && entry.cert.Current(now) {
		return entry.cert, nil
	}
	if ok && now.Before(entry.cert.ExpiresAt) {
		if failed := failedObservations(entry.cert); len(failed) > 0 {
			return Certificate{}, failureFrom(req, failed[0])
		}
	}
	if failure, blocked := s.activeQuarantine(req, now); blocked {
		failure.TaskID, failure.Action = req.TaskID, req.Action
		return Certificate{}, failure
	}

	cert := s.observe(ctx, req, key, false)
	failed := failedObservations(cert)
	if len(failed) > 0 && slices.ContainsFunc(failed, func(o Observation) bool { return o.Repairable }) && s.deps.Repair != nil {
		if repairErr := s.deps.Repair(ctx, req, failed); repairErr == nil {
			if s.deps.Audit != nil {
				s.deps.Audit("runenv.repair", cert, nil)
			}
			freshKey, fpErr := fingerprint(ctx, req)
			if fpErr == nil {
				key = freshKey
			}
			cert = s.observe(ctx, req, key, true)
			failed = failedObservations(cert)
		}
	}
	if len(failed) > 0 {
		failure := failureFrom(req, failed[0])
		cert.ExpiresAt = s.deps.Now().Add(failureTTL)
		s.mu.Lock()
		s.cache[key] = cacheEntry{cert: cert}
		s.mu.Unlock()
		// Provider and GitHub availability are external/transient admission
		// signals, not evidence that a host, project, or task filesystem is
		// unsafe. Cache them to suppress duplicate starts, but never apply the
		// machine-owned environment quarantine to tasks.
		if failure.Capability != autonomy.CapabilityProviderCapacity && failure.Capability != autonomy.CapabilityNetworkGitHub {
			s.quarantineOnce(ctx, key, failure, cert)
		}
		return Certificate{}, failure
	}
	s.mu.Lock()
	s.cache[key] = cacheEntry{cert: cert}
	for _, observation := range cert.Observations {
		delete(s.quarantined, quarantineScopeKey(req, observation.Scope, observation.Capability))
	}
	s.mu.Unlock()
	if s.deps.Audit != nil {
		s.deps.Audit("runenv.certified", cert, nil)
	}
	return cert, nil
}

func (s *Service) pruneExpiredLocked(now time.Time) {
	for key := range s.cache {
		if !now.Before(s.cache[key].cert.ExpiresAt) {
			delete(s.cache, key)
		}
	}
	for key := range s.quarantined {
		if !now.Before(s.quarantined[key].expiresAt) {
			delete(s.quarantined, key)
		}
	}
}

func (s *Service) observe(ctx context.Context, req Request, key string, repaired bool) Certificate {
	now := s.deps.Now()
	cert := Certificate{ID: certificateID(key, now), Fingerprint: key, TaskID: req.TaskID, ProjectID: req.ProjectID, Action: req.Action, ObservedAt: now, ExpiresAt: now.Add(s.deps.TTL), Repaired: repaired}
	for _, requirement := range req.Requirements {
		obs := Observation{Capability: requirement.Capability, Scope: requirement.Scope, Repairable: requirement.Repairable, ObservedAt: now}
		result, err := s.probe(ctx, req, requirement.Capability)
		obs.Available, obs.Contained, obs.Code, obs.Evidence = result.Available, result.Contained, result.Code, result.Evidence
		obs.Satisfied = err == nil && result.Available
		if err != nil && obs.Code == "" {
			obs.Code = capabilityCode(requirement.Capability)
		}
		if err != nil && obs.Evidence == "" {
			obs.Evidence = err.Error()
		}
		cert.Observations = append(cert.Observations, obs)
	}
	return cert
}

func (s *Service) probe(ctx context.Context, req Request, capability autonomy.Capability) (ProbeResult, error) {
	switch capability {
	case autonomy.CapabilitySourceRead:
		for _, root := range append([]string{req.WorkDir}, req.ReadRoots...) {
			if err := probeRead(root); err != nil {
				return ProbeResult{Code: "source_read_unavailable"}, err
			}
		}
		return ProbeResult{Available: true, Evidence: "source roots readable"}, nil
	case autonomy.CapabilitySourceWrite:
		if err := probeWrite(req.WorkDir); err != nil {
			return ProbeResult{Code: "source_write_unavailable"}, err
		}
		return ProbeResult{Available: true, Evidence: "source root writable"}, nil
	case autonomy.CapabilityScratchWrite:
		for _, root := range req.ScratchRoots {
			if err := probeWrite(root); err != nil {
				return ProbeResult{Code: "scratch_write_unavailable"}, err
			}
		}
		return ProbeResult{Available: true, Evidence: "required scratch roots writable"}, nil
	case autonomy.CapabilityGitAdminWrite:
		gitDir, common, err := gitDirs(ctx, req.WorkDir)
		if err != nil {
			return ProbeResult{Code: "git_admin_invalid"}, err
		}
		if err := probeWrite(gitDir); err != nil {
			return ProbeResult{Code: "git_admin_readonly"}, err
		}
		if common != gitDir {
			if err := probeWrite(common); err != nil {
				return ProbeResult{Code: "git_admin_readonly"}, err
			}
		}
		return ProbeResult{Available: true, Evidence: "git admin writable; common dir=" + common}, nil
	case autonomy.CapabilityCheckoutHealth:
		if err := probeCheckouts(ctx, req); err != nil {
			return ProbeResult{Code: "checkout_unhealthy"}, err
		}
		return ProbeResult{Available: true, Evidence: "checkout Git metadata and referenced objects readable"}, nil
	case autonomy.CapabilityObjectStore:
		if err := probeObjectStore(ctx, req); err != nil {
			return ProbeResult{Code: "object_store_unhealthy"}, err
		}
		return ProbeResult{Available: true, Evidence: "shared object store and refs readable"}, nil
	case autonomy.CapabilitySigning:
		if req.SigningPolicy.SignsCommits(ctx) {
			if err := project.ProbeGPGSigning(ctx); err != nil {
				return ProbeResult{Code: "signer_unavailable"}, err
			}
		}
		return ProbeResult{Available: true, Evidence: "signing policy " + string(req.SigningPolicy)}, nil
	case autonomy.CapabilityTaskMutation:
		return callProbe(ctx, s.deps.ProbeTaskMutation, req.TaskID, "task_mutation_unavailable")
	case autonomy.CapabilitySandboxMechanism:
		return callProbe(ctx, s.deps.ProbeSandbox, req.SandboxMode, "sandbox_unavailable")
	case autonomy.CapabilityProviderCapacity:
		return callProbe(ctx, s.deps.ProbeProvider, req.Provider, "provider_unavailable")
	case autonomy.CapabilityNetworkGitHub:
		return callProbe(ctx, s.deps.ProbeNetwork, req.ProjectID, "github_network_unavailable")
	default:
		return ProbeResult{Code: "unknown_capability"}, fmt.Errorf("unknown capability %q", capability)
	}
}

func callProbe(ctx context.Context, fn func(context.Context, string) (ProbeResult, error), value, code string) (ProbeResult, error) {
	if fn == nil {
		return ProbeResult{Code: code}, errors.New("probe is not configured")
	}
	r, err := fn(ctx, value)
	if err != nil && r.Code == "" {
		r.Code = code
	}
	return r, err
}

func probeRead(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func probeWrite(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("write root is empty")
	}
	f, err := os.CreateTemp(dir, ".sybra-runenv-probe-")
	if err != nil {
		return err
	}
	name := f.Name()
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(name)
		return closeErr
	}
	return os.Remove(name)
}

func gitDirs(ctx context.Context, workDir string) (gitDir, common string, err error) {
	gitDir, err = gitexec.Output(ctx, gitexec.Options{Dir: workDir}, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return "", "", err
	}
	common, err = project.CommonDir(ctx, workDir)
	if err != nil {
		return "", "", err
	}
	if info, statErr := os.Stat(gitDir); statErr != nil || !info.IsDir() {
		return "", "", fmt.Errorf("invalid git dir %q", gitDir)
	}
	if info, statErr := os.Stat(common); statErr != nil || !info.IsDir() {
		return "", "", fmt.Errorf("invalid git common dir %q", common)
	}
	return gitDir, common, nil
}

func probeCheckouts(ctx context.Context, req Request) error {
	for _, root := range requestGitRoots(req) {
		if _, _, err := gitDirs(ctx, root); err != nil {
			return err
		}
		if _, err := gitexec.Output(ctx, gitexec.Options{Dir: root}, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
			return err
		}
		if _, err := gitexec.Output(ctx, gitexec.Options{Dir: root}, "cat-file", "-e", "HEAD^{tree}"); err != nil {
			return err
		}
		if err := probeIndexObjects(ctx, root); err != nil {
			return err
		}
	}
	return nil
}

func probeObjectStore(ctx context.Context, req Request) error {
	if req.CloneDir != "" {
		return project.CheckBareCloneHealth(ctx, req.CloneDir)
	}
	return nil
}

func requestGitRoots(req Request) []string {
	if len(req.GitRoots) > 0 {
		return req.GitRoots
	}
	if slices.ContainsFunc(req.Requirements, func(requirement autonomy.CapabilityRequirement) bool {
		return requirement.Capability == autonomy.CapabilityGitAdminWrite || requirement.Capability == autonomy.CapabilityCheckoutHealth
	}) {
		return []string{req.WorkDir}
	}
	return nil
}

func probeIndexObjects(ctx context.Context, workDir string) error {
	staged, err := gitexec.RawOutput(ctx, gitexec.Options{Dir: workDir}, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	var objectIDs strings.Builder
	for record := range bytes.SplitSeq(staged, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		header, _, ok := bytes.Cut(record, []byte{'\t'})
		if !ok {
			return errors.New("malformed staged index entry")
		}
		fields := bytes.Fields(header)
		if len(fields) < 2 || string(fields[0]) == "160000" {
			continue
		}
		objectIDs.Write(fields[1])
		objectIDs.WriteByte('\n')
	}
	if objectIDs.Len() == 0 {
		return nil
	}
	out, err := gitexec.Output(ctx, gitexec.Options{Dir: workDir, Stdin: strings.NewReader(objectIDs.String())}, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return err
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), " missing") {
			return fmt.Errorf("index references unavailable object: %s", line)
		}
	}
	return nil
}

func validate(req Request) error {
	if strings.TrimSpace(req.TaskID) == "" || strings.TrimSpace(req.Action) == "" || strings.TrimSpace(req.WorkDir) == "" {
		return errors.New("runenv request requires task, action, and work directory")
	}
	if len(req.Requirements) == 0 {
		return errors.New("runenv request requires capabilities")
	}
	for _, r := range req.Requirements {
		if err := r.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func fingerprint(ctx context.Context, req Request) (string, error) {
	parts := []string{req.TaskID, req.ProjectID, req.Action, req.WorkDir, req.CloneDir, req.CloneGeneration, req.TaskBranch, req.Provider, req.SandboxMode, string(req.SigningPolicy), req.TaskMutationIdentity, req.ConfigVersion}
	if abs, err := filepath.EvalSymlinks(req.WorkDir); err == nil {
		parts = append(parts, abs)
	}
	for _, root := range append(append([]string{req.WorkDir}, req.ReadRoots...), req.ScratchRoots...) {
		parts = append(parts, pathIdentity(root))
	}
	for _, root := range requestGitRoots(req) {
		parts = append(parts, "git-root", root)
		gitDir, common, err := gitDirs(ctx, root)
		if err != nil {
			parts = append(parts, "unavailable", err.Error())
			continue
		}
		parts = append(parts,
			pathIdentity(gitDir),
			pathIdentity(common),
			fileIdentity(filepath.Join(gitDir, "index")),
			fileIdentity(filepath.Join(gitDir, "HEAD")),
			directoryIdentity(filepath.Join(common, "refs")),
			fileIdentity(filepath.Join(common, "packed-refs")),
			objectStoreIdentity(filepath.Join(common, "objects")),
		)
		if head, err := gitexec.Output(ctx, gitexec.Options{Dir: root}, "rev-parse", "HEAD"); err == nil {
			parts = append(parts, head)
		}
	}
	if req.CloneDir != "" {
		parts = append(parts,
			pathIdentity(req.CloneDir),
			objectStoreIdentity(filepath.Join(req.CloneDir, "objects")),
			directoryIdentity(filepath.Join(req.CloneDir, "refs")),
			fileIdentity(filepath.Join(req.CloneDir, "packed-refs")),
			fileIdentity(filepath.Join(req.CloneDir, "HEAD")),
		)
	}
	for _, r := range req.Requirements {
		parts = append(parts, string(r.Capability), r.Scope, strconv.FormatBool(r.Repairable))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:]), nil
}

func pathIdentity(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path + "|missing"
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return resolved + "|missing"
	}
	return resolved + "|" + info.Mode().String()
}

func fileIdentity(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path + "|missing"
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return resolved + "|unavailable|" + err.Error()
	}
	return resolved + "|" + info.Mode().String() + "|" + strconv.FormatInt(info.Size(), 10) + "|" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

// directoryIdentity deliberately caps traversal so fingerprint lookup remains
// cheap on repositories with very large ref namespaces. The current HEAD is
// fingerprinted separately, and clone generation covers shared-clone refresh.
func directoryIdentity(path string) string {
	const maxEntries = 256
	hash := sha256.New()
	entries, err := os.ReadDir(path)
	if err != nil {
		return path + "|unavailable|" + err.Error()
	}
	written := 0
	for _, entry := range entries {
		if written == maxEntries {
			_, _ = io.WriteString(hash, "truncated\n")
			break
		}
		if err := writeEntryIdentity(hash, entry.Name(), entry); err != nil {
			return path + "|unavailable|" + err.Error()
		}
		written++
		if !entry.IsDir() {
			continue
		}
		children, readErr := os.ReadDir(filepath.Join(path, entry.Name()))
		if readErr != nil {
			return path + "|unavailable|" + readErr.Error()
		}
		for _, child := range children {
			if written == maxEntries {
				_, _ = io.WriteString(hash, "truncated\n")
				break
			}
			if err := writeEntryIdentity(hash, entry.Name()+"/"+child.Name(), child); err != nil {
				return path + "|unavailable|" + err.Error()
			}
			written++
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// objectStoreIdentity is deliberately bounded by Git's object-store layout:
// loose objects live one level below a two-hex fanout directory, so the
// fanout directory's metadata changes when a loose object is added/removed.
// Pack/info contain a small number of generation files whose metadata is also
// included. This detects structural mutations without walking every object.
func objectStoreIdentity(path string) string {
	hash := sha256.New()
	entries, err := os.ReadDir(path)
	if err != nil {
		return path + "|unavailable|" + err.Error()
	}
	for _, entry := range entries {
		if err := writeEntryIdentity(hash, entry.Name(), entry); err != nil {
			return path + "|unavailable|" + err.Error()
		}
		if !entry.IsDir() || (entry.Name() != "pack" && entry.Name() != "info") {
			continue
		}
		children, readErr := os.ReadDir(filepath.Join(path, entry.Name()))
		if readErr != nil {
			return path + "|unavailable|" + readErr.Error()
		}
		for _, child := range children {
			if err := writeEntryIdentity(hash, entry.Name()+"/"+child.Name(), child); err != nil {
				return path + "|unavailable|" + err.Error()
			}
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeEntryIdentity(w io.Writer, name string, entry os.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, name+"|"+entry.Type().String()+"|"+strconv.FormatInt(info.Size(), 10)+"|"+strconv.FormatInt(info.ModTime().UnixNano(), 10)+"\n")
	return err
}

func certificateID(key string, now time.Time) string {
	sum := sha256.Sum256([]byte(key + now.String()))
	return "runenv-" + textutil.TruncateBytes(hex.EncodeToString(sum[:]), 16, "")
}
func failedObservations(c Certificate) []Observation {
	return slices.DeleteFunc(slices.Clone(c.Observations), func(o Observation) bool { return o.Satisfied })
}
func capabilityCode(c autonomy.Capability) string {
	return strings.ReplaceAll(string(c), "_health", "") + "_unavailable"
}
func failureFrom(req Request, o Observation) CertificationError {
	var cause error
	if strings.TrimSpace(o.Evidence) != "" {
		cause = errors.New(o.Evidence)
	}
	return CertificationError{TaskID: req.TaskID, ProjectID: req.ProjectID, Action: req.Action, Scope: o.Scope, Code: firstNonEmpty(o.Code, capabilityCode(o.Capability)), Capability: o.Capability, Cause: cause}
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (s *Service) keyLock(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lock := s.locks[key]; lock != nil {
		return lock
	}
	lock := &sync.Mutex{}
	s.locks[key] = lock
	return lock
}

func (s *Service) quarantineOnce(ctx context.Context, _ string, failure CertificationError, cert Certificate) {
	key := quarantineScopeKey(Request{TaskID: failure.TaskID, ProjectID: failure.ProjectID}, failure.Scope, failure.Capability)
	s.mu.Lock()
	_, exists := s.quarantined[key]
	if !exists {
		s.quarantined[key] = quarantineEntry{failure: failure, expiresAt: cert.ExpiresAt}
	}
	s.mu.Unlock()
	if exists {
		return
	}
	if s.deps.Quarantine != nil {
		s.deps.Quarantine(ctx, failure)
	}
	if s.deps.Audit != nil {
		s.deps.Audit("runenv.quarantined", cert, &failure)
	}
}

func (s *Service) activeQuarantine(req Request, now time.Time) (CertificationError, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, requirement := range req.Requirements {
		key := quarantineScopeKey(req, requirement.Scope, requirement.Capability)
		entry, ok := s.quarantined[key]
		if !ok {
			continue
		}
		if !now.Before(entry.expiresAt) {
			delete(s.quarantined, key)
			continue
		}
		return entry.failure, true
	}
	return CertificationError{}, false
}

func quarantineScopeKey(req Request, scope string, capability autonomy.Capability) string {
	identity := req.TaskID
	switch scope {
	case "host":
		identity = "host"
	case "project":
		identity = req.ProjectID
	case "provider":
		identity = "provider"
	}
	return scope + "|" + identity + "|" + string(capability)
}
