package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redhat-et/pricetag-metering/internal/api"
	"github.com/redhat-et/pricetag-metering/internal/period"
	"github.com/redhat-et/pricetag-metering/internal/store"
)

type PGStore struct {
	db *sql.DB
}

func New(db *sql.DB) *PGStore {
	return &PGStore{db: db}
}

// Rates

func (s *PGStore) ListRates(ctx context.Context, tenantID string, pg store.Pagination) (*api.Listing[api.RateResource], error) {
	limit, offset := pgDefaults(pg)

	var total int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM rates WHERE tenant_id = $1`, tenantID).Scan(&total); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, name, items, created_at, updated_at
		 FROM rates WHERE tenant_id = $1 ORDER BY name LIMIT $2 OFFSET $3`,
		tenantID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []api.RateResource
	for rows.Next() {
		r, err := scanRateResource(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &api.Listing[api.RateResource]{
		Limit: int64(limit), Offset: int64(offset), Total: total, Items: items,
	}, nil
}

func (s *PGStore) GetRate(ctx context.Context, tenantID, id string) (*api.RateResource, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, items, created_at, updated_at
		 FROM rates WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	return scanRateResource(row)
}

func (s *PGStore) CreateRate(ctx context.Context, tenantID string, rate *api.Rate) (*api.RateResource, error) {
	items, err := json.Marshal(rate.Items)
	if err != nil {
		return nil, err
	}
	res := &api.RateResource{Spec: *rate}
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO rates (tenant_id, name, items)
		 VALUES ($1, $2, $3)
		 RETURNING id, created_at, updated_at`,
		tenantID, rate.Name, items,
	).Scan(&res.Metadata.ID, &res.Metadata.CreatedAt, &res.Metadata.UpdatedAt)
	if err != nil {
		return nil, err
	}
	res.Metadata.TenantID = tenantID
	return res, nil
}

func (s *PGStore) UpdateRate(ctx context.Context, tenantID, id string, rate *api.Rate) (*api.RateResource, error) {
	items, err := json.Marshal(rate.Items)
	if err != nil {
		return nil, err
	}
	res := &api.RateResource{Spec: *rate}
	err = s.db.QueryRowContext(ctx,
		`UPDATE rates SET name = $1, items = $2, updated_at = now()
		 WHERE tenant_id = $3 AND id = $4
		 RETURNING id, created_at, updated_at`,
		rate.Name, items, tenantID, id,
	).Scan(&res.Metadata.ID, &res.Metadata.CreatedAt, &res.Metadata.UpdatedAt)
	if err != nil {
		return nil, err
	}
	res.Metadata.TenantID = tenantID
	return res, nil
}

func (s *PGStore) DeleteRate(ctx context.Context, tenantID, id string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM rates WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	return err
}

// Budgets

func (s *PGStore) ListBudgets(ctx context.Context, tenantID string, pg store.Pagination) (*api.Listing[api.BudgetResource], error) {
	limit, offset := pgDefaults(pg)

	var total int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM budgets WHERE tenant_id = $1`, tenantID).Scan(&total); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, name, "limit", period_start, period_duration,
		        renewal_type, max_carry, next_renewal_at, created_at, updated_at
		 FROM budgets WHERE tenant_id = $1 ORDER BY name LIMIT $2 OFFSET $3`,
		tenantID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []api.BudgetResource
	for rows.Next() {
		b, err := scanBudgetResource(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &api.Listing[api.BudgetResource]{
		Limit: int64(limit), Offset: int64(offset), Total: total, Items: items,
	}, nil
}

func (s *PGStore) GetBudget(ctx context.Context, tenantID, id string) (*api.BudgetResource, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, "limit", period_start, period_duration,
		        renewal_type, max_carry, next_renewal_at, created_at, updated_at
		 FROM budgets WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	return scanBudgetResource(row)
}

// ListAllBudgets lists budgets across all tenants for the service-wide
// loops (renewal, reconcile). Management APIs must use ListBudgets so
// results stay tenant-scoped.
func (s *PGStore) ListAllBudgets(ctx context.Context, pg store.Pagination) (*api.Listing[api.BudgetResource], error) {
	limit, offset := pgDefaults(pg)

	var total int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM budgets`).Scan(&total); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, name, "limit", period_start, period_duration,
		        renewal_type, max_carry, next_renewal_at, created_at, updated_at
		 FROM budgets ORDER BY next_renewal_at LIMIT $1 OFFSET $2`,
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []api.BudgetResource
	for rows.Next() {
		b, err := scanBudgetResource(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &api.Listing[api.BudgetResource]{
		Limit: int64(limit), Offset: int64(offset), Total: total, Items: items,
	}, nil
}

func (s *PGStore) CreateBudget(ctx context.Context,
	tenantID string,
	budget *api.Budget,
	callback func(res *api.BudgetResource) error) (*api.BudgetResource, error) {

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	res := &api.BudgetResource{Spec: *budget}
	nextRenewalAt, err := nextRenewalAt(budget.PeriodStart, budget.PeriodDuration)
	if err != nil {
		return nil, err
	}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO budgets (tenant_id, name, "limit", period_start, period_duration, renewal_type, max_carry, next_renewal_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, created_at, updated_at`,
		tenantID, budget.Name, budget.Limit, budget.PeriodStart,
		budget.PeriodDuration, budget.RenewalType, budget.MaxCarry, nextRenewalAt,
	).Scan(&res.Metadata.ID, &res.Metadata.CreatedAt, &res.Metadata.UpdatedAt)
	if err != nil {
		return nil, err
	}
	res.Metadata.TenantID = tenantID

	if err := callback(res); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *PGStore) UpdateBudget(ctx context.Context, tenantID,
	id string,
	budget *api.Budget,
	callback func(previousLimit int64, res *api.BudgetResource) error) (*api.BudgetResource, int64, error) {

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()

	res := &api.BudgetResource{Spec: *budget}
	var previousLimit int64
	nextRenewalAt, err := nextRenewalAt(budget.PeriodStart, budget.PeriodDuration)
	if err != nil {
		return nil, 0, err
	}
	err = tx.QueryRowContext(ctx,
		`WITH old AS (
			SELECT "limit" FROM budgets WHERE tenant_id = $8 AND id = $9
		)
		UPDATE budgets SET name = $1, "limit" = $2, period_start = $3, period_duration = $4,
		        renewal_type = $5, max_carry = $6, next_renewal_at = $7, updated_at = now()
		 WHERE tenant_id = $8 AND id = $9
		 RETURNING id, created_at, updated_at, (SELECT "limit" FROM old)`,
		budget.Name, budget.Limit, budget.PeriodStart, budget.PeriodDuration,
		budget.RenewalType, budget.MaxCarry, nextRenewalAt, tenantID, id,
	).Scan(&res.Metadata.ID, &res.Metadata.CreatedAt, &res.Metadata.UpdatedAt, &previousLimit)
	if err != nil {
		return nil, 0, err
	}
	res.Metadata.TenantID = tenantID

	if err := callback(previousLimit, res); err != nil {
		return nil, 0, err
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return res, previousLimit, nil
}

func (s *PGStore) DeleteBudget(ctx context.Context, tenantID, id string, callback func() error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`DELETE FROM budgets WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return err
	}

	if err := callback(); err != nil {
		return err
	}

	return tx.Commit()
}

// Budget Policies

func (s *PGStore) ListBudgetPolicies(ctx context.Context, tenantID string, pg store.Pagination) (*api.Listing[api.BudgetPolicyResource], error) {
	limit, offset := pgDefaults(pg)

	var total int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM budget_policies WHERE tenant_id = $1`, tenantID).Scan(&total); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, budget_ref, rate_ref, subjects, overage,
		        deduct_overage, created_at, updated_at
		 FROM budget_policies WHERE tenant_id = $1 LIMIT $2 OFFSET $3`,
		tenantID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []api.BudgetPolicyResource
	for rows.Next() {
		p, err := scanBudgetPolicyResource(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &api.Listing[api.BudgetPolicyResource]{
		Limit: int64(limit), Offset: int64(offset), Total: total, Items: items,
	}, nil
}

func (s *PGStore) GetBudgetPolicy(ctx context.Context, tenantID, id string) (*api.BudgetPolicyResource, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, budget_ref, rate_ref, subjects, overage,
		        deduct_overage, created_at, updated_at
		 FROM budget_policies WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	return scanBudgetPolicyResource(row)
}

func (s *PGStore) CreateBudgetPolicy(ctx context.Context, tenantID string, policy *api.BudgetPolicy) (*api.BudgetPolicyResource, error) {
	subjects, err := json.Marshal(policy.Subjects)
	if err != nil {
		return nil, err
	}
	var overage []byte
	if policy.Overage != nil {
		overage, err = json.Marshal(policy.Overage)
		if err != nil {
			return nil, err
		}
	}
	res := &api.BudgetPolicyResource{Spec: *policy}
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO budget_policies (tenant_id, budget_ref, rate_ref, subjects, overage, deduct_overage)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at, updated_at`,
		tenantID, policy.BudgetRef, policy.RateRef, subjects, overage, policy.DeductOverage,
	).Scan(&res.Metadata.ID, &res.Metadata.CreatedAt, &res.Metadata.UpdatedAt)
	if err != nil {
		return nil, err
	}
	res.Metadata.TenantID = tenantID
	return res, nil
}

func (s *PGStore) UpdateBudgetPolicy(ctx context.Context, tenantID, id string, policy *api.BudgetPolicy) (*api.BudgetPolicyResource, error) {
	subjects, err := json.Marshal(policy.Subjects)
	if err != nil {
		return nil, err
	}
	var overage []byte
	if policy.Overage != nil {
		overage, err = json.Marshal(policy.Overage)
		if err != nil {
			return nil, err
		}
	}
	res := &api.BudgetPolicyResource{Spec: *policy}
	err = s.db.QueryRowContext(ctx,
		`UPDATE budget_policies SET budget_ref = $1, rate_ref = $2, subjects = $3,
		        overage = $4, deduct_overage = $5, updated_at = now()
		 WHERE tenant_id = $6 AND id = $7
		 RETURNING id, created_at, updated_at`,
		policy.BudgetRef, policy.RateRef, subjects, overage,
		policy.DeductOverage, tenantID, id,
	).Scan(&res.Metadata.ID, &res.Metadata.CreatedAt, &res.Metadata.UpdatedAt)
	if err != nil {
		return nil, err
	}
	res.Metadata.TenantID = tenantID
	return res, nil
}

func (s *PGStore) DeleteBudgetPolicy(ctx context.Context, tenantID, id string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM budget_policies WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	return err
}

// Policy resolution

func (s *PGStore) FindPoliciesForIdentity(ctx context.Context, tenantID string, identity api.Identity) ([]api.BudgetPolicyResource, error) {
	// Build JSONB containment conditions for each identity dimension.
	// subjects @> '[{"kind":"user","name":"alice"}]'::jsonb
	conditions := []string{}
	args := []any{tenantID}
	argN := 2

	if identity.User != "" {
		subj, _ := json.Marshal([]api.Subject{{Kind: "user", Name: identity.User}})
		conditions = append(conditions, fmt.Sprintf("subjects @> $%d::jsonb", argN))
		args = append(args, string(subj))
		argN++
	}
	for _, g := range identity.Groups {
		subj, _ := json.Marshal([]api.Subject{{Kind: "group", Name: g}})
		conditions = append(conditions, fmt.Sprintf("subjects @> $%d::jsonb", argN))
		args = append(args, string(subj))
		argN++
	}
	if identity.Organization != "" {
		subj, _ := json.Marshal([]api.Subject{{Kind: "organization", Name: identity.Organization}})
		conditions = append(conditions, fmt.Sprintf("subjects @> $%d::jsonb", argN))
		args = append(args, string(subj))
		argN++
	}
	if identity.Tenant != "" {
		subj, _ := json.Marshal([]api.Subject{{Kind: "tenant", Name: identity.Tenant}})
		conditions = append(conditions, fmt.Sprintf("subjects @> $%d::jsonb", argN))
		args = append(args, string(subj))
		argN++
	}

	if len(conditions) == 0 {
		return nil, nil
	}

	q := `SELECT id, tenant_id, budget_ref, rate_ref, subjects, overage,
	             deduct_overage, created_at, updated_at
	      FROM budget_policies
	      WHERE tenant_id = $1 AND (` + strings.Join(conditions, " OR ") + `)`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var policies []api.BudgetPolicyResource
	for rows.Next() {
		p, err := scanBudgetPolicyResource(rows)
		if err != nil {
			return nil, err
		}
		policies = append(policies, *p)
	}
	return policies, rows.Err()
}

func (s *PGStore) FindPoliciesForBudget(ctx context.Context, tenantID, budgetID string) ([]api.BudgetPolicyResource, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, budget_ref, rate_ref, subjects, overage,
		        deduct_overage, created_at, updated_at
		 FROM budget_policies WHERE tenant_id = $1 AND budget_ref = $2`,
		tenantID, budgetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var policies []api.BudgetPolicyResource
	for rows.Next() {
		p, err := scanBudgetPolicyResource(rows)
		if err != nil {
			return nil, err
		}
		policies = append(policies, *p)
	}
	return policies, rows.Err()
}

// Renewal

// nextRenewalAt computes the first period boundary for a budget: one
// duration past the anchor. When the anchor is in the past the boundary may
// already be behind `now`; that is fine — the renewal loop treats a due
// budget as catch-up and advances it to the current period in one step.
func nextRenewalAt(periodStart time.Time, periodDuration string) (time.Time, error) {
	d, err := period.Parse(periodDuration)
	if err != nil {
		return time.Time{}, err
	}
	return d.AddTo(periodStart), nil
}

// RenewDueBudgets renews up to `limit` budgets whose next_renewal_at has
// passed, each in its own transaction. The candidate list is unlocked; the
// per-budget claim is the row lock taken by SELECT ... FOR UPDATE SKIP
// LOCKED, so pods running the sweep concurrently pick disjoint budgets and a
// budget locked by a slow pod is skipped (and picked up by a later sweep)
// rather than blocking. The renewal callback's writes precede the claim's
// commit — see store.RenewalFunc for the crash semantics.
func (s *PGStore) RenewDueBudgets(ctx context.Context, now time.Time, limit int, fn store.RenewalFunc) (int, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM budgets WHERE next_renewal_at <= $1 ORDER BY next_renewal_at LIMIT $2`,
		now, limit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	renewed := 0
	for _, id := range ids {
		n, err := s.renewBudget(ctx, now, id, fn)
		if err != nil {
			return renewed, err
		}
		renewed += n
	}
	return renewed, nil
}

// renewBudget claims and renews one due budget. It returns 0 (not an
// error) when another pod holds the row or renewed it in the meantime.
func (s *PGStore) renewBudget(ctx context.Context, now time.Time, id string, fn store.RenewalFunc) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, "limit", period_start, period_duration,
		        renewal_type, max_carry, next_renewal_at, created_at, updated_at
		 FROM budgets WHERE id = $1 FOR UPDATE SKIP LOCKED`, id)
	budget, err := scanBudgetResource(row)
	if errors.Is(err, sql.ErrNoRows) {
		// Locked by another pod mid-renewal: leave it to that pod.
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if budget.Spec.NextRenewalAt.After(now) {
		// Renewed (or re-anchored by an update) between the candidate list
		// and the lock.
		return 0, nil
	}

	d, err := period.Parse(budget.Spec.PeriodDuration)
	if err != nil {
		return 0, fmt.Errorf("budget %s: %w", budget.Metadata.ID, err)
	}
	window, err := period.WindowAt(budget.Spec.PeriodStart, d, now)
	if err != nil {
		return 0, fmt.Errorf("budget %s: %w", budget.Metadata.ID, err)
	}

	// The completed period's spend, from the ledger. Computed inside the
	// claim's transaction but from data a concurrent ingest may still be
	// adding to: a charge racing the renewal can be lost from this sum (or
	// lost from the balance by the refill SET). The ledger stays
	// authoritative and reconcile rebuilds the balance from it — the same
	// class of accepted drift as out-of-order flag writes.
	prevSpent, err := sumBudgetCharges(ctx, tx, budget.Metadata.TenantID, budget.Metadata.ID,
		window.PrevStart, window.CurrentStart)
	if err != nil {
		return 0, err
	}

	// Charges already accrued in the new period before this renewal
	// processed the boundary — ingest does not wait for the renewal loop.
	// The refill must subtract them, or the SET erases them from the
	// counter. A charge committed here whose DECRBY lands after the refill
	// is subtracted twice — the mirror of the accepted drift above, in the
	// fail-closed direction, corrected by the next reconcile.
	currentSpent, err := sumBudgetCharges(ctx, tx, budget.Metadata.TenantID, budget.Metadata.ID,
		window.CurrentStart, now)
	if err != nil {
		return 0, err
	}

	if err := fn(ctx, &store.BudgetRenewal{
		Budget:          *budget,
		PrevPeriodStart: window.PrevStart,
		PrevPeriodEnd:   window.CurrentStart,
		PrevSpent:       prevSpent,
		CurrentSpent:    currentSpent,
	}); err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE budgets SET next_renewal_at = $2, updated_at = now() WHERE id = $1`,
		budget.Metadata.ID, window.NextEnd); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return 1, nil
}

// ListChargeTargets returns the distinct (identity, resource) pairs ever
// charged against a budget. The set is deliberately unbounded in time: a
// user whose last event left the flag at deny can never produce another
// event (admission rejects the request), so only a renewal — not the next
// ingest — can lift that flag; bounding the query would strand those
// identities. At single-cluster scale the distinct pairs per budget are
// bounded by the tenant's active users times resources.
func (s *PGStore) ListChargeTargets(ctx context.Context, tenantID, budgetID string) ([]api.ChargeTarget, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT u.identity, u.resource
		 FROM usage u JOIN usage_charges c ON c.usage_id = u.id
		 WHERE u.tenant_id = $1 AND c.budget_ref = $2`,
		tenantID, budgetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var targets []api.ChargeTarget
	for rows.Next() {
		var identityJSON, resourceJSON []byte
		if err := rows.Scan(&identityJSON, &resourceJSON); err != nil {
			return nil, err
		}
		var target api.ChargeTarget
		if err := json.Unmarshal(identityJSON, &target.Identity); err != nil {
			return nil, fmt.Errorf("parse identity for budget %s: %w", budgetID, err)
		}
		if err := json.Unmarshal(resourceJSON, &target.Resource); err != nil {
			return nil, fmt.Errorf("parse resource for budget %s: %w", budgetID, err)
		}
		// The ledger's identity carries user, groups, and organization; the
		// tenant is the record's scope, not part of the stored JSON, but
		// policy matching needs it (tenant-subject policies).
		target.Identity.Tenant = tenantID
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

// Usage

// InsertUsage persists the event-level usage row and returns its id. For an
// event already present (redelivery), it returns the existing row's id so
// the caller can dedup each charge individually.
func (s *PGStore) InsertUsage(ctx context.Context, record *api.UsageRecord) (int64, error) {
	identity, err := json.Marshal(record.Identity)
	if err != nil {
		return 0, err
	}
	resource, err := json.Marshal(record.Resource)
	if err != nil {
		return 0, err
	}
	usage, err := json.Marshal(record.Usage)
	if err != nil {
		return 0, err
	}

	var id int64
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO usage (tenant_id, event_id, request_id, "timestamp", identity, resource, usage)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (tenant_id, event_id) DO NOTHING
		 RETURNING id`,
		record.TenantID, record.EventID, record.RequestID, record.Timestamp,
		identity, resource, usage).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// Event already present — return its id for per-charge dedup.
		err = s.db.QueryRowContext(ctx,
			`SELECT id FROM usage WHERE tenant_id = $1 AND event_id = $2`,
			record.TenantID, record.EventID).Scan(&id)
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// InsertUsageCharge persists one budget's charge for a usage event. It
// returns false when the charge already exists, so a redelivered event
// never decrements the same budget twice.
func (s *PGStore) InsertUsageCharge(ctx context.Context, usageID int64, charge *api.UsageCharge) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_charges (usage_id, budget_ref, bu_cost)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (usage_id, budget_ref) DO NOTHING`,
		usageID, charge.BudgetRef, charge.BUCost)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *PGStore) QueryUsage(ctx context.Context, tenantID string, query *api.UsageQuery) (*api.UsageResult, error) {
	from, to, err := resolveTimeRange(query)
	if err != nil {
		return nil, err
	}

	granularity := query.Granularity
	if granularity == "" {
		granularity = "total"
	}

	var bucketExpr string
	if granularity != "total" {
		bucketExpr = fmt.Sprintf(`date_trunc('%s', "timestamp")`, granularity)
	}

	// usage is 1:1 with the event; its budgets' charges live in
	// usage_charges. The inner query produces one row per event with the
	// event's total cost across budgets, so requests count events and usage
	// maps are summed once per event; bu_cost stays additive in the outer
	// aggregation.
	var innerSelect []string
	if bucketExpr != "" {
		innerSelect = append(innerSelect, bucketExpr+" AS bucket")
	}
	for _, dim := range query.GroupBy {
		innerSelect = append(innerSelect, dimensionExpr(dim)+" AS \""+dim+"\"")
	}
	innerSelect = append(innerSelect,
		"u.usage",
		"COALESCE(c.cost, 0) AS bu_cost",
	)

	var outerSelect []string
	var outerGroup []string
	if bucketExpr != "" {
		outerSelect = append(outerSelect, "bucket")
		outerGroup = append(outerGroup, "bucket")
	}
	for _, dim := range query.GroupBy {
		quoted := `"` + dim + `"`
		outerSelect = append(outerSelect, quoted)
		outerGroup = append(outerGroup, quoted)
	}
	outerSelect = append(outerSelect,
		"count(*) AS requests",
		"jsonb_sum_agg(usage) AS usage_agg",
		"sum(bu_cost) AS bu_cost",
	)

	where := []string{"tenant_id = $1", `"timestamp" >= $2`, `"timestamp" <= $3`}
	args := []any{tenantID, from, to}
	argN := 4

	if query.User != "" {
		where = append(where, fmt.Sprintf("identity->>'user' = $%d", argN))
		args = append(args, query.User)
		argN++
	}
	if query.Group != "" {
		where = append(where, fmt.Sprintf("identity->'groups' ? $%d", argN))
		args = append(args, query.Group)
		argN++
	}
	if query.Org != "" {
		where = append(where, fmt.Sprintf("identity->>'organization' = $%d", argN))
		args = append(args, query.Org)
		argN++
	}
	if query.Model != "" {
		where = append(where, fmt.Sprintf("resource->>'model' = $%d", argN))
		args = append(args, query.Model)
		argN++
	}
	if query.Provider != "" {
		where = append(where, fmt.Sprintf("resource->>'provider' = $%d", argN))
		args = append(args, query.Provider)
		argN++
	}

	q := "SELECT " + strings.Join(outerSelect, ", ") +
		" FROM (SELECT " + strings.Join(innerSelect, ", ") +
		" FROM usage u" +
		" LEFT JOIN (SELECT usage_id, sum(bu_cost) AS cost FROM usage_charges GROUP BY usage_id) c" +
		" ON c.usage_id = u.id" +
		" WHERE " + strings.Join(where, " AND ") +
		") per_event"
	if len(outerGroup) > 0 {
		q += " GROUP BY " + strings.Join(outerGroup, ", ")
		q += " ORDER BY " + strings.Join(outerGroup, ", ")
	}

	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	q += fmt.Sprintf(" LIMIT %d", limit)
	if query.Offset > 0 {
		q += fmt.Sprintf(" OFFSET %d", query.Offset)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var data []api.UsageBucket
	for rows.Next() {
		var b api.UsageBucket
		var usageJSON []byte

		scanArgs := make([]any, 0)
		if granularity != "total" {
			var bucket time.Time
			scanArgs = append(scanArgs, &bucket)
		}
		for range query.GroupBy {
			var dim string
			scanArgs = append(scanArgs, &dim)
		}
		scanArgs = append(scanArgs, &b.Requests, &usageJSON, &b.BUCost)

		if err := rows.Scan(scanArgs...); err != nil {
			return nil, err
		}

		if granularity != "total" {
			b.Bucket = scanArgs[0].(*time.Time).Format(time.RFC3339)
		}
		b.Dimensions = make(map[string]string)
		dimOffset := 0
		if granularity != "total" {
			dimOffset = 1
		}
		for i, dim := range query.GroupBy {
			b.Dimensions[dim] = *scanArgs[dimOffset+i].(*string)
		}
		if err := json.Unmarshal(usageJSON, &b.Usage); err != nil {
			return nil, err
		}
		data = append(data, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &api.UsageResult{
		Range:       api.UsageRange{From: from.Format("2006-01-02"), To: to.Format("2006-01-02")},
		Granularity: granularity,
		GroupBy:     query.GroupBy,
		Data:        data,
	}, nil
}

func (s *PGStore) DeleteAllUsage(ctx context.Context, tenantID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM usage WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// helpers

type scanner interface {
	Scan(dest ...any) error
}

// querier is the common subset of *sql.DB and *sql.Tx the ledger-sum
// helper needs, so the renewal claim can run it inside its transaction
// while reconcile runs it on the pool.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// sumBudgetCharges sums a budget's ledger charges for usage events whose
// timestamp falls in the half-open window [from, to).
func sumBudgetCharges(ctx context.Context, q querier, tenantID, budgetID string, from, to time.Time) (int64, error) {
	var sum int64
	err := q.QueryRowContext(ctx,
		`SELECT COALESCE(sum(c.bu_cost), 0)
		 FROM usage_charges c JOIN usage u ON u.id = c.usage_id
		 WHERE u.tenant_id = $1 AND c.budget_ref = $2
		   AND u."timestamp" >= $3 AND u."timestamp" < $4`,
		tenantID, budgetID, from, to).Scan(&sum)
	return sum, err
}

// SumBudgetCharges implements store.Store.
func (s *PGStore) SumBudgetCharges(ctx context.Context, tenantID, budgetID string, from, to time.Time) (int64, error) {
	return sumBudgetCharges(ctx, s.db, tenantID, budgetID, from, to)
}

func scanRateResource(s scanner) (*api.RateResource, error) {
	var r api.RateResource
	var itemsJSON []byte
	if err := s.Scan(&r.Metadata.ID, &r.Metadata.TenantID, &r.Spec.Name,
		&itemsJSON, &r.Metadata.CreatedAt, &r.Metadata.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(itemsJSON, &r.Spec.Items); err != nil {
		return nil, err
	}
	return &r, nil
}

func scanBudgetResource(s scanner) (*api.BudgetResource, error) {
	var b api.BudgetResource
	if err := s.Scan(&b.Metadata.ID, &b.Metadata.TenantID, &b.Spec.Name,
		&b.Spec.Limit, &b.Spec.PeriodStart, &b.Spec.PeriodDuration,
		&b.Spec.RenewalType, &b.Spec.MaxCarry, &b.Spec.NextRenewalAt,
		&b.Metadata.CreatedAt, &b.Metadata.UpdatedAt); err != nil {
		return nil, err
	}
	return &b, nil
}

func scanBudgetPolicyResource(s scanner) (*api.BudgetPolicyResource, error) {
	var p api.BudgetPolicyResource
	var subjectsJSON, overageJSON []byte
	if err := s.Scan(&p.Metadata.ID, &p.Metadata.TenantID,
		&p.Spec.BudgetRef, &p.Spec.RateRef,
		&subjectsJSON, &overageJSON, &p.Spec.DeductOverage,
		&p.Metadata.CreatedAt, &p.Metadata.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(subjectsJSON, &p.Spec.Subjects); err != nil {
		return nil, err
	}
	if len(overageJSON) > 0 {
		p.Spec.Overage = &api.OverageConfig{}
		if err := json.Unmarshal(overageJSON, p.Spec.Overage); err != nil {
			return nil, err
		}
	}
	return &p, nil
}

func dimensionExpr(dim string) string {
	switch dim {
	case "user":
		return "identity->>'user'"
	case "group":
		return "identity->>'groups'"
	case "org":
		return "identity->>'organization'"
	case "model":
		return "resource->>'model'"
	case "provider":
		return "resource->>'provider'"
	case "region":
		return "resource->>'region'"
	default:
		return "resource->>'" + dim + "'"
	}
}

func pgDefaults(pg store.Pagination) (int, int) {
	limit := pg.Limit
	if limit <= 0 {
		limit = 100
	}
	offset := pg.Offset
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func resolveTimeRange(query *api.UsageQuery) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	to := now

	switch query.Range {
	case api.RangeCustom:
		from, err := time.Parse("2006-01-02", query.From)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid from date: %w", err)
		}
		if query.To != "" {
			t, err := time.Parse("2006-01-02", query.To)
			if err != nil {
				return time.Time{}, time.Time{}, fmt.Errorf("invalid to date: %w", err)
			}
			to = t.Add(24*time.Hour - time.Nanosecond)
		}
		return from, to, nil
	case api.Range24h:
		return now.Add(-24 * time.Hour), to, nil
	case api.Range7d, "":
		return now.AddDate(0, 0, -7), to, nil
	case api.Range30d:
		return now.AddDate(0, 0, -30), to, nil
	case api.Range90d:
		return now.AddDate(0, 0, -90), to, nil
	case api.Range180d:
		return now.AddDate(0, 0, -180), to, nil
	case api.Range365d:
		return now.AddDate(0, 0, -365), to, nil
	case api.RangeMTD:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), to, nil
	case api.RangeQTD:
		qMonth := ((now.Month()-1)/3)*3 + 1
		return time.Date(now.Year(), qMonth, 1, 0, 0, 0, 0, time.UTC), to, nil
	case api.RangeYTD:
		return time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC), to, nil
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("unknown range: %s", query.Range)
	}
}
