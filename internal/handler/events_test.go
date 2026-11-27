package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opendatahub-io/ai-metering-service/internal/admission"
	"github.com/opendatahub-io/ai-metering-service/internal/api"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
)

// fakeStore implements only the Store methods the ingest path touches; the
// embedded interface makes any other call a loud nil panic rather than a
// silent stub. chargeInserted is consumed one value per InsertUsageCharge
// call, so tests control exactly which charges the (usage_id, budget_ref)
// marker accepts — the first of two same-budget charges, or none for a full
// redelivery.
type fakeStore struct {
	store.Store
	policies          []api.PolicyWithRates
	chargeInserted    []bool
	insertChargeCalls int
}

func (s *fakeStore) FindPoliciesWithRates(ctx context.Context, tenantID string, identity api.Identity) ([]api.PolicyWithRates, error) {
	return s.policies, nil
}

// SettleUsage mirrors the real CTE's semantics for the tests: each charge
// consumes one chargeInserted flag, in order; the settled refs — those the
// marker accepted — are returned for the handler to decrement.
func (s *fakeStore) SettleUsage(ctx context.Context, record *api.UsageRecord, charges []api.UsageCharge) ([]string, error) {
	var settled []string
	for _, charge := range charges {
		inserted := s.chargeInserted[s.insertChargeCalls]
		s.insertChargeCalls++
		if inserted {
			settled = append(settled, charge.BudgetRef)
		}
	}
	return settled, nil
}

// paired joins a policy with its rate's items, the way
// FindPoliciesWithRates delivers them in one query.
func paired(policy api.BudgetPolicyResource, items []api.RateItem) api.PolicyWithRates {
	return api.PolicyWithRates{Policy: policy, Items: items}
}

// fakeKV tracks the one balance counter the overlapping-policy scenario
// drains, and records every admission flag write for assertion.
type fakeKV struct {
	store.KeyStore
	balance   int64
	decrCalls int
	set       map[string]string
}

func newFakeKV(balance int64) *fakeKV {
	return &fakeKV{balance: balance, set: make(map[string]string)}
}

func (k *fakeKV) DecrKey(ctx context.Context, tenantID, key string, value int64) (int64, error) {
	k.decrCalls++
	k.balance -= value
	return k.balance, nil
}

func (k *fakeKV) GetKey(ctx context.Context, tenantID, key string) (string, error) {
	if v, ok := k.set[key]; ok {
		return v, nil
	}
	return "", store.ErrKeyNotFound
}

func (k *fakeKV) SetKey(ctx context.Context, tenantID, key, value string) error {
	k.set[key] = value
	return nil
}

func ingestEvent(t *testing.T, fs *fakeStore, kv *fakeKV, event *api.CloudEvent) int {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/events", RequireTenant(), NewEventsHandler(fs, kv).Ingest)

	body, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant", "acme-corp")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Code
}

func usageEvent() *api.CloudEvent {
	return &api.CloudEvent{
		SpecVersion: "1.0",
		ID:          "evt-1",
		Source:      "praxis/test",
		Type:        "io.opendatahub.aigateway.usage.v1",
		Time:        time.Now(),
		Data: api.CloudEventData{
			RequestID: "req-1",
			Identity: api.Identity{
				User:   "alice",
				Groups: []string{"team"},
				Tenant: "acme-corp",
			},
			Resource: map[string]string{"model": "m1"},
			Usage:    map[string]int64{"input_tokens": 10},
		},
	}
}

// Two policies resolve for the same identity and reference the same budget:
// a user-level policy with hard enforcement and a group-level policy with
// soft overage. The budget must be charged once (the second charge's ledger
// insert collides on (usage_id, budget_ref)), yet both identity objects'
// flags must be rewritten from the shared budget's post-decrement balance —
// each under its own policy's overage semantics.
func TestIngestOverlappingPoliciesShareBudget(t *testing.T) {
	resource := map[string]string{"model": "m1"}
	userKey := admission.BuildAdmissionKey("acme-corp", api.Subject{Kind: api.SubjectKindUser, Name: "alice"}, resource)
	groupKey := admission.BuildAdmissionKey("acme-corp", api.Subject{Kind: api.SubjectKindGroup, Name: "team"}, resource)

	fs := &fakeStore{
		policies: []api.PolicyWithRates{
			paired(api.BudgetPolicyResource{Spec: api.BudgetPolicy{
				BudgetRef: "shared-budget",
				RateRef:   "r-user",
				Subjects:  []api.Subject{{Kind: api.SubjectKindUser, Name: "alice"}},
				// no overage: hard enforcement
			}}, []api.RateItem{{Resource: resource, Costs: map[string]int64{"input_tokens": 2}}}),
			paired(api.BudgetPolicyResource{Spec: api.BudgetPolicy{
				BudgetRef: "shared-budget",
				RateRef:   "r-group",
				Subjects:  []api.Subject{{Kind: api.SubjectKindGroup, Name: "team"}},
				Overage:   &api.OverageConfig{Type: "soft"},
			}}, []api.RateItem{{Resource: resource, Costs: map[string]int64{"input_tokens": 3}}}),
		},
		// The user policy's charge inserts and decrements; the group
		// policy's charge collides on the shared budget's marker.
		chargeInserted: []bool{true, false},
	}
	// Balance 10 minus the user policy's cost (10 tokens * 2) goes to -10:
	// negative, so the overage semantics decide each flag's state.
	kv := newFakeKV(10)

	if code := ingestEvent(t, fs, kv, usageEvent()); code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", code, http.StatusNoContent)
	}

	if fs.insertChargeCalls != 2 {
		t.Errorf("InsertUsageCharge calls = %d, want 2 (both charges attempted)", fs.insertChargeCalls)
	}
	if kv.decrCalls != 1 {
		t.Errorf("DecrKey calls = %d, want 1 (shared budget charged once)", kv.decrCalls)
	}
	if got, want := kv.set[userKey], string(admission.StateDeny); got != want {
		t.Errorf("user flag = %q, want %q (hard enforcement, negative balance)", got, want)
	}
	if got, want := kv.set[groupKey], string(admission.StateOver); got != want {
		t.Errorf("group flag = %q, want %q (soft overage, negative balance)", got, want)
	}
}

// A full redelivery settles nothing — every charge's marker already exists —
// so no balance moves and no flags are written; the first delivery's writes
// stand.
func TestIngestRedeliveryWritesNoFlags(t *testing.T) {
	resource := map[string]string{"model": "m1"}
	fs := &fakeStore{
		policies: []api.PolicyWithRates{
			paired(api.BudgetPolicyResource{Spec: api.BudgetPolicy{
				BudgetRef: "shared-budget",
				RateRef:   "r-user",
				Subjects:  []api.Subject{{Kind: api.SubjectKindUser, Name: "alice"}},
			}}, []api.RateItem{{Resource: resource, Costs: map[string]int64{"input_tokens": 2}}}),
		},
		chargeInserted: []bool{false},
	}
	kv := newFakeKV(10)

	if code := ingestEvent(t, fs, kv, usageEvent()); code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", code, http.StatusNoContent)
	}

	if kv.decrCalls != 0 {
		t.Errorf("DecrKey calls = %d, want 0", kv.decrCalls)
	}
	if len(kv.set) != 0 {
		t.Errorf("flag writes = %v, want none", kv.set)
	}
}

// overlappingFixture builds the two-policies-one-budget configuration with
// configurable per-token costs, so the ambiguous-pricing warning can be
// exercised with differing and with equal computed costs.
func overlappingFixture(userCost, groupCost int64) (*fakeStore, *fakeKV) {
	resource := map[string]string{"model": "m1"}
	fs := &fakeStore{
		policies: []api.PolicyWithRates{
			paired(api.BudgetPolicyResource{Spec: api.BudgetPolicy{
				BudgetRef: "shared-budget",
				RateRef:   "r-user",
				Subjects:  []api.Subject{{Kind: api.SubjectKindUser, Name: "alice"}},
			}}, []api.RateItem{{Resource: resource, Costs: map[string]int64{"input_tokens": userCost}}}),
			paired(api.BudgetPolicyResource{Spec: api.BudgetPolicy{
				BudgetRef: "shared-budget",
				RateRef:   "r-group",
				Subjects:  []api.Subject{{Kind: api.SubjectKindGroup, Name: "team"}},
			}}, []api.RateItem{{Resource: resource, Costs: map[string]int64{"input_tokens": groupCost}}}),
		},
		chargeInserted: []bool{true, false},
	}
	return fs, newFakeKV(1000)
}

// When two policies sharing a budget price the same usage differently, the
// collision carries an ambiguous-pricing warning naming both costs — the
// operator-visible signal that the configuration needs aligned rate_refs.
// Equal costs stay silent: either rate prices the event identically.
func TestIngestWarnsOnAmbiguousPricing(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	fs, kv := overlappingFixture(2, 3) // 20 BU vs 30 BU for the event's 10 tokens
	if code := ingestEvent(t, fs, kv, usageEvent()); code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", code, http.StatusNoContent)
	}
	if !strings.Contains(buf.String(), "ambiguous pricing") {
		t.Errorf("missing ambiguous-pricing warning in log output:\n%s", buf.String())
	}

	buf.Reset()
	fs, kv = overlappingFixture(2, 2) // 20 BU either way
	if code := ingestEvent(t, fs, kv, usageEvent()); code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", code, http.StatusNoContent)
	}
	if strings.Contains(buf.String(), "ambiguous pricing") {
		t.Errorf("unexpected ambiguous-pricing warning for equal costs:\n%s", buf.String())
	}
}
