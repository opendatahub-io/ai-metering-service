package balance

import (
	"github.com/opendatahub-io/ai-metering-service/internal/api"
)

// The opening-balance rules — what a period starts with, given the budget
// spec, its policies, and the completed period's spend. Shared by the
// renewal process (which materializes the opening into Redis at the
// boundary) and the utilization projection (which recomputes it on demand
// from the ledger), so the two can never disagree on the arithmetic.

// NewBalance computes the balance a budget starts a period with:
//
//	drain:    limit
//	rollover: limit + min(limit - prevSpent, max_carry) when unused
//	deduct_overage (any policy referencing the budget): minus the previous
//	          period's overspend
//
// deduct_overage is configured on the BudgetPolicy but the balance is per
// budget; when several policies reference the same budget, the first one
// enabling it applies. prevSpent is the closed period's ledger sum, so the
// result is deterministic per period — a retried renewal writes the same
// value.
func NewBalance(budget api.Budget, policies []api.BudgetPolicyResource, prevSpent int64) int64 {
	carry, debt := openingParts(budget, policies, prevSpent)
	return budget.Limit + carry - debt
}

// openingParts breaks a period's opening balance into its components: the
// rollover carry granted from the completed period's unused budget
// (capped by max_carry), and the completed period's overage deducted when
// a policy referencing the budget enables deduct_overage (the first one
// enabling it applies — the balance is per budget). NewBalance is their
// sum with the limit; ComputeUtilization reports them separately.
func openingParts(budget api.Budget, policies []api.BudgetPolicyResource, prevSpent int64) (carry, debt int64) {
	if budget.RenewalType == api.RenewalRollover {
		if unused := budget.Limit - prevSpent; unused > 0 {
			carry = unused
			if budget.MaxCarry != nil && carry > *budget.MaxCarry {
				carry = *budget.MaxCarry
			}
		}
	}
	for _, p := range policies {
		if p.Spec.DeductOverage {
			if overage := prevSpent - budget.Limit; overage > 0 {
				debt = overage
			}
			break
		}
	}
	return carry, debt
}
