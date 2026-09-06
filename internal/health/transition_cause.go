package health

import (
	"strings"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/taskstatus"
)

type TransitionCause string

const (
	CauseUnknown            TransitionCause = "unknown"
	CauseIncidentRecurrence TransitionCause = "incident_recurrence"
	CauseInfrastructure     TransitionCause = "infrastructure"
	CausePlanning           TransitionCause = "planning_insufficiency"
	CauseSpecification      TransitionCause = "specification_gap"
	CauseOperator           TransitionCause = "operator_decision"
)

// ClassifyTransition uses only structured control-plane evidence. Headless is
// an execution transport, not evidence of task simplicity or planning quality.
func ClassifyTransition(e audit.Event) TransitionCause {
	if e.Type != audit.EventTaskStatusChanged {
		return CauseUnknown
	}
	actor, _ := e.Data["actor"].(string)
	from, _ := e.Data["from"].(string)
	to, _ := e.Data["to"].(string)
	if IsIncidentReopen(from, to, actor) {
		return CauseIncidentRecurrence
	}
	if to != string(taskstatus.HumanRequired) && to != string(taskstatus.Blocked) {
		return CauseUnknown
	}
	code, _ := e.Data["escalation_code"].(string)
	owner, _ := e.Data["failure_owner"].(string)
	provenance, _ := e.Data["evidence_provenance"].(string)
	if code == "" || (provenance != string(autonomy.ProvenanceControlPlane) && provenance != string(autonomy.ProvenanceOperator) && provenance != string(autonomy.ProvenanceGitHub) && provenance != string(autonomy.ProvenanceFilesystem) && provenance != string(autonomy.ProvenanceGit)) {
		return CauseUnknown
	}
	switch autonomy.FailureOwner(owner) {
	case autonomy.FailureOwnerMachine, autonomy.FailureOwnerExternalTransient:
		return CauseInfrastructure
	case autonomy.FailureOwnerSpecification:
		if strings.HasPrefix(code, "planning.") {
			return CausePlanning
		}
		return CauseSpecification
	case autonomy.FailureOwnerOperatorAuthority, autonomy.FailureOwnerOperatorDecision, autonomy.FailureOwnerPolicy:
		return CauseOperator
	default:
		return CauseUnknown
	}
}

func IsIncidentReopen(from, to, actor string) bool {
	return actor == "monitor.incident.reopen" && (from == string(taskstatus.Done) || from == string(taskstatus.Cancelled)) && to == string(taskstatus.Todo)
}

func nextAction(cause TransitionCause) string {
	switch cause {
	case CauseIncidentRecurrence:
		return "inspect_incident_recurrence"
	case CauseInfrastructure:
		return "repair_execution_environment"
	case CausePlanning:
		return "inspect_planning_failure_before_replan"
	case CauseSpecification:
		return "clarify_requirements"
	case CauseOperator:
		return "await_operator_decision"
	default:
		return "investigate_missing_evidence"
	}
}
