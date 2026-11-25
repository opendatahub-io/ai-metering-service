package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/opendatahub-io/ai-metering-service/internal/admission"
	"github.com/opendatahub-io/ai-metering-service/internal/api"
	goredis "github.com/redis/go-redis/v9"
)

// These tests verify the renewal loop end to end: at a period boundary the
// balance is refilled per the budget's renewal semantics and the admission
// flags the old balance had driven to deny are recomputed from the new
// state.
//
// The budgets use a short period duration with a back-dated anchor so the
// first boundary is already due — the tests never wait for a period to
// elapse. The renewal itself is observed, not triggered: each test reads
// the loop's liveness signal from /health after its setup is complete,
// waits for the next sweep to run, and then asserts the renewed Redis
// state. This exercises the real loop (ticker, claim, refill, flag
// recompute) with no test-only endpoints. The wait is bounded by the
// configured sweep interval, which /health reports, so a deployment with a
// shorter interval gets proportionally faster tests.
//
// The next boundary is far enough out that exactly one renewal runs during
// the test — a recurring short period would re-renew with an empty previous
// period and lift a deduct_overage deny (overage is forgiven after one
// period, by design).

// renewalFixture is one model, rate, budget, and tenant-subject policy, plus
// the Redis handles for the budget's derived state.
type renewalFixture struct {
	balanceKey string
	resource   map[string]string
	client     *goredis.Client
}

// ingestAt ingests one event as alice at the given event time; the event
// time decides which period the charge lands in.
func (rf *renewalFixture) ingestAt(t *testing.T, eventID string, eventTime time.Time, inputTokens int64) {
	t.Helper()
	resp := doRequest(t, "POST", "/api/v1/events", map[string]any{
		"specversion":     "1.0",
		"id":              eventID,
		"source":          "praxis/e2e-test",
		"type":            "io.opendatahub.aigateway.usage.v1",
		"time":            eventTime.Format(time.RFC3339),
		"datacontenttype": "application/json",
		"data": map[string]any{
			"request_id": "req-" + eventID,
			"identity":   map[string]any{"user": "alice", "tenant": tenant},
			"resource":   rf.resource,
			"usage":      map[string]int64{"input_tokens": inputTokens},
		},
	})
	requireStatus(t, resp, http.StatusNoContent)
}

func (rf *renewalFixture) flagKey() string {
	return admission.BuildAdmissionKey(tenant, api.Subject{Kind: api.SubjectKindTenant, Name: tenant}, rf.resource)
}

func (rf *renewalFixture) expectBalance(t *testing.T, want int64) {
	t.Helper()
	if got := redisGet(t, rf.client, rf.balanceKey); got != fmt.Sprintf("%d", want) {
		t.Fatalf("balance %s: expected %d, got %s", rf.balanceKey, want, got)
	}
}

func (rf *renewalFixture) expectFlag(t *testing.T, want string) {
	t.Helper()
	if got := redisGet(t, rf.client, rf.flagKey()); got != want {
		t.Fatalf("admission flag %s: expected %s, got %s", rf.flagKey(), want, got)
	}
}

// renewalHealth is the renewal block of the /health payload.
type renewalHealth struct {
	LastSweepAt       time.Time `json:"last_sweep_at"`
	Interval          string    `json:"interval"`
	LastReconcileAt   time.Time `json:"last_reconcile_at"`
	ReconcileInterval string    `json:"reconcile_interval"`
}

func getRenewalHealth(t *testing.T) renewalHealth {
	t.Helper()
	resp := doRequest(t, "GET", "/health", nil)
	requireStatus(t, resp, http.StatusOK)
	var h struct {
		Renewal renewalHealth `json:"renewal"`
	}
	jsonUnmarshal(t, readBody(t, resp), &h)
	return h.Renewal
}

// waitForSweep waits until the renewal loop has run a sweep after the given
// baseline. Call it with a baseline read after the test's setup (budgets
// created, events ingested): the waited-for sweep then observes the final
// ledger state, and the assertions that follow are deterministic — a sweep
// that already ran before the baseline does not count, since it may have
// renewed from incomplete setup.
func waitForSweep(t *testing.T, baseline time.Time) {
	t.Helper()
	health := getRenewalHealth(t)
	interval, err := time.ParseDuration(health.Interval)
	if err != nil {
		t.Fatalf("parse sweep interval %q from /health: %v", health.Interval, err)
	}

	// One tick plus a margin for the sweep's own work.
	deadline := time.Now().Add(interval + 15*time.Second)
	for {
		if getRenewalHealth(t).LastSweepAt.After(baseline) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("renewal loop did not sweep within its interval (%s) plus margin — last sweep at %s",
				interval, baseline)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// waitForReconcile waits until the reconcile loop has run a pass after the
// given baseline. Skips the test when the configured reconcile interval is
// too slow for an e2e wait — run the stack with a small
// renewal.reconcileInterval to enable reconcile coverage.
func waitForReconcile(t *testing.T, baseline time.Time) {
	t.Helper()
	health := getRenewalHealth(t)
	interval, err := time.ParseDuration(health.ReconcileInterval)
	if err != nil {
		t.Fatalf("parse reconcile interval %q from /health: %v", health.ReconcileInterval, err)
	}
	if interval > time.Minute {
		t.Skipf("reconcile interval %s is too slow for an e2e wait; configure renewal.reconcileInterval smaller to enable this test", interval)
	}

	// One interval plus a margin for the reconcile pass's own work.
	deadline := time.Now().Add(interval + 30*time.Second)
	for {
		if getRenewalHealth(t).LastReconcileAt.After(baseline) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reconcile loop did not run within its interval (%s) plus margin — last reconcile at %s",
				interval, baseline)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// newRenewalFixture creates the model/rate/budget/policy set for one
// renewal scenario. The budget's anchor is back-dated so its first boundary
// (anchor + PT120S) is already due and the next one is ~2 minutes out.
func newRenewalFixture(t *testing.T, model, renewalType string, limit int64, maxCarry *int64, deductOverage bool) *renewalFixture {
	t.Helper()
	resource := map[string]string{"model": model, "provider": "test"}

	resp := doRequest(t, "POST", "/api/v1/rates", map[string]any{
		"name": model + "-rate",
		"items": []map[string]any{
			{"resource": map[string]string{"model": model}, "costs": map[string]int64{"input_tokens": 3}},
		},
	})
	requireStatus(t, resp, http.StatusCreated)
	var rateCreated map[string]any
	jsonUnmarshal(t, readBody(t, resp), &rateCreated)
	rateID := rateCreated["metadata"].(map[string]any)["id"].(string)

	anchor := time.Now().UTC().Add(-130 * time.Second)
	budget := map[string]any{
		"name":            model + "-budget",
		"limit":           limit,
		"period_start":    anchor.Format(time.RFC3339),
		"period_duration": "PT120S",
		"renewal_type":    renewalType,
	}
	if maxCarry != nil {
		budget["max_carry"] = *maxCarry
	}
	resp = doRequest(t, "POST", "/api/v1/budgets", budget)
	requireStatus(t, resp, http.StatusCreated)
	var budgetCreated map[string]any
	jsonUnmarshal(t, readBody(t, resp), &budgetCreated)
	budgetID := budgetCreated["metadata"].(map[string]any)["id"].(string)

	resp = doRequest(t, "POST", "/api/v1/budget_policies", map[string]any{
		"budget_ref":     budgetID,
		"rate_ref":       rateID,
		"subjects":       []map[string]string{{"kind": "tenant", "name": tenant}},
		"deduct_overage": deductOverage,
	})
	requireStatus(t, resp, http.StatusCreated)

	return &renewalFixture{
		balanceKey: admission.BuildBalanceKey(tenant, budgetID),
		resource:   resource,
		client:     redisClient(t),
	}
}

func int64Ptr(v int64) *int64 { return &v }

func TestRenewal_RefillsBalanceAndLiftsDeny(t *testing.T) {
	// A drain budget, exhausted into hard denial in the completed period:
	// the renewal must refill the balance to the limit and lift the deny
	// flag to allow. Without the flag recompute, a hard-denied identity
	// produces no further events and the deny would never lift.
	rf := newRenewalFixture(t, "renewal-drain", "drain", 300, nil, false)

	// Cost 600 in the completed period against a 300 budget: the period
	// ended at -300 with the flag deny.
	eventTime := time.Now().UTC().Add(-70 * time.Second)
	rf.ingestAt(t, "evt-renewal-drain-001", eventTime, 200)

	waitForSweep(t, getRenewalHealth(t).LastSweepAt)
	rf.expectBalance(t, 300) // limit, drained
	rf.expectFlag(t, "allow")
}

func TestRenewal_CurrentPeriodChargesSurviveRefill(t *testing.T) {
	// Charges accrued in the new period before the renewal processes the
	// boundary must survive the refill: ingest does not wait for the
	// renewal loop, so a plain SET to the opening balance would erase
	// them. The renewed balance is opening (limit) minus what the current
	// period has already spent.
	rf := newRenewalFixture(t, "renewal-current", "drain", 300, nil, false)

	// Cost 600 charged after the boundary (the current period).
	rf.ingestAt(t, "evt-renewal-current-001", time.Now().UTC(), 200)

	waitForSweep(t, getRenewalHealth(t).LastSweepAt)
	rf.expectBalance(t, -300) // 300 limit - 600 current-period charges
	rf.expectFlag(t, "deny")
}

func TestRenewal_ReconcileRebuildsAfterRedisLoss(t *testing.T) {
	// The repair path: Redis loses a budget's derived state (flush,
	// failover) — absent keys would read as "no budget applies", i.e.
	// unlimited. The reconcile loop rebuilds the balance and flags from
	// the ledger.
	rf := newRenewalFixture(t, "renewal-reconcile", "drain", 300, nil, false)

	// Cost 600 charged in the current period: balance -300, flag deny.
	rf.ingestAt(t, "evt-renewal-reconcile-001", time.Now().UTC(), 200)
	rf.expectBalance(t, -300)
	rf.expectFlag(t, "deny")

	// Simulate the loss: drop the derived state, leave the ledger intact.
	deleted, err := rf.client.Del(context.Background(), rf.balanceKey, rf.flagKey()).Result()
	if err != nil || deleted != 2 {
		t.Fatalf("drop derived state: deleted=%d err=%v", deleted, err)
	}

	waitForReconcile(t, getRenewalHealth(t).LastReconcileAt)
	rf.expectBalance(t, -300) // rebuilt: opening 300 - current spend 600
	rf.expectFlag(t, "deny")
}

func TestRenewal_DeductOverageCarriesDebt(t *testing.T) {
	// A deduct_overage budget overspent by 600 in the completed period:
	// the renewal starts the new period at limit - 600 = -300, and the
	// flag must stay deny — the recompute derives it from the renewed
	// (still negative) balance rather than assuming a renewal allows.
	rf := newRenewalFixture(t, "renewal-deduct", "drain", 300, nil, true)

	// The event is back-dated into the completed period so its cost counts
	// as the previous period's spend. Cost 900: overage 600.
	eventTime := time.Now().UTC().Add(-70 * time.Second)
	rf.ingestAt(t, "evt-renewal-deduct-001", eventTime, 300)

	waitForSweep(t, getRenewalHealth(t).LastSweepAt)
	rf.expectBalance(t, -300) // 300 - 600 overage
	rf.expectFlag(t, "deny")
}

func TestRenewal_RolloverCarriesUnused(t *testing.T) {
	// A rollover budget barely used in the completed period: the renewal
	// carries the unused budget forward, capped by max_carry.
	rf := newRenewalFixture(t, "renewal-rollover", "rollover", 300, int64Ptr(100), false)

	// Cost 30 in the completed period: 270 unused, carry capped at 100.
	eventTime := time.Now().UTC().Add(-70 * time.Second)
	rf.ingestAt(t, "evt-renewal-rollover-001", eventTime, 10)

	waitForSweep(t, getRenewalHealth(t).LastSweepAt)
	rf.expectBalance(t, 400) // 300 limit + 100 carry
	rf.expectFlag(t, "allow")
}
