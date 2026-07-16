package agent

import "strings"

const (
	processOwnerFlagEnv  = "SYBRA_AGENT_OWNER"
	processAgentIDEnv    = "SYBRA_AGENT_ID"
	processTaskIDEnv     = "SYBRA_TASK_ID"
	processAgentModeEnv  = "SYBRA_AGENT_MODE"
	processOwnerFlagTrue = "1"
)

type processOwner struct {
	AgentID string
	TaskID  string
	Mode    string
}

func processOwnerForAgent(a *Agent) processOwner {
	if a == nil {
		return processOwner{}
	}
	return processOwner{
		AgentID: strings.TrimSpace(a.ID),
		TaskID:  strings.TrimSpace(a.TaskID),
		Mode:    strings.TrimSpace(a.Mode),
	}
}

func processOwnerAssignments(owner processOwner) []string {
	if owner.AgentID == "" || owner.Mode == "" {
		return nil
	}
	assignments := []string{
		processOwnerFlagEnv + "=" + processOwnerFlagTrue,
		processAgentIDEnv + "=" + owner.AgentID,
		processAgentModeEnv + "=" + owner.Mode,
	}
	if owner.TaskID != "" {
		assignments = append(assignments, processTaskIDEnv+"="+owner.TaskID)
	}
	return assignments
}

func processOwnerFromEnvAssignments(assignments []string) processOwner {
	owner := processOwner{}
	marked := false
	for _, assignment := range assignments {
		key, value, ok := strings.Cut(assignment, "=")
		if !ok {
			continue
		}
		switch key {
		case processOwnerFlagEnv:
			if value != processOwnerFlagTrue {
				return processOwner{}
			}
			marked = true
		case processAgentIDEnv:
			owner.AgentID = value
		case processTaskIDEnv:
			owner.TaskID = value
		case processAgentModeEnv:
			owner.Mode = value
		}
	}
	if !marked || owner.AgentID == "" || owner.Mode == "" {
		return processOwner{}
	}
	return owner
}

func processOwnerFromAnyEnv(assignments []string) processOwner {
	if owner := mcpOwnerFromEnvAssignments(assignments); owner != (mcpOwner{}) {
		return processOwner(owner)
	}
	if owner := processOwnerFromEnvAssignments(assignments); owner != (processOwner{}) {
		return owner
	}
	return processOwner{}
}

func injectProcessOwnerEnv(cfg RunConfig, owner processOwner) RunConfig {
	assignments := processOwnerAssignments(owner)
	if len(assignments) == 0 {
		return cfg
	}
	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, processOwnerFlagEnv, processAgentIDEnv, processTaskIDEnv, processAgentModeEnv)
	cfg.ExtraEnv = append(cfg.ExtraEnv, assignments...)
	return cfg
}
