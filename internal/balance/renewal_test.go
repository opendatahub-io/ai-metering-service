package balance

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/opendatahub-io/ai-metering-service/internal/admission"
	"github.com/opendatahub-io/ai-metering-service/internal/api"
	"github.com/opendatahub-io/ai-metering-service/internal/period"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
)

// --- NewBalance ---

func int64Ptr(v int64) *int64 { return &v }

func TestNewBalance(t *testing.T) {
	cases := []struct {
		name      string
		budget    api.Budget
		deduct    bool
		prevSpent int64
		want      int64
	}{
		{
			name:      "drain resets to limit",
			budget:    api.Budget{Limit: 1000, RenewalType: api.RenewalDrain},
			prevSpent: 400,
			want:      1000,
		},
		{
			name:      "drain ignores overage without deduct_overage",
			budget:    api.Budget{Limit: 1000, RenewalType: api.RenewalDrain},
			prevSpent: 1500, // 500 over
			want:      1000,
		},
		{
			name:      "drain with deduct_overage subtracts overspend",
			budget:    api.Budget{Limit: 1000, RenewalType: api.RenewalDrain},
			deduct:    true,
			prevSpent: 1500,
			want:      500,
		},
		{
			name:      "deduct_overage with no overspend deducts nothing",
			budget:    api.Budget{Limit: 1000, RenewalType: api.RenewalDrain},
			deduct:    true,
			prevSpent: 400,
			want:      1000,
		},
		{
			name:      "rollover carries unused budget",
			budget:    api.Budget{Limit: 1000, RenewalType: api.RenewalRollover},
			prevSpent: 400,
			want:      1600,
		},
		{
			name:      "rollover carries nothing when fully spent",
			budget:    api.Budget{Limit: 1000, RenewalType: api.RenewalRollover},
			prevSpent: 1000,
			want:      1000,
		},
		{
			name:      "rollover carries nothing in overage",
			budget:    api.Budget{Limit: 1000, RenewalType: api.RenewalRollover},
			prevSpent: 1200,
			want:      1000,
		},
		{
			name:      "rollover capped by max_carry",
			budget:    api.Budget{Limit: 1000, RenewalType: api.RenewalRollover, MaxCarry: int64Ptr(100)},
			prevSpent: 400,
			want:      1100,
		},
		{
			name:      "rollover below the cap carries it all",
			budget:    api.Budget{Limit: 1000, RenewalType: api.RenewalRollover, MaxCarry: int64Ptr(900)},
			prevSpent: 400,
			want:      1600,
		},
		{
			name:      "rollover with deduct_overage: carry minus overspend",
			budget:    api.Budget{Limit: 1000, RenewalType: api.RenewalRollover},
			deduct:    true,
			prevSpent: 1200, // 200 over → no carry, 200 deducted
			want:      800,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			policies := []api.BudgetPolicyResource{}
			if c.deduct {
				policies = append(policies, api.BudgetPolicyResource{
					Spec: api.BudgetPolicy{DeductOverage: true},
				})
			}
			if got := NewBalance(c.budget, policies, c.prevSpent); got != c.want {
				t.Fatalf("expected %d, got %d", c.want, got)
			}
		})
	}
}

func TestNewBalance_DeductOverageFirstPolicyWins(t *testing.T) {
	// deduct_overage is per policy but the balance is per budget: the
	// first policy enabling it applies, once — not once per policy.
	budget := api.Budget{Limit: 1000, RenewalType: api.RenewalDrain}
	policies := []api.BudgetPolicyResource{
		{Spec: api.BudgetPolicy{DeductOverage: true}},
		{Spec: api.BudgetPolicy{DeductOverage: true}},
	}
	if got := NewBalance(budget, policies, 1500); got != 500 {
		t.Fatalf("expected 500, got %d", got)
	}
}

// --- Flag recompute ---

// fakeKeyStore is an in-memory KeyStore that records which keys SetKey
// wrote, so tests can assert the write-only-changed rule.
type fakeKeyStore struct {
	data    map[string]string
	written map[string]bool
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{data: map[string]string{}, written: map[string]bool{}}
}

func (f *fakeKeyStore) SetKey(_ context.Context, _, key, value string) error {
	f.data[key] = value
	f.written[key] = true
	return nil
}

func (f *fakeKeyStore) GetKey(_ context.Context, _, key string) (string, error) {
	if v, ok := f.data[key]; ok {
		return v, nil
	}
	return "", store.ErrKeyNotFound
}

func (f *fakeKeyStore) DeleteKey(_ context.Context, _, key string) error {
	delete(f.data, key)
	return nil
}

func (f *fakeKeyStore) DecrKey(_ context.Context, _, key string, value int64) (int64, error) {
	v, _ := strconv.ParseInt(f.data[key], 10, 64)
	v -= value
	f.data[key] = strconv.FormatInt(v, 10)
	return v, nil
}

func (f *fakeKeyStore) setBalance(t *testing.T, tenantID, budgetID string, balance int64) {
	t.Helper()
	key := admission.BuildBalanceKey(tenantID, budgetID)
	f.data[key] = strconv.FormatInt(balance, 10)
}

// fakeRenewalStore implements the recompute and rebuild paths of
// store.Store; any other method panics via the nil embedded interface.
type fakeRenewalStore struct {
	store.Store
	policies         []api.BudgetPolicyResource
	budgetPolicies   []api.BudgetPolicyResource
	rates            map[string]*api.RateResource
	targets          []api.ChargeTarget
	budget           *api.BudgetResource
	sumBudgetCharges func(from, to time.Time) int64
}

func (f *fakeRenewalStore) FindPoliciesWithRates(_ context.Context, _ string, _ api.Identity) ([]api.PolicyWithRates, error) {
	// Join the policies with their rates, the way the single-query
	// resolution does; a missing rate errors like the real LEFT JOIN path.
	var resolved []api.PolicyWithRates
	for _, policy := range f.policies {
		rate, ok := f.rates[policy.Spec.RateRef]
		if !ok {
			return nil, fmt.Errorf("get rate %s: rate does not resolve", policy.Spec.RateRef)
		}
		resolved = append(resolved, api.PolicyWithRates{Policy: policy, Items: rate.Spec.Items})
	}
	return resolved, nil
}

func (f *fakeRenewalStore) FindPoliciesForBudget(_ context.Context, _, _ string) ([]api.BudgetPolicyResource, error) {
	return f.budgetPolicies, nil
}

func (f *fakeRenewalStore) GetBudget(_ context.Context, _, id string) (*api.BudgetResource, error) {
	if f.budget != nil && f.budget.Metadata.ID == id {
		return f.budget, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakeRenewalStore) SumBudgetCharges(_ context.Context, _, _ string, from, to time.Time) (int64, error) {
	return f.sumBudgetCharges(from, to), nil
}

func (f *fakeRenewalStore) GetRate(_ context.Context, _, id string) (*api.RateResource, error) {
	rate, ok := f.rates[id]
	if !ok {
		return nil, fmt.Errorf("rate %s not found", id)
	}
	return rate, nil
}

func (f *fakeRenewalStore) ListChargeTargets(_ context.Context, _, _ string) ([]api.ChargeTarget, error) {
	return f.targets, nil
}

const tenantID = "acme-corp"

func recomputeFixture(t *testing.T, balances map[string]int64, policies []api.BudgetPolicyResource) (*fakeRenewalStore, *fakeKeyStore) {
	t.Helper()
	kv := newFakeKeyStore()
	for budgetID, balance := range balances {
		kv.setBalance(t, tenantID, budgetID, balance)
	}
	s := &fakeRenewalStore{
		policies: policies,
		rates: map[string]*api.RateResource{
			"rate-1": {Spec: api.Rate{Name: "rate-1", Items: []api.RateItem{{
				Resource: map[string]string{},
				Costs:    map[string]int64{"input_tokens": 1},
			}}}},
		},
	}
	return s, kv
}

func TestRecomputeFlags_LiftsDenyAfterRenewal(t *testing.T) {
	// The scenario renewal exists for: alice's budget went negative, her
	// flag was written deny, the period renewed, the balance is back at
	// the limit — the recompute must lift the flag to allow.
	policies := []api.BudgetPolicyResource{{
		Metadata: api.Metadata{ID: "p1", TenantID: tenantID},
		Spec: api.BudgetPolicy{
			BudgetRef: "budget-1",
			RateRef:   "rate-1",
			Subjects:  []api.Subject{{Kind: api.SubjectKindUser, Name: "alice"}},
		},
	}}
	resource := map[string]string{"model": "claude"}
	s, kv := recomputeFixture(t, map[string]int64{"budget-1": 1000}, policies)

	flagKey := admission.BuildAdmissionKey(tenantID, api.Subject{Kind: api.SubjectKindUser, Name: "alice"}, resource)
	kv.data[flagKey] = string(admission.StateDeny)

	if err := recomputeFlag(context.Background(), s, kv, tenantID, api.Identity{User: "alice", Tenant: tenantID}, resource); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := kv.data[flagKey]; got != string(admission.StateAllow) {
		t.Fatalf("expected the deny flag to lift to allow, got %q", got)
	}
}

func TestRecomputeFlags_KeepsDenyFromOtherBudget(t *testing.T) {
	// Two budgets write alice's flag. Renewing budget-1 must not lift the
	// deny driven by budget-2: the fold is over every matching budget's
	// current balance, not just the renewed one.
	soft := &api.OverageConfig{Type: "soft"}
	policies := []api.BudgetPolicyResource{
		{
			Metadata: api.Metadata{ID: "p1", TenantID: tenantID},
			Spec: api.BudgetPolicy{
				BudgetRef: "budget-1",
				RateRef:   "rate-1",
				Subjects:  []api.Subject{{Kind: api.SubjectKindUser, Name: "alice"}},
			},
		},
		{
			Metadata: api.Metadata{ID: "p2", TenantID: tenantID},
			Spec: api.BudgetPolicy{
				BudgetRef: "budget-2",
				RateRef:   "rate-1",
				Subjects:  []api.Subject{{Kind: api.SubjectKindUser, Name: "alice"}},
				Overage:   soft,
			},
		},
	}
	resource := map[string]string{"model": "claude"}
	// budget-1 renewed and healthy; budget-2 still in soft overage.
	s, kv := recomputeFixture(t, map[string]int64{"budget-1": 1000, "budget-2": -50}, policies)

	flagKey := admission.BuildAdmissionKey(tenantID, api.Subject{Kind: api.SubjectKindUser, Name: "alice"}, resource)

	if err := recomputeFlag(context.Background(), s, kv, tenantID, api.Identity{User: "alice", Tenant: tenantID}, resource); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := kv.data[flagKey]; got != string(admission.StateOver) {
		t.Fatalf("expected over (budget-2 soft overage wins the fold), got %q", got)
	}
}

func TestRecomputeFlags_DeductOverageCanStartNegative(t *testing.T) {
	// deduct_overage can renew a budget below its limit — even negative.
	// The recompute must write deny for a hard-enforced policy, not allow.
	policies := []api.BudgetPolicyResource{{
		Metadata: api.Metadata{ID: "p1", TenantID: tenantID},
		Spec: api.BudgetPolicy{
			BudgetRef: "budget-1",
			RateRef:   "rate-1",
			Subjects:  []api.Subject{{Kind: api.SubjectKindUser, Name: "alice"}},
		},
	}}
	resource := map[string]string{"model": "claude"}
	s, kv := recomputeFixture(t, map[string]int64{"budget-1": -200}, policies)

	flagKey := admission.BuildAdmissionKey(tenantID, api.Subject{Kind: api.SubjectKindUser, Name: "alice"}, resource)
	kv.data[flagKey] = string(admission.StateAllow)

	if err := recomputeFlag(context.Background(), s, kv, tenantID, api.Identity{User: "alice", Tenant: tenantID}, resource); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := kv.data[flagKey]; got != string(admission.StateDeny) {
		t.Fatalf("expected deny for a renewed-negative balance, got %q", got)
	}
}

func TestRecomputeFlags_WritesOnlyChanged(t *testing.T) {
	policies := []api.BudgetPolicyResource{{
		Metadata: api.Metadata{ID: "p1", TenantID: tenantID},
		Spec: api.BudgetPolicy{
			BudgetRef: "budget-1",
			RateRef:   "rate-1",
			Subjects:  []api.Subject{{Kind: api.SubjectKindTenant, Name: tenantID}},
		},
	}}
	resource := map[string]string{"model": "claude"}
	s, kv := recomputeFixture(t, map[string]int64{"budget-1": 1000}, policies)

	flagKey := admission.BuildAdmissionKey(tenantID, api.Subject{Kind: api.SubjectKindTenant, Name: tenantID}, resource)
	kv.data[flagKey] = string(admission.StateAllow) // already correct

	if err := recomputeFlag(context.Background(), s, kv, tenantID, api.Identity{User: "alice", Tenant: tenantID}, resource); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The write-only-changed rule: SetKey must not be called for a flag
	// whose value already matches.
	if kv.written[flagKey] {
		t.Fatal("expected no write for a flag whose value already matches")
	}
	if got := kv.data[flagKey]; got != string(admission.StateAllow) {
		t.Fatalf("expected allow, got %q", got)
	}
}

func TestRecomputeFlags_FoldsAllSubjectsOfIdentity(t *testing.T) {
	// Alice is in the engineering group and covered by a group policy:
	// the recompute must write both her user key and the group key.
	policies := []api.BudgetPolicyResource{{
		Metadata: api.Metadata{ID: "p1", TenantID: tenantID},
		Spec: api.BudgetPolicy{
			BudgetRef: "budget-1",
			RateRef:   "rate-1",
			Subjects:  []api.Subject{{Kind: api.SubjectKindGroup, Name: "engineering"}},
		},
	}}
	resource := map[string]string{"model": "claude"}
	s, kv := recomputeFixture(t, map[string]int64{"budget-1": -10}, policies)

	identity := api.Identity{User: "alice", Groups: []string{"engineering"}, Tenant: tenantID}
	if err := recomputeFlag(context.Background(), s, kv, tenantID, identity, resource); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	groupKey := admission.BuildAdmissionKey(tenantID, api.Subject{Kind: api.SubjectKindGroup, Name: "engineering"}, resource)
	if got := kv.data[groupKey]; got != string(admission.StateDeny) {
		t.Fatalf("expected the group flag to be deny, got %q", got)
	}
}

func TestRecomputeFlags_SkipsBudgetWithoutBalanceState(t *testing.T) {
	// A budget whose balance key is absent (Redis flushed, republish
	// pending) contributes nothing rather than reading as a zero balance.
	policies := []api.BudgetPolicyResource{{
		Metadata: api.Metadata{ID: "p1", TenantID: tenantID},
		Spec: api.BudgetPolicy{
			BudgetRef: "missing-budget",
			RateRef:   "rate-1",
			Subjects:  []api.Subject{{Kind: api.SubjectKindUser, Name: "alice"}},
		},
	}}
	resource := map[string]string{"model": "claude"}
	s, kv := recomputeFixture(t, nil, policies)

	if err := recomputeFlag(context.Background(), s, kv, tenantID, api.Identity{User: "alice", Tenant: tenantID}, resource); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flagKey := admission.BuildAdmissionKey(tenantID, api.Subject{Kind: api.SubjectKindUser, Name: "alice"}, resource)
	if _, exists := kv.data[flagKey]; exists {
		t.Fatal("a budget without balance state must not produce a flag")
	}
}

func TestRecomputeFlags_RateMismatchLeavesFlagUntouched(t *testing.T) {
	// The policy's rate does not cover the resource: the policy
	// contributes no state, and an existing flag is left as-is (deleting
	// it would fail open).
	policies := []api.BudgetPolicyResource{{
		Metadata: api.Metadata{ID: "p1", TenantID: tenantID},
		Spec: api.BudgetPolicy{
			BudgetRef: "budget-1",
			RateRef:   "rate-1",
			Subjects:  []api.Subject{{Kind: api.SubjectKindUser, Name: "alice"}},
		},
	}}
	resource := map[string]string{"model": "granite"} // rate-1 is the catch-all — override below
	s, kv := recomputeFixture(t, map[string]int64{"budget-1": -10}, policies)
	// Replace the wildcard rate with one that only covers another model.
	s.rates["rate-1"] = &api.RateResource{
		Spec: api.Rate{Name: "rate-1", Items: []api.RateItem{{
			Resource: map[string]string{"model": "claude"},
			Costs:    map[string]int64{"input_tokens": 1},
		}}},
	}

	flagKey := admission.BuildAdmissionKey(tenantID, api.Subject{Kind: api.SubjectKindUser, Name: "alice"}, resource)
	kv.data[flagKey] = string(admission.StateOver)

	if err := recomputeFlag(context.Background(), s, kv, tenantID, api.Identity{User: "alice", Tenant: tenantID}, resource); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := kv.data[flagKey]; got != string(admission.StateOver) {
		t.Fatalf("expected the flag to be left untouched at over, got %q", got)
	}
}

// --- Reconcile / rebuild ---

// rebuildFixture builds a Renewer around a budget anchored 130s in the past
// with a PT120S period, so the completed window is [anchor, anchor+120s)
// and the current period started 10s ago. The fake ledger returns
// prevSpent for the completed window and currentSpent for the current one.
func rebuildFixture(t *testing.T, budget api.Budget, policies []api.BudgetPolicyResource, prevSpent, currentSpent int64) (*fakeRenewalStore, *fakeKeyStore) {
	t.Helper()
	budget.PeriodDuration = "PT120S"
	budget.PeriodStart = time.Now().UTC().Add(-130 * time.Second)
	resource := &api.BudgetResource{
		Metadata: api.Metadata{ID: "budget-1", TenantID: tenantID},
		Spec:     budget,
	}

	kv := newFakeKeyStore()
	s := &fakeRenewalStore{
		policies:       policies,
		budgetPolicies: policies,
		rates:          map[string]*api.RateResource{},
		budget:         resource,
		sumBudgetCharges: func(from, to time.Time) int64 {
			if from.Equal(budget.PeriodStart) {
				return prevSpent
			}
			return currentSpent
		},
	}
	return s, kv
}

func TestRebuildBalance(t *testing.T) {
	// The rebuild identity: opening balance (from the completed period's
	// spend) minus the current period's accrued charges — the same value
	// the renewal state machine holds.
	cases := []struct {
		name         string
		budget       api.Budget
		deduct       bool
		prevSpent    int64
		currentSpent int64
		want         int64
	}{
		{"drain", api.Budget{Limit: 300, RenewalType: api.RenewalDrain}, false, 600, 0, 300},
		{"drain with current-period charges", api.Budget{Limit: 300, RenewalType: api.RenewalDrain}, false, 0, 600, -300},
		{"deduct_overage", api.Budget{Limit: 300, RenewalType: api.RenewalDrain}, true, 900, 0, -300},
		{"deduct_overage with current charges", api.Budget{Limit: 300, RenewalType: api.RenewalDrain}, true, 900, 100, -400},
		{"rollover capped", api.Budget{Limit: 300, RenewalType: api.RenewalRollover, MaxCarry: int64Ptr(100)}, false, 30, 50, 350},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			policies := []api.BudgetPolicyResource{}
			if c.deduct {
				policies = append(policies, api.BudgetPolicyResource{Spec: api.BudgetPolicy{DeductOverage: true}})
			}
			s, kv := rebuildFixture(t, c.budget, policies, c.prevSpent, c.currentSpent)

			balance, err := rebuildBalance(context.Background(), s, kv, *s.budget)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if balance != c.want {
				t.Fatalf("expected balance %d, got %d", c.want, balance)
			}
			// The rebuild writes back, so later readers find it.
			if got := kv.data[admission.BuildBalanceKey(tenantID, "budget-1")]; got != strconv.FormatInt(c.want, 10) {
				t.Fatalf("expected the rebuilt balance written back as %d, got %q", c.want, got)
			}
		})
	}
}

func TestRecomputeFlags_RebuildsMissingBalance(t *testing.T) {
	// Redis lost this budget's balance (flush, failover) and the periodic
	// reconcile has not caught up: the flag recompute rebuilds it from the
	// ledger on the spot and derives the state from the rebuilt value,
	// rather than skipping the budget's contribution.
	policies := []api.BudgetPolicyResource{{
		Metadata: api.Metadata{ID: "p1", TenantID: tenantID},
		Spec: api.BudgetPolicy{
			BudgetRef: "budget-1",
			RateRef:   "rate-1",
			Subjects:  []api.Subject{{Kind: api.SubjectKindUser, Name: "alice"}},
		},
	}}
	budget := api.Budget{Limit: 300, RenewalType: api.RenewalDrain}
	s, kv := rebuildFixture(t, budget, policies, 0, 600) // current period spent 600 → -300
	s.rates["rate-1"] = &api.RateResource{Spec: api.Rate{Name: "rate-1", Items: []api.RateItem{{
		Resource: map[string]string{},
		Costs:    map[string]int64{"input_tokens": 1},
	}}}}

	resource := map[string]string{"model": "claude"}
	if err := recomputeFlag(context.Background(), s, kv, tenantID, api.Identity{User: "alice", Tenant: tenantID}, resource); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	flagKey := admission.BuildAdmissionKey(tenantID, api.Subject{Kind: api.SubjectKindUser, Name: "alice"}, resource)
	if got := kv.data[flagKey]; got != string(admission.StateDeny) {
		t.Fatalf("expected deny from the rebuilt balance (-300), got %q", got)
	}
	if got := kv.data[admission.BuildBalanceKey(tenantID, "budget-1")]; got != "-300" {
		t.Fatalf("expected the rebuilt balance -300 written back, got %q", got)
	}
}

func TestRecomputeFlags_SkipsUnresolvableBudgetWithoutBalance(t *testing.T) {
	// A policy whose budget no longer resolves, with no balance state
	// either: skip its contribution rather than failing the recompute.
	policies := []api.BudgetPolicyResource{{
		Metadata: api.Metadata{ID: "p1", TenantID: tenantID},
		Spec: api.BudgetPolicy{
			BudgetRef: "deleted-budget",
			RateRef:   "rate-1",
			Subjects:  []api.Subject{{Kind: api.SubjectKindUser, Name: "alice"}},
		},
	}}
	resource := map[string]string{"model": "claude"}
	s, kv := recomputeFixture(t, nil, policies)

	if err := recomputeFlag(context.Background(), s, kv, tenantID, api.Identity{User: "alice", Tenant: tenantID}, resource); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flagKey := admission.BuildAdmissionKey(tenantID, api.Subject{Kind: api.SubjectKindUser, Name: "alice"}, resource)
	if _, exists := kv.data[flagKey]; exists {
		t.Fatal("an unresolvable budget must not produce a flag")
	}
}

func TestRebuildBalance_FirstPeriodHasNoCarry(t *testing.T) {
	// A rollover budget still in its first period has no completed window:
	// nothing was saved yet, so the rebuild must not grant carry — the
	// opening is the plain limit, matching what budget creation wrote.
	// (NewBalance with prevSpent=0 would wrongly award the full carry.)
	budget := api.Budget{Limit: 300, RenewalType: api.RenewalRollover, MaxCarry: int64Ptr(100)}
	s, kv := rebuildFixture(t, budget, nil, 0, 50)
	s.budget.Spec.PeriodStart = time.Now().UTC() // anchor now: no boundary has passed

	balance, err := rebuildBalance(context.Background(), s, kv, *s.budget)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := int64(300 - 50); balance != want {
		t.Fatalf("expected balance %d (limit - current spend, no carry), got %d", want, balance)
	}
}

// --- ComputeUtilization ---

func TestComputeUtilization(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	window := period.Window{
		CurrentStart: now.AddDate(0, 0, -10),
		NextEnd:      now.AddDate(0, 0, 20),
		PrevStart:    now.AddDate(0, 0, -40), // a completed period exists
	}

	cases := []struct {
		name            string
		budget          api.Budget
		policies        []api.BudgetPolicyResource
		prevSpent       int64
		spent           int64
		wantOpening     int64
		wantRemaining   int64
		wantOverage     int64
		wantUtilization *float64
		wantCarry       int64
		wantCarriedDebt int64
		wantPrevOverage int64
		wantBurnRate    *float64
		wantProjection  bool
	}{
		{
			name: "healthy drain budget", budget: api.Budget{Limit: 1000, RenewalType: api.RenewalDrain},
			prevSpent: 100, spent: 400,
			wantOpening: 1000, wantRemaining: 600, wantUtilization: f64(0.4),
			wantPrevOverage: 0, wantBurnRate: f64(40), wantProjection: true, // 400 BU / 10 days
		},
		{
			name: "over budget", budget: api.Budget{Limit: 1000, RenewalType: api.RenewalDrain},
			prevSpent: 0, spent: 1300,
			wantOpening: 1000, wantRemaining: -300, wantOverage: 300, wantUtilization: f64(1.3),
			wantBurnRate: f64(130), wantProjection: false, // already in overage
		},
		{
			name: "deduct carries debt", budget: api.Budget{Limit: 1000, RenewalType: api.RenewalDrain},
			policies:  []api.BudgetPolicyResource{{Spec: api.BudgetPolicy{DeductOverage: true}}},
			prevSpent: 1500, spent: 200,
			wantOpening: 500, wantRemaining: 300, wantUtilization: f64(0.4),
			wantCarriedDebt: 500, wantPrevOverage: 500, wantBurnRate: f64(20), wantProjection: true,
		},
		{
			name: "rollover carries unused capped", budget: api.Budget{Limit: 1000, RenewalType: api.RenewalRollover, MaxCarry: int64Ptr(100)},
			prevSpent: 100, spent: 200,
			wantOpening: 1100, wantRemaining: 900, wantUtilization: f64(200.0 / 1100.0),
			wantCarry: 100, wantPrevOverage: 0, wantBurnRate: f64(20), wantProjection: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := api.BudgetResource{Metadata: api.Metadata{ID: "b1", TenantID: "acme-corp"}, Spec: tc.budget}
			u := ComputeUtilization(res, tc.policies, window, tc.prevSpent, tc.spent, now)

			if u.Opening != tc.wantOpening {
				t.Errorf("opening = %d, want %d", u.Opening, tc.wantOpening)
			}
			if u.Remaining != tc.wantRemaining {
				t.Errorf("remaining = %d, want %d", u.Remaining, tc.wantRemaining)
			}
			if u.Overage != tc.wantOverage {
				t.Errorf("overage = %d, want %d", u.Overage, tc.wantOverage)
			}
			if tc.wantUtilization == nil || u.Utilization == nil {
				if (tc.wantUtilization == nil) != (u.Utilization == nil) {
					t.Errorf("utilization = %v, want %v", u.Utilization, tc.wantUtilization)
				}
			} else if *u.Utilization != *tc.wantUtilization {
				t.Errorf("utilization = %v, want %v", *u.Utilization, *tc.wantUtilization)
			}
			if u.Carry != tc.wantCarry {
				t.Errorf("carry = %d, want %d", u.Carry, tc.wantCarry)
			}
			if u.CarriedDebt != tc.wantCarriedDebt {
				t.Errorf("carried debt = %d, want %d", u.CarriedDebt, tc.wantCarriedDebt)
			}
			if u.PrevOverage != tc.wantPrevOverage {
				t.Errorf("prev overage = %d, want %d", u.PrevOverage, tc.wantPrevOverage)
			}
			if tc.wantBurnRate == nil || u.BurnRate == nil {
				if (tc.wantBurnRate == nil) != (u.BurnRate == nil) {
					t.Errorf("burn rate = %v, want %v", u.BurnRate, tc.wantBurnRate)
				}
			} else if *u.BurnRate != *tc.wantBurnRate {
				t.Errorf("burn rate = %v, want %v", *u.BurnRate, *tc.wantBurnRate)
			}
			if (u.ProjectedExhaustion != nil) != tc.wantProjection {
				t.Errorf("projected exhaustion present = %v, want %v", u.ProjectedExhaustion != nil, tc.wantProjection)
			}
		})
	}
}

func f64(v float64) *float64 { return &v }

func TestComputeUtilizationFirstPeriod(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	// First period: PrevStart == CurrentStart — the limit stands, no
	// carry or debt, no burn rate before 24h elapsed.
	window := period.Window{CurrentStart: now.Add(-2 * time.Hour), NextEnd: now.AddDate(0, 0, 30), PrevStart: now.Add(-2 * time.Hour)}
	res := api.BudgetResource{Spec: api.Budget{Limit: 500, RenewalType: api.RenewalRollover}}

	u := ComputeUtilization(res, nil, window, 0, 50, now)

	if u.Opening != 500 || u.Remaining != 450 || u.Carry != 0 || u.CarriedDebt != 0 || u.PrevSpent != 0 {
		t.Errorf("first period: %+v", u)
	}
	if u.BurnRate != nil || u.ProjectedExhaustion != nil {
		t.Errorf("no burn rate or projection before 24h: %+v", u)
	}
}
