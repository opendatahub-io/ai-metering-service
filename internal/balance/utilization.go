package balance

import (
	"time"

	"github.com/opendatahub-io/ai-metering-service/internal/api"
	"github.com/opendatahub-io/ai-metering-service/internal/period"
)

// The utilization projection: a budget's current-period spend against its
// opening balance, computable at any time from the ledger — the mirror of
// what the renewal process enforces.

// ComputeUtilization derives a budget's current-period utilization
// snapshot from the ledger sums. Pure: no Redis access — opening minus
// spent is the same balance rebuildBalance writes, so the API reports the
// state admission enforces. spent is the current period's charges summed
// to now; prevSpent the completed period's (zero in the first period —
// the caller skips the sum, matching rebuildBalance).
func ComputeUtilization(budget api.BudgetResource, policies []api.BudgetPolicyResource, window period.Window, prevSpent, spent int64, now time.Time) api.BudgetUtilization {
	u := api.BudgetUtilization{
		BudgetID:   budget.Metadata.ID,
		BudgetName: budget.Spec.Name,
		Limit:      budget.Spec.Limit,
		Period:     api.UtilizationPeriod{Start: window.CurrentStart, End: window.NextEnd},
		Spent:      spent,
		PrevSpent:  prevSpent,
	}

	if window.PrevStart.Equal(window.CurrentStart) {
		// First period: nothing completed to earn carry or accrue debt —
		// the limit stands as the opening.
		u.Opening = budget.Spec.Limit
	} else {
		u.PrevOverage = max(0, prevSpent-budget.Spec.Limit)
		u.Carry, u.CarriedDebt = openingParts(budget.Spec, policies, prevSpent)
		u.Opening = budget.Spec.Limit + u.Carry - u.CarriedDebt
	}

	u.Remaining = u.Opening - spent
	u.Overage = max(0, -u.Remaining)
	if u.Opening > 0 {
		ratio := float64(spent) / float64(u.Opening)
		u.Utilization = &ratio
	}

	if elapsed := now.Sub(window.CurrentStart); elapsed >= 24*time.Hour {
		rate := float64(spent) / (elapsed.Hours() / 24)
		u.BurnRate = &rate
		if u.Remaining > 0 && rate > 0 {
			days := float64(u.Remaining) / rate
			exhaustion := now.Add(time.Duration(days * float64(24*time.Hour)))
			u.ProjectedExhaustion = &exhaustion
		}
	}
	return u
}
