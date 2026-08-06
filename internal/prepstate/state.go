package prepstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/project"
)

const MarkerName = ".sybra-prep-state"

type State struct {
	Branch    string `json:"branch"`
	HeadSHA   string `json:"head_sha"`
	Remote    string `json:"remote"`
	RemoteSHA string `json:"remote_sha"`
}

func WriteVerified(ctx context.Context, wtPath, branch string) (bool, error) {
	if branch == "" {
		var err error
		branch, err = project.CurrentBranch(ctx, wtPath)
		if err != nil {
			return false, fmt.Errorf("current branch: %w", err)
		}
	}
	headSHA, err := project.CurrentCommit(ctx, wtPath)
	if err != nil {
		return false, fmt.Errorf("current commit: %w", err)
	}
	remote := project.PushRemote(ctx, wtPath)
	remoteSHA, ok, err := project.RefreshedRemoteTrackingSHA(ctx, wtPath, remote, branch)
	if err != nil || !ok || remoteSHA != headSHA {
		_ = Clear(wtPath)
		return false, err
	}
	if err := addToInfoExclude(ctx, wtPath, MarkerName); err != nil {
		return false, err
	}
	data, err := json.Marshal(State{
		Branch:    branch,
		HeadSHA:   headSHA,
		Remote:    remote,
		RemoteSHA: remoteSHA,
	})
	if err != nil {
		return false, fmt.Errorf("marshal prep state: %w", err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, MarkerName), append(data, '\n'), 0o600); err != nil {
		return false, fmt.Errorf("write %s: %w", MarkerName, err)
	}
	return true, nil
}

func Reusable(ctx context.Context, wtPath, branch string) (bool, error) {
	state, ok, err := Read(wtPath)
	if err != nil || !ok {
		return false, err
	}
	if state.Branch != branch {
		return false, nil
	}
	currentBranch, err := project.CurrentBranch(ctx, wtPath)
	if err != nil || currentBranch != branch {
		return false, err
	}
	headSHA, err := project.CurrentCommit(ctx, wtPath)
	if err != nil || headSHA != state.HeadSHA {
		return false, err
	}
	if project.PushRemote(ctx, wtPath) != state.Remote {
		return false, nil
	}
	remoteSHA, ok, err := project.RefreshedRemoteTrackingSHA(ctx, wtPath, state.Remote, state.Branch)
	if err != nil {
		return false, err
	}
	if !ok || remoteSHA != state.RemoteSHA {
		return false, nil
	}
	return remoteSHA == state.HeadSHA, nil
}

func Read(wtPath string) (State, bool, error) {
	data, err := os.ReadFile(filepath.Join(wtPath, MarkerName))
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		return State{}, false, nil
	default:
		return State{}, false, fmt.Errorf("read %s: %w", MarkerName, err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, false, fmt.Errorf("parse %s: %w", MarkerName, err)
	}
	if state.Branch == "" || state.HeadSHA == "" || state.Remote == "" || state.RemoteSHA == "" {
		return State{}, false, nil
	}
	return state, true, nil
}

func Clear(wtPath string) error {
	if err := os.Remove(filepath.Join(wtPath, MarkerName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", MarkerName, err)
	}
	return nil
}

func addToInfoExclude(ctx context.Context, wtPath, entry string) error {
	out, err := gitexec.Output(ctx, gitexec.Options{Dir: wtPath}, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return fmt.Errorf("resolve info/exclude: %w", err)
	}
	excludePath := out
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(wtPath, excludePath)
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("mkdir info dir: %w", err)
	}
	existing, rerr := os.ReadFile(excludePath)
	if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return fmt.Errorf("read info/exclude: %w", rerr)
	}
	line := "/" + entry
	for raw := range strings.SplitSeq(string(existing), "\n") {
		if strings.TrimSpace(raw) == line {
			return nil
		}
	}
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open info/exclude: %w", err)
	}
	defer func() { _ = f.Close() }()
	prefix := ""
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		prefix = "\n"
	}
	if _, err := f.WriteString(prefix + line + "\n"); err != nil {
		return fmt.Errorf("append info/exclude: %w", err)
	}
	return nil
}
