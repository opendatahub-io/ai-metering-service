// Package renewal refills budget balances at period boundaries, recomputes
// the admission flags the refill affects, and reconciles Redis state
// against the durable ledger.
//
// Trigger: every Metering Service pod runs a sweep loop. This is safe with
// multiple pods and no leader election because the claim of a due budget is
// a row lock in Postgres (SELECT ... FOR UPDATE SKIP LOCKED) — pods pick
// disjoint budgets and extra pods become extra workers. A separate CronJob
// was considered and rejected: it buys no timeliness (its minimum
// granularity is the same minute-level cadence as this loop) at the cost of
// a second deployment artifact that must stay schema-compatible with the
// service.
//
// Timeliness: period boundaries are P7D/P1M-scale, so a renewal landing a
// sweep-interval after the boundary is fine, and it fails closed — briefly
// denying traffic whose budget should now allow it — the safe direction,
// within the drift the ADR's overshoot model already accepts.
//
// Why not renew lazily, on the next usage event: a hard-denied budget
// rejects requests at admission, so no events arrive for it, so no
// accounting pass would ever renew it. A proactive sweep is the only
// trigger that cannot deadlock.
//
// Reconcile is the repair path: balances and flags in Redis are derived
// state, rebuildable at any time from the usage ledger in Postgres. It runs
// at startup (an empty Redis and a tenant with no budgets are
// indistinguishable to a reader — absent keys read as "no budget applies",
// i.e. unlimited), periodically as a self-heal bounding ledger/counter
// drift, and lazily when a flag recompute finds a balance key missing.
package balance

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/opendatahub-io/ai-metering-service/internal/admission"
	"github.com/opendatahub-io/ai-metering-service/internal/api"
	"github.com/opendatahub-io/ai-metering-service/internal/config"
	"github.com/opendatahub-io/ai-metering-service/internal/period"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
)

const (
	defaultInterval          = 30 * time.Second
	defaultBatchSize         = 100
	defaultReconcileInterval = 10 * time.Minute
)

type Renewer struct {
	store             store.Store
	kv                store.KeyStore
	interval          time.Duration
	reconcileInterval time.Duration
	batchSize         int

	// lastSweep and lastReconcile track the loops' liveness for /health:
	// a stale timestamp (older than a couple of intervals) means the loop
	// is stuck — flags frozen at their last values. In-memory and per-pod
	// by design: they answer "is this pod's loop alive", not "when did any
	// budget last renew" (that is what budgets.next_renewal_at records).
	statusMu      sync.RWMutex
	lastSweep     SweepStatus
	lastReconcile ReconcileStatus
}

// SweepStatus is the outcome of the most recent sweep pass.
type SweepStatus struct {
	At      time.Time `json:"last_sweep_at"`
	Renewed int       `json:"renewed"`
	Err     string    `json:"error,omitempty"`
}

// ReconcileStatus is the outcome of the most recent reconcile pass.
type ReconcileStatus struct {
	At         time.Time `json:"last_reconcile_at"`
	Reconciled int       `json:"reconciled"`
	Err        string    `json:"error,omitempty"`
}

// New builds a Renewer from the service configuration. Zero values in the
// renewal config fall back to the defaults: a 30s sweep interval, a 10m
// reconcile interval, and 100 budgets per pass.
func New(s store.Store, kv store.KeyStore, cfg config.RenewalConfig) *Renewer {
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	reconcileInterval := cfg.ReconcileInterval
	if reconcileInterval <= 0 {
		reconcileInterval = defaultReconcileInterval
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	return &Renewer{
		store:             s,
		kv:                kv,
		interval:          interval,
		reconcileInterval: reconcileInterval,
		batchSize:         batchSize,
	}
}

// Run sweeps and reconciles on tickers until ctx is cancelled. Both run
// once immediately at startup: the sweep so a pod that comes up late — or
// a service that was down over a boundary — catches up missed renewals,
// and the reconcile because an empty Redis and a tenant with no budgets
// are indistinguishable to a reader (absent keys read as "no budget
// applies"), so a flushed Redis must be republished before it is quietly
// read as unlimited. Failures are logged and retried on the next tick;
// they never stop the loops.
func (r *Renewer) Run(ctx context.Context) error {
	r.reconcileLogged(ctx)
	r.sweepLogged(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	reconcileTicker := time.NewTicker(r.reconcileInterval)
	defer reconcileTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.sweepLogged(ctx)
		case <-reconcileTicker.C:
			r.reconcileLogged(ctx)
		}
	}
}

// sweep runs one renewal pass and records its outcome for the liveness
// signal exposed by Status.
func (r *Renewer) sweep(ctx context.Context) (int, error) {
	n, err := r.store.RenewDueBudgets(ctx, time.Now().UTC(), r.batchSize, r.renew)

	r.statusMu.Lock()
	r.lastSweep = SweepStatus{At: time.Now().UTC(), Renewed: n}
	if err != nil {
		r.lastSweep.Err = err.Error()
	}
	r.statusMu.Unlock()

	return n, err
}

// Status reports the outcome of the most recent sweep pass. A zero
// LastSweepAt means no sweep has run yet in this pod.
func (r *Renewer) Status() SweepStatus {
	r.statusMu.RLock()
	defer r.statusMu.RUnlock()
	return r.lastSweep
}

// LastReconcile reports the outcome of the most recent reconcile pass. A
// zero At means no reconcile has run yet in this pod.
func (r *Renewer) LastReconcile() ReconcileStatus {
	r.statusMu.RLock()
	defer r.statusMu.RUnlock()
	return r.lastReconcile
}

// Interval reports the configured sweep interval, so a watcher of the
// liveness signal knows how stale last_sweep_at may legitimately be.
func (r *Renewer) Interval() time.Duration {
	return r.interval
}

// ReconcileInterval reports the configured reconcile interval.
func (r *Renewer) ReconcileInterval() time.Duration {
	return r.reconcileInterval
}

func (r *Renewer) sweepLogged(ctx context.Context) {
	log.Printf("renewal sweep: starting at %s", time.Now().UTC())
	n, err := r.sweep(ctx)
	if err != nil {
		log.Printf("renewal sweep: %v", err)
		return
	}
	if n > 0 {
		log.Printf("renewal sweep: renewed %d budget(s)", n)
	}
}

func (r *Renewer) reconcileLogged(ctx context.Context) {
	start := time.Now().UTC()
	n, err := r.Reconcile(ctx)

	r.statusMu.Lock()
	r.lastReconcile = ReconcileStatus{At: time.Now().UTC(), Reconciled: n}
	if err != nil {
		r.lastReconcile.Err = err.Error()
	}
	r.statusMu.Unlock()

	if err != nil {
		log.Printf("reconcile: %v", err)
		return
	}
	if n > 0 {
		log.Printf("reconcile: rebuilt %d budget(s) in %s", n, time.Since(start).Round(time.Millisecond))
	}
}

// Reconcile rebuilds every budget's balance from the usage ledger and
// republishes the admission flags the balances derive. Safe to run at any
// time: the Metering Service is the only writer of balances and flags, and
// the rebuild is deterministic — it reproduces exactly what the renewal
// state machine holds (see rebuildBalance), so a reconcile that races a
// renewal converges rather than fights it. It runs at startup (Redis may
// have lost state while the service was down — absent keys read as "no
// budget applies") and periodically as a self-heal that bounds any drift
// between the ledger and the Redis counters, in either direction, by the
// reconcile interval.
func (r *Renewer) Reconcile(ctx context.Context) (int, error) {
	reconciled := 0
	for offset := 0; ; offset += r.batchSize {
		listing, err := r.store.ListAllBudgets(ctx, store.Pagination{Limit: r.batchSize, Offset: offset})
		if err != nil {
			return reconciled, fmt.Errorf("list budgets: %w", err)
		}
		for _, budget := range listing.Items {
			if err := r.reconcileBudget(ctx, budget); err != nil {
				return reconciled, fmt.Errorf("budget %s: %w", budget.Metadata.ID, err)
			}
			reconciled++
		}
		if offset+len(listing.Items) >= int(listing.Total) {
			return reconciled, nil
		}
	}
}

func (r *Renewer) reconcileBudget(ctx context.Context, budget api.BudgetResource) error {
	return Rebuild(ctx, r.store, r.kv, budget)
}

// Rebuild re-derives a budget's derived state — the balance counter and
// the admission flags affected by it — from the ledger, under the given
// (possibly just-updated) spec. Every path that writes a balance on any
// grounds other than a usage-event charge funnels through here — renewal
// refills, reconcile passes, lazy rebuilds, budget updates — so they all
// produce the same value and cannot fight each other.
func Rebuild(ctx context.Context, s store.Store, kv store.KeyStore, budget api.BudgetResource) error {
	if _, err := rebuildBalance(ctx, s, kv, budget); err != nil {
		return err
	}
	return recomputeFlags(ctx, s, kv, budget.Metadata.TenantID, budget.Metadata.ID)
}

// rebuildBalance derives a budget's current balance from the ledger and
// writes it back to Redis. It reproduces exactly what the renewal state
// machine holds, including catch-up over missed boundaries: the current
// period's opening balance — computed by the same NewBalance rule from the
// most recently completed period's spend — minus the charges already
// accrued in the current period. That identity is what makes reconcile a
// repair rather than a second writer with opinions: renewal and rebuild
// agree by construction.
//
// A budget still in its first period has no completed window, so its
// opening is the plain limit — no rollover carry (nothing was saved to
// carry) and no overage deduction (nothing was overspent) — matching what
// budget creation writes.
func rebuildBalance(ctx context.Context, s store.Store, kv store.KeyStore, budget api.BudgetResource) (int64, error) {
	tenantID := budget.Metadata.TenantID
	now := time.Now().UTC()

	d, err := period.Parse(budget.Spec.PeriodDuration)
	if err != nil {
		return 0, fmt.Errorf("parse period duration %q: %w", budget.Spec.PeriodDuration, err)
	}
	window, err := period.WindowAt(budget.Spec.PeriodStart, d, now)
	if err != nil {
		return 0, err
	}

	policies, err := s.FindPoliciesForBudget(ctx, tenantID, budget.Metadata.ID)
	if err != nil {
		return 0, fmt.Errorf("find policies: %w", err)
	}

	// The opening balance of the current period. During the first period
	// (PrevStart == CurrentStart) no period has completed to earn carry or
	// accrue overage, so the limit stands as the opening — NewBalance with
	// prevSpent=0 would wrongly grant a rollover budget its full carry.
	opening := budget.Spec.Limit
	if !window.PrevStart.Equal(window.CurrentStart) {
		prevSpent, err := s.SumBudgetCharges(ctx, tenantID, budget.Metadata.ID, window.PrevStart, window.CurrentStart)
		if err != nil {
			return 0, fmt.Errorf("sum previous period charges: %w", err)
		}
		opening = NewBalance(budget.Spec, policies, prevSpent)
	}
	currentSpent, err := s.SumBudgetCharges(ctx, tenantID, budget.Metadata.ID, window.CurrentStart, now)
	if err != nil {
		return 0, fmt.Errorf("sum current period charges: %w", err)
	}

	balance := opening - currentSpent

	balanceKey := admission.BuildBalanceKey(tenantID, budget.Metadata.ID)
	if err := kv.SetKey(ctx, tenantID, balanceKey, strconv.FormatInt(balance, 10)); err != nil {
		return 0, fmt.Errorf("set balance: %w", err)
	}
	return balance, nil
}

// renew applies one due budget's renewal. It runs inside the store's
// renewal transaction: the Redis writes below precede the claim's commit,
// so a crash mid-renewal is retried rather than skipped.
func (r *Renewer) renew(ctx context.Context, rn *store.BudgetRenewal) error {
	budget := rn.Budget
	tenantID := budget.Metadata.TenantID

	policies, err := r.store.FindPoliciesForBudget(ctx, tenantID, budget.Metadata.ID)
	if err != nil {
		return fmt.Errorf("find policies for budget %s: %w", budget.Metadata.ID, err)
	}

	// The refill: the new period's opening balance minus the charges
	// already accrued in the new period. Ingest does not wait for the
	// renewal loop, so by the time the sweep processes a boundary, the
	// current period may already carry decrements — a plain SET to the
	// opening balance would erase them from the counter. This is the same
	// identity rebuildBalance uses, which is what keeps renewal and
	// reconcile from disagreeing.
	//
	// The SET can still overwrite a concurrent in-flight DECRBY (a charge
	// landing between the ledger sum inside the claim and this write): the
	// ledger stays authoritative, reconcile rebuilds the balance from it,
	// and the next accounting pass re-derives the flag from the current
	// balance — the same class of accepted drift as out-of-order flag
	// writes.
	newBalance := NewBalance(budget.Spec, policies, rn.PrevSpent) - rn.CurrentSpent
	balanceKey := admission.BuildBalanceKey(tenantID, budget.Metadata.ID)
	if err := r.kv.SetKey(ctx, tenantID, balanceKey, strconv.FormatInt(newBalance, 10)); err != nil {
		return fmt.Errorf("set balance for budget %s: %w", budget.Metadata.ID, err)
	}

	// Lift the flags the old balance had driven to deny/over — without
	// this, a renewed budget keeps denying its identities until their next
	// event, and a hard-denied identity produces no events at all.
	return recomputeFlags(ctx, r.store, r.kv, tenantID, budget.Metadata.ID)
}
