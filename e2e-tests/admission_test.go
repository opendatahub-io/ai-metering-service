package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/redhat-et/pricetag-metering/internal/admission"
	"github.com/redhat-et/pricetag-metering/internal/api"
	goredis "github.com/redis/go-redis/v9"
)

// These tests verify the data-plane state the Metering Service derives from
// ingested events: the Redis balance counters and the admission flags. They
// run against the same stack as the rest of the e2e suite (service on
// METERING_SERVICE_URL, Redis on METERING_REDIS_URL).

func redisURL() string {
	if u := os.Getenv("METERING_REDIS_URL"); u != "" {
		return u
	}
	return "redis://localhost:6379/0"
}

func redisClient(t *testing.T) *goredis.Client {
	t.Helper()
	opts, err := goredis.ParseURL(redisURL())
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	client := goredis.NewClient(opts)
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// cleanupRedis drops every ai-metering:* key. It runs after the suite so the
// stack is left clean even though the API's budget delete does not (yet)
// remove balance and admission keys. Best effort: an unreachable Redis is
// logged, not fatal, so the suite result is preserved.
func cleanupRedis() {
	opts, err := goredis.ParseURL(redisURL())
	if err != nil {
		log.Printf("cleanup redis: %v", err)
		return
	}
	client := goredis.NewClient(opts)
	defer client.Close()

	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		log.Printf("cleanup redis: %v", err)
		return
	}

	deleted := 0
	var cursor uint64
	for {
		keys, next, err := client.Scan(ctx, cursor, "ai-metering:*", 100).Result()
		if err != nil {
			log.Printf("cleanup redis: %v", err)
			return
		}
		if len(keys) > 0 {
			if err := client.Del(ctx, keys...).Err(); err != nil {
				log.Printf("cleanup redis: %v", err)
				return
			}
			deleted += len(keys)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	log.Printf("cleanup redis: deleted %d keys", deleted)
}

func redisGet(t *testing.T, client *goredis.Client, key string) string {
	t.Helper()
	val, err := client.Get(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return val
}

func jsonUnmarshal(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}

type budgetSpec struct {
	limit   int64
	overage map[string]any
}

// admissionTest drives one scenario against a uniquely-named model, so each
// test's resource hash — and therefore its admission keys — cannot collide
// with other tests or previous runs.
type admissionTest struct {
	budgetIDs []string
	rateID    string
	client    *goredis.Client
	resource  map[string]string
}

// newAdmissionTest creates a rate for the given model and one budget +
// tenant-subject policy per budget spec, all matching that model.
func newAdmissionTest(t *testing.T, model string, budgets ...budgetSpec) *admissionTest {
	t.Helper()
	return newAdmissionTestForSubjects(t, model,
		[]map[string]string{{"kind": "tenant", "name": tenant}}, budgets...)
}

// newAdmissionTestForSubjects is newAdmissionTest with explicit policy
// subjects; every budget gets a policy subjecting all of them.
func newAdmissionTestForSubjects(t *testing.T, model string, subjects []map[string]string, budgets ...budgetSpec) *admissionTest {
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

	at := &admissionTest{client: redisClient(t), resource: resource, rateID: rateID}
	for i, b := range budgets {
		resp = doRequest(t, "POST", "/api/v1/budgets", map[string]any{
			"name":            fmt.Sprintf("%s-budget-%d", model, i),
			"limit":           b.limit,
			"period_start":    "2026-11-01T00:00:00Z",
			"period_duration": "P1M",
			"renewal_type":    "drain",
		})
		requireStatus(t, resp, http.StatusCreated)
		var budgetCreated map[string]any
		jsonUnmarshal(t, readBody(t, resp), &budgetCreated)
		budgetID := budgetCreated["metadata"].(map[string]any)["id"].(string)

		policy := map[string]any{
			"budget_ref": budgetID,
			"rate_ref":   rateID,
			"subjects":   subjects,
		}
		if b.overage != nil {
			policy["overage"] = b.overage
		}
		resp = doRequest(t, "POST", "/api/v1/budget_policies", policy)
		requireStatus(t, resp, http.StatusCreated)

		at.budgetIDs = append(at.budgetIDs, budgetID)
	}

	// Clear the flags this test may write so assertions start from a known
	// state regardless of what previous runs left behind.
	at.clearFlag("tenant", tenant)
	at.clearFlag("user", "alice")
	at.clearFlag("user", "bob")
	at.clearFlag("user", "carol")
	at.clearFlag("group", "engineering")
	at.clearFlag("group", "data-science")
	at.clearFlag("organization", "research")
	return at
}

func (at *admissionTest) balanceKey(i int) string {
	return admission.BuildBalanceKey(tenant, at.budgetIDs[i])
}

func (at *admissionTest) flagKeyFor(kind, name string) string {
	return admission.BuildAdmissionKey(tenant, api.Subject{Kind: api.SubjectKind(kind), Name: name}, at.resource)
}

func (at *admissionTest) clearFlag(kind, name string) {
	at.client.Del(context.Background(), at.flagKeyFor(kind, name))
}

func (at *admissionTest) ingestEvent(t *testing.T, eventID string, identity map[string]any, resource map[string]string, inputTokens int64) {
	t.Helper()
	// Event time is now-based so usage queries with relative ranges (e.g.
	// range=30d, whose upper bound is the current time) always include it.
	resp := doRequest(t, "POST", "/api/v1/events", map[string]any{
		"specversion":     "1.0",
		"id":              eventID,
		"source":          "praxis/e2e-test",
		"type":            "io.opendatahub.aigateway.usage.v1",
		"time":            time.Now().UTC().Format(time.RFC3339),
		"datacontenttype": "application/json",
		"data": map[string]any{
			"request_id": "req-" + eventID,
			"identity":   identity,
			"resource":   resource,
			"usage":      map[string]int64{"input_tokens": inputTokens},
		},
	})
	requireStatus(t, resp, http.StatusNoContent)
}

func (at *admissionTest) ingestAs(t *testing.T, eventID string, identity map[string]any, inputTokens int64) {
	t.Helper()
	at.ingestEvent(t, eventID, identity, at.resource, inputTokens)
}

func (at *admissionTest) ingest(t *testing.T, eventID string, inputTokens int64) {
	t.Helper()
	at.ingestAs(t, eventID, map[string]any{"user": "alice", "tenant": tenant}, inputTokens)
}

func (at *admissionTest) expectBalance(t *testing.T, i int, want int64) {
	t.Helper()
	key := at.balanceKey(i)
	if got := redisGet(t, at.client, key); got != fmt.Sprintf("%d", want) {
		t.Fatalf("balance %s: expected %d, got %s", key, want, got)
	}
}

func (at *admissionTest) expectFlag(t *testing.T, want string) {
	t.Helper()
	at.expectFlagFor(t, "tenant", tenant, want)
}

func (at *admissionTest) expectFlagFor(t *testing.T, kind, name, want string) {
	t.Helper()
	key := at.flagKeyFor(kind, name)
	if got := redisGet(t, at.client, key); got != want {
		t.Fatalf("admission flag %s: expected %s, got %s", key, want, got)
	}
}

func (at *admissionTest) expectFlagAbsent(t *testing.T, kind, name string) {
	t.Helper()
	key := at.flagKeyFor(kind, name)
	err := at.client.Get(context.Background(), key).Err()
	if err == nil {
		t.Fatalf("admission flag %s: expected absent, got %q", key, redisGet(t, at.client, key))
	}
	if !errors.Is(err, goredis.Nil) {
		t.Fatalf("admission flag %s: expected absent, got error %v", key, err)
	}
}

// expectUsageCount asserts how many usage events were recorded for the given
// model — 0 means an unmatched event left no trace in the ledger.
func expectUsageCount(t *testing.T, model string, want int) {
	t.Helper()
	resp := doRequest(t, "GET", "/api/v1/usage?range=30d&group_by=model", nil)
	requireStatus(t, resp, http.StatusOK)
	var result struct {
		Data []struct {
			Dimensions map[string]string `json:"dimensions"`
			Requests   int               `json:"requests"`
		} `json:"data"`
	}
	jsonUnmarshal(t, readBody(t, resp), &result)
	for _, row := range result.Data {
		if row.Dimensions["model"] == model {
			if row.Requests != want {
				t.Fatalf("usage for model %s: expected %d requests, got %d", model, want, row.Requests)
			}
			return
		}
	}
	if want != 0 {
		t.Fatalf("expected %d usage rows for model %s, found none", want, model)
	}
}

func TestAdmission_BalanceAndFlags(t *testing.T) {
	at := newAdmissionTest(t, "admission-balance", budgetSpec{limit: 1000}) // hard enforcement

	// One event, cost 300: balance 1000-300=700, still allowed.
	at.ingest(t, "evt-adm-001", 100)
	at.expectBalance(t, 0, 700)
	at.expectFlag(t, "allow")

	// Redelivering the same event must not charge twice.
	at.ingest(t, "evt-adm-001", 100)
	at.expectBalance(t, 0, 700)

	// An event that exhausts the budget: balance 700-1200=-500, hard deny.
	at.ingest(t, "evt-adm-002", 400)
	at.expectBalance(t, 0, -500)
	at.expectFlag(t, "deny")

	// Redelivery while in overage must not deepen it.
	at.ingest(t, "evt-adm-002", 400)
	at.expectBalance(t, 0, -500)

	// Topping up the budget raises the ceiling going forward: the 1500
	// already spent this period still counts, so the balance becomes
	// 5000-1500=3500 — and the deny flag lifts immediately, without
	// waiting for the next accounted event.
	resp := doRequest(t, "PUT", "/api/v1/budgets/"+at.budgetIDs[0], map[string]any{
		"name":            "admission-balance-budget-0",
		"limit":           5000,
		"period_start":    "2026-11-01T00:00:00Z",
		"period_duration": "P1M",
		"renewal_type":    "drain",
	})
	requireStatus(t, resp, http.StatusOK)

	at.expectBalance(t, 0, 5000-1500)
	at.expectFlag(t, "allow")

	at.ingest(t, "evt-adm-003", 10)
	at.expectBalance(t, 0, 5000-1500-30)
	at.expectFlag(t, "allow")
}

func TestAdmission_SoftOverage(t *testing.T) {
	at := newAdmissionTest(t, "admission-soft", budgetSpec{limit: 100, overage: map[string]any{"type": "soft"}})

	// Cost 600 against a 100 budget: balance -500, soft overage, not deny.
	at.ingest(t, "evt-soft-001", 200)
	at.expectBalance(t, 0, -500)
	at.expectFlag(t, "over")
}

func TestAdmission_MultiBudgetFold(t *testing.T) {
	// Two budgets charge the same events through two tenant-level policies
	// on the same model. Both exhaust; the shared (tenant, resource) flag
	// must fold to the hard budget's deny, not the soft budget's over.
	at := newAdmissionTest(t, "admission-fold",
		budgetSpec{limit: 100},                                          // hard
		budgetSpec{limit: 100, overage: map[string]any{"type": "soft"}}, // soft
	)

	// Cost 1200 against two 100-limit budgets: both go to -1100.
	at.ingest(t, "evt-fold-001", 400)
	at.expectBalance(t, 0, -1100)
	at.expectBalance(t, 1, -1100)
	at.expectFlag(t, "deny")

	// Redelivery must not double-charge either budget.
	at.ingest(t, "evt-fold-001", 400)
	at.expectBalance(t, 0, -1100)
	at.expectBalance(t, 1, -1100)
}

func TestAdmission_FoldPartialExhaustion(t *testing.T) {
	// Hard budget healthy, soft budget exhausted: the fold is over.
	// Once the hard budget also exhausts: the fold is deny.
	at := newAdmissionTest(t, "admission-fold-partial",
		budgetSpec{limit: 10000},                                         // hard
		budgetSpec{limit: 100, overage: map[string]any{"type": "soft"}}, // soft
	)

	// Cost 600: hard 10000-600=9400 (allow), soft 100-600=-500 (over) → over.
	at.ingest(t, "evt-fp-001", 200)
	at.expectBalance(t, 0, 9400)
	at.expectBalance(t, 1, -500)
	at.expectFlag(t, "over")

	// Cost 12000: hard 9400-12000=-2600 (deny), soft deeper (over) → deny.
	at.ingest(t, "evt-fp-002", 4000)
	at.expectBalance(t, 0, -2600)
	at.expectFlag(t, "deny")
}

func TestAdmission_ExactLimitBoundary(t *testing.T) {
	// A charge that lands the balance exactly on zero is still allowed;
	// only a negative balance flips the flag.
	at := newAdmissionTest(t, "admission-boundary", budgetSpec{limit: 300})

	at.ingest(t, "evt-bnd-001", 100) // cost 300 → balance exactly 0
	at.expectBalance(t, 0, 0)
	at.expectFlag(t, "allow")

	at.ingest(t, "evt-bnd-002", 1) // cost 3 → balance -3
	at.expectBalance(t, 0, -3)
	at.expectFlag(t, "deny")
}

func TestAdmission_ZeroCost(t *testing.T) {
	// A zero-cost event still settles: charge row written, flag computed.
	at := newAdmissionTest(t, "admission-zerocost", budgetSpec{limit: 100})

	at.ingest(t, "evt-zc-001", 0) // cost 0
	at.expectBalance(t, 0, 100)
	at.expectFlag(t, "allow")
	expectUsageCount(t, "admission-zerocost", 1)
}

func TestAdmission_MostSpecificWins(t *testing.T) {
	// Alice has a direct user policy (small, hard budget) on top of a
	// tenant-wide policy (large budget). Her user flag is the answer for
	// her; the tenant flag is written separately for users without one.
	at := newAdmissionTest(t, "admission-specificity",
		budgetSpec{limit: 10000}, // tenant-level, stays healthy
	)

	// Add a user-level policy with its own small budget on the same model.
	resp := doRequest(t, "POST", "/api/v1/budgets", map[string]any{
		"name":            "admission-specificity-alice",
		"limit":           100,
		"period_start":    "2026-11-01T00:00:00Z",
		"period_duration": "P1M",
		"renewal_type":    "drain",
	})
	requireStatus(t, resp, http.StatusCreated)
	var budgetCreated map[string]any
	jsonUnmarshal(t, readBody(t, resp), &budgetCreated)
	aliceBudget := budgetCreated["metadata"].(map[string]any)["id"].(string)

	resp = doRequest(t, "POST", "/api/v1/budget_policies", map[string]any{
		"budget_ref": aliceBudget,
		"rate_ref":   at.rateID,
		"subjects":   []map[string]string{{"kind": "user", "name": "alice"}},
	})
	requireStatus(t, resp, http.StatusCreated)

	// Alice's event charges both budgets; her user budget exhausts, so her
	// user flag is deny while the tenant flag stays allow.
	at.ingest(t, "evt-spec-001", 200) // cost 600
	aliceKey := admission.BuildBalanceKey(tenant, aliceBudget)
	if got := redisGet(t, at.client, aliceKey); got != "-500" {
		t.Fatalf("alice balance: expected -500, got %s", got)
	}
	at.expectBalance(t, 0, 10000-600)
	at.expectFlagFor(t, "user", "alice", "deny")
	at.expectFlagFor(t, "tenant", tenant, "allow")

	// Bob has no user policy: no user flag is written for him, and the
	// tenant flag governs him at read time (crate falls through).
	at.ingestAs(t, "evt-spec-002", map[string]any{"user": "bob", "tenant": tenant}, 10)
	at.expectFlagAbsent(t, "user", "bob")
	at.expectFlagFor(t, "tenant", tenant, "allow")
}

func TestAdmission_GroupSubject(t *testing.T) {
	// A group-subject policy charges members and writes the group flag;
	// non-members are untouched.
	at := newAdmissionTestForSubjects(t, "admission-group",
		[]map[string]string{{"kind": "group", "name": "engineering"}},
		budgetSpec{limit: 100},
	)

	// Alice is in engineering: charged, group flag written.
	at.ingestAs(t, "evt-grp-001", map[string]any{
		"user": "alice", "groups": []string{"engineering"}, "tenant": tenant,
	}, 200) // cost 600 → balance -500
	at.expectBalance(t, 0, -500)
	at.expectFlagFor(t, "group", "engineering", "deny")
	at.expectFlagAbsent(t, "user", "alice") // no user-level policy exists

	// Bob is in another group: no policy matches, nothing charged.
	at.ingestAs(t, "evt-grp-002", map[string]any{
		"user": "bob", "groups": []string{"data-science"}, "tenant": tenant,
	}, 200)
	at.expectBalance(t, 0, -500) // unchanged
	at.expectFlagAbsent(t, "group", "data-science")
	// Only alice's member event is in the ledger; bob's did not add a row.
	expectUsageCount(t, "admission-group", 1)
}

func TestAdmission_OrganizationSubject(t *testing.T) {
	at := newAdmissionTestForSubjects(t, "admission-org",
		[]map[string]string{{"kind": "organization", "name": "research"}},
		budgetSpec{limit: 10000},
	)

	at.ingestAs(t, "evt-org-001", map[string]any{
		"user": "alice", "organization": "research", "tenant": tenant,
	}, 100) // cost 300
	at.expectBalance(t, 0, 9700)
	// The key segment is the full "organization" spelling.
	at.expectFlagFor(t, "organization", "research", "allow")

	// An event without an organization does not match the policy.
	at.ingestAs(t, "evt-org-002", map[string]any{"user": "bob", "tenant": tenant}, 100)
	at.expectBalance(t, 0, 9700) // unchanged
}

func TestAdmission_MultiSubjectPolicy(t *testing.T) {
	// One policy subjecting both alice and the engineering group: an event
	// from alice (a member) writes both flags from the same single charge.
	at := newAdmissionTestForSubjects(t, "admission-multisubject",
		[]map[string]string{
			{"kind": "user", "name": "alice"},
			{"kind": "group", "name": "engineering"},
		},
		budgetSpec{limit: 10000},
	)

	at.ingestAs(t, "evt-ms-001", map[string]any{
		"user": "alice", "groups": []string{"engineering"}, "tenant": tenant,
	}, 100) // cost 300, charged once
	at.expectBalance(t, 0, 9700)
	at.expectFlagFor(t, "user", "alice", "allow")
	at.expectFlagFor(t, "group", "engineering", "allow")
}

func TestAdmission_NoMatchingPolicy(t *testing.T) {
	// The policy subjects carol; alice's event matches nothing and must
	// leave no trace: no usage row, no flags, no balance change.
	at := newAdmissionTestForSubjects(t, "admission-nomatch",
		[]map[string]string{{"kind": "user", "name": "carol"}},
		budgetSpec{limit: 100},
	)

	at.ingest(t, "evt-nm-001", 100)
	at.expectBalance(t, 0, 100) // untouched
	at.expectFlagAbsent(t, "user", "alice")
	expectUsageCount(t, "admission-nomatch", 0)
}

func TestAdmission_RateItemMismatch(t *testing.T) {
	// The policy matches the identity, but its rate has no item for the
	// event's model: no charge, no flags, no ledger row.
	at := newAdmissionTest(t, "admission-ratemismatch", budgetSpec{limit: 100})

	otherResource := map[string]string{"model": "admission-ratemismatch-other", "provider": "test"}
	at.ingestEvent(t, "evt-rm-001", map[string]any{"user": "alice", "tenant": tenant}, otherResource, 100)

	at.expectBalance(t, 0, 100) // untouched
	at.expectFlagAbsent(t, "tenant", tenant)
	expectUsageCount(t, "admission-ratemismatch", 0)
}
