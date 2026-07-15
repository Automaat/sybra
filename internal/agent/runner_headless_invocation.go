package agent

import (
	"fmt"
)

// buildHeadlessInvocation builds the subprocess invocation for a headless
// agent. The returned env slice contains "KEY=VALUE" entries the caller must
// merge into cmd.Env (Bash tool timeout for claude is delivered this way —
// claude has no CLI flag for it).
func buildHeadlessInvocation(a *Agent, cfg RunConfig) (name string, args, env []string, command string, err error) {
	prov, providerErr := providerForInvocation(a, cfg)
	if providerErr != nil {
		err = providerErr
		return
	}
	for _, tool := range cfg.AllowedTools {
		if !safeArgRe.MatchString(tool) {
			err = fmt.Errorf("invalid tool %q: must match %s", tool, safeArgRe)
			return
		}
	}
	if a.Model != "" && !safeArgRe.MatchString(a.Model) {
		err = fmt.Errorf("invalid model %q: must match %s", a.Model, safeArgRe)
		return
	}
	if cfg.FallbackModel != "" && !safeArgRe.MatchString(cfg.FallbackModel) {
		err = fmt.Errorf("invalid fallback model %q: must match %s", cfg.FallbackModel, safeArgRe)
		return
	}

	inv, buildErr := prov.BuildHeadlessInvocation(a, cfg)
	if buildErr != nil {
		err = buildErr
		return
	}
	return inv.name, inv.args, inv.env, inv.command, nil
}
