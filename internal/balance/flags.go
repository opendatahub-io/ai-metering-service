package balance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/opendatahub-io/ai-metering-service/internal/accounting"
	"github.com/opendatahub-io/ai-metering-service/internal/admission"
	"github.com/opendatahub-io/ai-metering-service/internal/api"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
)

// recomputeFlags rewrites the admission flags for every (identity, resource)
// pair ever charged against the budget (see store.ListChargeTargets for why
// the set is unbounded in time). It must run after the balance refill, not
// before, so the states below derive from the new balance.
func recomputeFlags(ctx context.Context, s store.Store, kv store.KeyStore, tenantID, budgetID string) error {
	targets, err := s.ListChargeTargets(ctx, tenantID, budgetID)
	if err != nil {
		return fmt.Errorf("list charge targets for budget %s: %w", budgetID, err)
	}
	for _, target := range targets {
		if err := recomputeFlag(ctx, s, kv, tenantID, target.Identity, target.Resource); err != nil {
			return fmt.Errorf("recompute admission flag for budget %s: %w", budgetID, err)
		}
	}
	return nil
}

// recomputeFlag derives the admission states for one (identity, resource)
// pair from current state — the full fold across every policy that matches
// the identity and rates the resource, most restrictive outcome per key,
// exactly the ADR's definition of a fully resolved flag. The policy
// resolution is shared with the ingest path (accounting.ResolveApplicable);
// unlike ingest, which folds only the current event's charges against the
// balances it just decremented, this reads every matching budget's live
// balance — so it both lifts a deny whose budget was renewed and keeps a
// deny driven by a different, still-exhausted budget.
//
// Keys whose policies no longer match (a policy was deleted, or its rate no
// longer covers the resource) are left untouched rather than deleted:
// deleting would read as "no budget applies" and fail open. Clearing stale
// flags is the reconcile path's job.
func recomputeFlag(ctx context.Context, s store.Store, kv store.KeyStore, tenantID string, identity api.Identity, resource map[string]string) error {
	applicable, err := accounting.ResolveApplicable(ctx, s, tenantID, identity, resource)
	if err != nil {
		return fmt.Errorf("resolve applicable policies: %w", err)
	}

	states := make(map[string]admission.State)
	for _, a := range applicable {
		var balance int64
		balanceStr, err := kv.GetKey(ctx, tenantID, admission.BuildBalanceKey(tenantID, a.Policy.Spec.BudgetRef))
		switch {
		case errors.Is(err, store.ErrKeyNotFound):
			// Redis has no balance state for this budget — lost to a flush
			// or failover, with the periodic reconcile not yet caught up.
			// Rebuild it from the ledger rather than skipping the budget's
			// contribution: the rebuild writes the key back, so later
			// readers find it too. A budget that no longer resolves (policy
			// outliving its budget) keeps the skip behavior — it must not
			// fail the whole recompute.
			budget, err := s.GetBudget(ctx, tenantID, a.Policy.Spec.BudgetRef)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("get budget %s: %w", a.Policy.Spec.BudgetRef, err)
			}
			balance, err = rebuildBalance(ctx, s, kv, *budget)
			if err != nil {
				return fmt.Errorf("rebuild balance for budget %s: %w", a.Policy.Spec.BudgetRef, err)
			}
		case err != nil:
			return fmt.Errorf("read balance for budget %s: %w", a.Policy.Spec.BudgetRef, err)
		default:
			balance, err = strconv.ParseInt(balanceStr, 10, 64)
			if err != nil {
				return fmt.Errorf("parse balance %q for budget %s: %w", balanceStr, a.Policy.Spec.BudgetRef, err)
			}
		}

		state := admission.StateForBalance(balance, a.Policy.Spec.Overage)
		for _, key := range a.AdmissionKeys {
			states[key] = admission.MostRestrictive(states[key], state)
		}
	}

	// Write only the flags that changed — the same rule as the ingest path.
	for key, state := range states {
		current, err := kv.GetKey(ctx, tenantID, key)
		if err != nil && !errors.Is(err, store.ErrKeyNotFound) {
			return fmt.Errorf("read admission flag %s: %w", key, err)
		}
		if current == string(state) {
			continue
		}
		if err := kv.SetKey(ctx, tenantID, key, string(state)); err != nil {
			return fmt.Errorf("set admission flag %s: %w", key, err)
		}
	}
	return nil
}
