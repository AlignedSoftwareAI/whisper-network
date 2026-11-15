//go:build !linux

package sandbox

// runNetChecks is never reached off Linux: Probe does not run the self-test.
func runNetChecks(selfTestTargets, map[string]string) {}

func outcome(err error) string {
	if err == nil {
		return OutcomeOpen
	}
	return "error: " + err.Error()
}
