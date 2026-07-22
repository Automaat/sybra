package runoutcome

const (
	Started           = "started"
	Stalled           = "stalled"
	Completed         = "completed"
	Failed            = "failed"
	CancelledShutdown = "cancelled_shutdown"
	Superseded        = "superseded"
	Unknown           = "unknown"
)

func Normalize(outcome string) string {
	switch outcome {
	case Started, Stalled, Completed, Failed, CancelledShutdown, Superseded, Unknown:
		return outcome
	default:
		return Unknown
	}
}

func IsTerminal(outcome string) bool {
	outcome = Normalize(outcome)
	return outcome == Completed || outcome == Failed
}

func IsStallLike(outcome string) bool {
	outcome = Normalize(outcome)
	return outcome == Stalled || outcome == CancelledShutdown || outcome == Superseded
}
