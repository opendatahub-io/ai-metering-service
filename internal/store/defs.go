package store

import (
	"context"
	"errors"
	"time"

	"github.com/opendatahub-io/ai-metering-service/internal/api"
)

// ErrKeyNotFound is returned by KeyStore.GetKey when the key does not exist.
// An absent admission flag is not an error condition — it means "no budget
// applies" — so callers must treat it distinctly from a failed read.
var ErrKeyNotFound = errors.New("key not found")

type Store interface {
	ListRates(ctx context.Context, tenantID string, pg Pagination) (*api.Listing[api.RateResource], error)
	GetRate(ctx context.Context, tenantID, id string) (*api.RateResource, error)
	CreateRate(ctx context.Context, tenantID string, rate *api.Rate) (*api.RateResource, error)
	UpdateRate(ctx context.Context, tenantID, id string, rate *api.Rate) (*api.RateResource, error)
	DeleteRate(ctx context.Context, tenantID, id string) error

	// Catalog rates — the global pricing catalog. Not part of the
	// accounting path: catalog entries are templates rate items are copied
	// from. ApplyCatalogRate inserts one entry, or updates the costs of
	// the entry whose resource already exists — a changed pricing file
	// must reprice the catalog (existing rate copies are snapshots and are
	// never touched). Returns nil, nil when the entry exists with
	// unchanged costs: nothing was written.
	ApplyCatalogRate(ctx context.Context, rate *api.CatalogRate) (*api.ResourceCatalogRate, error)
	ListCatalogRates(ctx context.Context, pg Pagination, provider, modelPrefix string) (*api.Listing[api.ResourceCatalogRate], error)
	GetCatalogRate(ctx context.Context, id string) (*api.ResourceCatalogRate, error)

	ListBudgets(ctx context.Context, tenantID string, pg Pagination) (*api.Listing[api.BudgetResource], error)
	GetBudget(ctx context.Context, tenantID, id string) (*api.BudgetResource, error)
	// ListAllBudgets lists budgets across all tenants. It exists for the
	// service-wide loops (renewal, reconcile), which are not scoped to a
	// tenant; management APIs must never use it.
	ListAllBudgets(ctx context.Context, pg Pagination) (*api.Listing[api.BudgetResource], error)
	CreateBudget(ctx context.Context, tenantID string, budget *api.Budget, callback func(res *api.BudgetResource) error) (*api.BudgetResource, error)
	UpdateBudget(ctx context.Context, tenantID, id string, budget *api.Budget, callback func(previousLimit int64, res *api.BudgetResource) error) (*api.BudgetResource, int64, error)
	DeleteBudget(ctx context.Context, tenantID, id string, callback func() error) error

	ListBudgetPolicies(ctx context.Context, tenantID string, pg Pagination) (*api.Listing[api.BudgetPolicyResource], error)
	GetBudgetPolicy(ctx context.Context, tenantID, id string) (*api.BudgetPolicyResource, error)
	CreateBudgetPolicy(ctx context.Context, tenantID string, policy *api.BudgetPolicy) (*api.BudgetPolicyResource, error)
	UpdateBudgetPolicy(ctx context.Context, tenantID, id string, policy *api.BudgetPolicy) (*api.BudgetPolicyResource, error)
	DeleteBudgetPolicy(ctx context.Context, tenantID, id string) error

	// FindPoliciesWithRates resolves every policy matching the identity,
	// joined with its rate's items — one round trip instead of a policy
	// query plus a rate query per policy. A policy whose rate no longer
	// resolves errors, mirroring the old per-policy GetRate failure.
	FindPoliciesWithRates(ctx context.Context, tenantID string, identity api.Identity) ([]api.PolicyWithRates, error)
	FindPoliciesForBudget(ctx context.Context, tenantID, budgetID string) ([]api.BudgetPolicyResource, error)

	// Renewal — period-boundary refills of budget balances.
	RenewDueBudgets(ctx context.Context, now time.Time, limit int, fn RenewalFunc) (int, error)
	// ListChargeTargets returns the distinct (identity, resource) pairs ever
	// charged against a budget — the inputs for recomputing the admission
	// flags a renewal affects.
	ListChargeTargets(ctx context.Context, tenantID, budgetID string) ([]api.ChargeTarget, error)
	// SumBudgetCharges returns the BU charged to a budget for usage events
	// whose timestamp falls in the half-open window [from, to). This is the
	// ledger-derived spend that renewal, reconcile, and rebuild all derive
	// balances from.
	SumBudgetCharges(ctx context.Context, tenantID, budgetID string, from, to time.Time) (int64, error)

	// SettleUsage persists the usage event and its per-budget charges in
	// one round trip: the event row is upserted (a redelivery reuses the
	// existing row's id), and each charge is inserted with ON CONFLICT DO
	// NOTHING against the (usage_id, budget_ref) idempotency marker. It
	// returns the budget refs whose charges were newly inserted — the
	// caller decrements balances and recomputes flags for exactly those,
	// skipping duplicates from redelivery or overlapping policies. Charges
	// sharing a budget within one call settle atomically, in argument
	// order: the first charge for a budget wins, same as the per-charge
	// loop it replaces.
	SettleUsage(ctx context.Context, record *api.UsageRecord, charges []api.UsageCharge) ([]string, error)
	QueryUsage(ctx context.Context, tenantID string, query *api.UsageQuery) (*api.UsageResult, error)
	DeleteAllUsage(ctx context.Context, tenantID string) (int64, error)
}

type KeyStore interface {
	SetKey(ctx context.Context, tenantID, key string, value string) error
	GetKey(ctx context.Context, tenantID, key string) (string, error)
	DeleteKey(ctx context.Context, tenantID, key string) error
	DecrKey(ctx context.Context, tenantID, key string, value int64) (int64, error)
}

type Pagination struct {
	Limit  int
	Offset int
}

// BudgetRenewal carries one due budget through its renewal: the budget, the
// completed period whose spend feeds the new balance, and that spend summed
// from the usage ledger.
type BudgetRenewal struct {
	Budget api.BudgetResource
	// PrevPeriodStart and PrevPeriodEnd bound the completed period,
	// half-open [PrevPeriodStart, PrevPeriodEnd). When several boundaries
	// were missed (pod outage, idle budget), this is the most recently
	// completed period, not each missed one replayed.
	PrevPeriodStart time.Time
	PrevPeriodEnd   time.Time
	// PrevSpent is the BU charged to this budget within the completed
	// period, from the usage ledger. It feeds the new period's opening
	// balance (rollover carry, overage deduction).
	PrevSpent int64
	// CurrentSpent is the BU already charged within the new period before
	// the renewal processes the boundary (ingest does not wait for the
	// renewal loop). The refill must subtract it: a plain SET to the
	// opening balance would erase those charges from the counter.
	CurrentSpent int64
}

// RenewalFunc applies one budget's renewal (Redis balance refill and
// admission flag recompute). It runs inside the renewal's database
// transaction, after PrevSpent has been computed and before the budget's
// next_renewal_at is advanced: its Redis writes precede the commit, so a
// crash after the callback but before the commit leaves the claim
// un-advanced and the renewal is retried — deterministically, since the new
// balance is derived from the closed period's ledger — rather than skipped
// with a stale balance behind an advanced claim.
type RenewalFunc func(ctx context.Context, renewal *BudgetRenewal) error
