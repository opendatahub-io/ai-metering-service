package admission

import "github.com/opendatahub-io/ai-metering-service/internal/api"

// State is the value of an admission flag: the precomputed admission
// decision Praxis reads for one (identity object, resource) pair. The Rust
// admission crate must mirror these exact values.
type State string

const (
	StateAllow State = "allow"
	StateOver  State = "over"
	StateDeny  State = "deny"
)

// MostRestrictive folds two admission states: deny beats over beats allow.
// This single rule serves every place that combines states and they must all
// agree: the crate's read-time precedence across a user's group flags, the
// ingest-time fold across one event's charges, and the renewal recompute
// across every budget applying to an identity.
func MostRestrictive(a, b State) State {
	if a == StateDeny || b == StateDeny {
		return StateDeny
	}
	if a == StateOver || b == StateOver {
		return StateOver
	}
	return StateAllow
}

// OverageState maps a policy's overage configuration to the admission state
// written when its budget's balance goes negative. No overage configured
// means hard enforcement (deny). Soft overage keeps admitting with the
// over-budget signal (over). Fallback overage levels (group, organization,
// tenant) are a non-goal per the ADR — policy creation rejects them — so any
// other type reaching this point is an invalid or pre-validation row; deny
// is the defense-in-depth answer, since leaving the flag absent would read
// as "no budget applies" and fail open.
func OverageState(overage *api.OverageConfig) State {
	if overage == nil {
		return StateDeny
	}
	if overage.Type == "soft" {
		return StateOver
	}
	return StateDeny
}

// StateForBalance derives the state a policy contributes for its budget's
// current balance: a non-negative balance allows; a negative balance is
// overage — hard enforcement denies, soft overage keeps admitting with the
// over-budget signal.
func StateForBalance(balance int64, overage *api.OverageConfig) State {
	if balance >= 0 {
		return StateAllow
	}
	return OverageState(overage)
}
