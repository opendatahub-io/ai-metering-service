package accounting

import (
	"context"
	"fmt"
	"testing"

	"github.com/opendatahub-io/ai-metering-service/internal/admission"
	"github.com/opendatahub-io/ai-metering-service/internal/api"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
)

// fakeStore implements only the resolution path of store.Store; any other
// method panics via the nil embedded interface.
type fakeStore struct {
	store.Store
	policies []api.BudgetPolicyResource
	rates    map[string]*api.RateResource
}

func (f *fakeStore) FindPoliciesWithRates(ctx context.Context, tenantID string, identity api.Identity) ([]api.PolicyWithRates, error) {
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

func policy(id, budgetRef, rateRef string, subjects ...api.Subject) api.BudgetPolicyResource {
	return api.BudgetPolicyResource{
		Metadata: api.Metadata{ID: id, TenantID: "acme-corp"},
		Spec: api.BudgetPolicy{
			BudgetRef: budgetRef,
			RateRef:   rateRef,
			Subjects:  subjects,
		},
	}
}

func rate(id string, items ...api.RateItem) *api.RateResource {
	return &api.RateResource{
		Metadata: api.Metadata{ID: id, TenantID: "acme-corp"},
		Spec:     api.Rate{Name: id, Items: items},
	}
}

func usageEvent(user string, groups []string, org, model string, usage map[string]int64) *api.CloudEvent {
	return &api.CloudEvent{
		ID: "evt-test",
		Data: api.CloudEventData{
			Identity: api.Identity{
				User:         user,
				Groups:       groups,
				Organization: org,
				Tenant:       "acme-corp",
			},
			Resource: map[string]string{"model": model, "provider": "test"},
			Usage:    usage,
		},
	}
}

func chargeKeys(t *testing.T, charges []ResolvedCharge) []string {
	t.Helper()
	var keys []string
	for _, c := range charges {
		keys = append(keys, c.AdmissionKeys...)
	}
	return keys
}

func assertContains(t *testing.T, items []string, want ...string) {
	t.Helper()
	for _, w := range want {
		found := false
		for _, item := range items {
			if item == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected %v to contain %q", items, w)
		}
	}
}

func assertNotContains(t *testing.T, items []string, want ...string) {
	t.Helper()
	for _, w := range want {
		for _, item := range items {
			if item == w {
				t.Fatalf("expected %v to NOT contain %q", items, w)
			}
		}
	}
}

func tenantKey(kind, name string, resource map[string]string) string {
	return admission.BuildAdmissionKey("acme-corp", api.Subject{Kind: api.SubjectKind(kind), Name: name}, resource)
}

// --- ResolveCharges ---

func TestResolveCharges_NoPolicies(t *testing.T) {
	s := &fakeStore{rates: map[string]*api.RateResource{}}
	charges, err := ResolveCharges(context.Background(), s, "acme-corp", usageEvent("alice", nil, "", "claude", map[string]int64{"input_tokens": 10}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(charges) != 0 {
		t.Fatalf("expected no charges, got %+v", charges)
	}
}

func TestResolveCharges_SingleTenantPolicy(t *testing.T) {
	s := &fakeStore{
		policies: []api.BudgetPolicyResource{
			policy("p1", "budget-1", "rate-1", api.Subject{Kind: api.SubjectKindTenant, Name: "acme-corp"}),
		},
		rates: map[string]*api.RateResource{
			"rate-1": rate("rate-1", api.RateItem{
				Resource: map[string]string{"model": "claude"},
				Costs:    map[string]int64{"input_tokens": 3, "output_tokens": 15},
			}),
		},
	}
	event := usageEvent("alice", nil, "", "claude", map[string]int64{"input_tokens": 10, "output_tokens": 4})

	charges, err := ResolveCharges(context.Background(), s, "acme-corp", event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(charges) != 1 {
		t.Fatalf("expected 1 charge, got %d", len(charges))
	}
	if want := int64(10*3 + 4*15); charges[0].BUCost != want {
		t.Fatalf("expected cost %d, got %d", want, charges[0].BUCost)
	}
	if charges[0].Policy.Spec.BudgetRef != "budget-1" {
		t.Fatalf("expected budget-1, got %s", charges[0].Policy.Spec.BudgetRef)
	}
	assertContains(t, chargeKeys(t, charges), tenantKey("tenant", "acme-corp", event.Data.Resource))
}

func TestResolveCharges_RateDoesNotResolve(t *testing.T) {
	s := &fakeStore{
		policies: []api.BudgetPolicyResource{
			policy("p1", "budget-1", "missing-rate", api.Subject{Kind: api.SubjectKindTenant, Name: "acme-corp"}),
		},
		rates: map[string]*api.RateResource{},
	}
	if _, err := ResolveCharges(context.Background(), s, "acme-corp", usageEvent("alice", nil, "", "claude", nil)); err == nil {
		t.Fatal("expected an error for an unresolvable rate_ref")
	}
}

func TestResolveCharges_RateItemMismatchSkipsCharge(t *testing.T) {
	// The policy matches the identity, but its rate covers a different
	// model: the policy contributes no charge and no error.
	s := &fakeStore{
		policies: []api.BudgetPolicyResource{
			policy("p1", "budget-1", "rate-1", api.Subject{Kind: api.SubjectKindTenant, Name: "acme-corp"}),
		},
		rates: map[string]*api.RateResource{
			"rate-1": rate("rate-1", api.RateItem{
				Resource: map[string]string{"model": "granite"},
				Costs:    map[string]int64{"input_tokens": 3},
			}),
		},
	}
	charges, err := ResolveCharges(context.Background(), s, "acme-corp", usageEvent("alice", nil, "", "claude", map[string]int64{"input_tokens": 10}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(charges) != 0 {
		t.Fatalf("expected no charges for a non-matching rate item, got %+v", charges)
	}
}

func TestResolveCharges_MultiplePoliciesChargedIndependently(t *testing.T) {
	// A user policy (BU budget) and a tenant policy (token budget): both
	// match, both charge, each with its own rate and cost.
	s := &fakeStore{
		policies: []api.BudgetPolicyResource{
			policy("p1", "budget-bu", "rate-bu", api.Subject{Kind: api.SubjectKindUser, Name: "alice"}),
			policy("p2", "budget-tokens", "rate-tokens", api.Subject{Kind: api.SubjectKindTenant, Name: "acme-corp"}),
		},
		rates: map[string]*api.RateResource{
			"rate-bu": rate("rate-bu", api.RateItem{
				Resource: map[string]string{},
				Costs:    map[string]int64{"input_tokens": 3, "output_tokens": 15},
			}),
			"rate-tokens": rate("rate-tokens", api.RateItem{
				Resource: map[string]string{},
				Costs:    map[string]int64{"input_tokens": 1, "output_tokens": 1},
			}),
		},
	}
	event := usageEvent("alice", nil, "", "claude", map[string]int64{"input_tokens": 10, "output_tokens": 4})

	charges, err := ResolveCharges(context.Background(), s, "acme-corp", event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(charges) != 2 {
		t.Fatalf("expected 2 charges, got %d", len(charges))
	}

	byBudget := map[string]int64{}
	for _, c := range charges {
		byBudget[c.Policy.Spec.BudgetRef] = c.BUCost
	}
	if byBudget["budget-bu"] != 10*3+4*15 {
		t.Fatalf("budget-bu: expected 90, got %d", byBudget["budget-bu"])
	}
	if byBudget["budget-tokens"] != 10+4 {
		t.Fatalf("budget-tokens: expected 14, got %d", byBudget["budget-tokens"])
	}

	// The user policy writes only the user flag; the tenant policy only the
	// tenant flag — each charge carries the subjects of its own policy.
	keys := chargeKeys(t, charges)
	assertContains(t, keys, tenantKey("user", "alice", event.Data.Resource))
	assertContains(t, keys, tenantKey("tenant", "acme-corp", event.Data.Resource))
}

func TestResolveCharges_MixedSubjectsPartialMatch(t *testing.T) {
	// One policy subjects alice, bob, and the engineering group. Alice (an
	// engineering member) triggers charges keyed to alice and the group;
	// bob's key must not appear.
	s := &fakeStore{
		policies: []api.BudgetPolicyResource{
			policy("p1", "budget-1", "rate-1",
				api.Subject{Kind: api.SubjectKindUser, Name: "alice"},
				api.Subject{Kind: api.SubjectKindUser, Name: "bob"},
				api.Subject{Kind: api.SubjectKindGroup, Name: "engineering"},
			),
		},
		rates: map[string]*api.RateResource{
			"rate-1": rate("rate-1", api.RateItem{
				Resource: map[string]string{},
				Costs:    map[string]int64{"input_tokens": 1},
			}),
		},
	}
	event := usageEvent("alice", []string{"engineering"}, "", "claude", map[string]int64{"input_tokens": 7})

	charges, err := ResolveCharges(context.Background(), s, "acme-corp", event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(charges) != 1 {
		t.Fatalf("expected 1 charge, got %d", len(charges))
	}
	if charges[0].BUCost != 7 {
		t.Fatalf("expected cost 7, got %d", charges[0].BUCost)
	}
	keys := chargeKeys(t, charges)
	assertContains(t, keys,
		tenantKey("user", "alice", event.Data.Resource),
		tenantKey("group", "engineering", event.Data.Resource))
	assertNotContains(t, keys, tenantKey("user", "bob", event.Data.Resource))
}

func TestResolveCharges_GroupAndOrgPolicies(t *testing.T) {
	s := &fakeStore{
		policies: []api.BudgetPolicyResource{
			policy("p1", "budget-group", "rate-1", api.Subject{Kind: api.SubjectKindGroup, Name: "engineering"}),
			policy("p2", "budget-org", "rate-1", api.Subject{Kind: api.SubjectKindOrganization, Name: "research"}),
		},
		rates: map[string]*api.RateResource{
			"rate-1": rate("rate-1", api.RateItem{
				Resource: map[string]string{},
				Costs:    map[string]int64{"input_tokens": 2},
			}),
		},
	}
	event := usageEvent("alice", []string{"engineering"}, "research", "claude", map[string]int64{"input_tokens": 5})

	charges, err := ResolveCharges(context.Background(), s, "acme-corp", event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(charges) != 2 {
		t.Fatalf("expected 2 charges, got %d", len(charges))
	}
	keys := chargeKeys(t, charges)
	assertContains(t, keys,
		tenantKey("group", "engineering", event.Data.Resource),
		tenantKey("organization", "research", event.Data.Resource))
}

func TestResolveCharges_CatchAllVsExactRateItem(t *testing.T) {
	// The rate offers a catch-all item (empty resource map) and an exact
	// item for the event's model; the exact one (more keys, tighter
	// constraint set) must price the charge.
	s := &fakeStore{
		policies: []api.BudgetPolicyResource{
			policy("p1", "budget-1", "rate-1", api.Subject{Kind: api.SubjectKindTenant, Name: "acme-corp"}),
		},
		rates: map[string]*api.RateResource{
			"rate-1": rate("rate-1",
				api.RateItem{
					Resource: map[string]string{},
					Costs:    map[string]int64{"input_tokens": 1},
				},
				api.RateItem{
					Resource: map[string]string{"model": "claude"},
					Costs:    map[string]int64{"input_tokens": 100},
				},
			),
		},
	}
	charges, err := ResolveCharges(context.Background(), s, "acme-corp", usageEvent("alice", nil, "", "claude", map[string]int64{"input_tokens": 1}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(charges) != 1 || charges[0].BUCost != 100 {
		t.Fatalf("expected the exact item to price the charge at 100, got %+v", charges)
	}
}

// --- MatchSubjects ---

func TestMatchSubjects_AllKinds(t *testing.T) {
	subjects := []api.Subject{
		{Kind: api.SubjectKindUser, Name: "alice"},
		{Kind: api.SubjectKindUser, Name: "bob"}, // different user: must not match
		{Kind: api.SubjectKindGroup, Name: "engineering"},
		{Kind: api.SubjectKindGroup, Name: "data-science"}, // not a member: must not match
		{Kind: api.SubjectKindOrganization, Name: "research"},
		{Kind: api.SubjectKindTenant, Name: "acme-corp"},
		{Kind: "workspace", Name: "acme-corp"}, // unknown kind: must not match
	}
	identity := api.Identity{
		User:         "alice",
		Groups:       []string{"engineering", "ml-team"},
		Organization: "research",
		Tenant:       "acme-corp",
	}

	matched := MatchSubjects(subjects, identity)

	want := []api.Subject{
		{Kind: api.SubjectKindUser, Name: "alice"},
		{Kind: api.SubjectKindGroup, Name: "engineering"},
		{Kind: api.SubjectKindOrganization, Name: "research"},
		{Kind: api.SubjectKindTenant, Name: "acme-corp"},
	}
	if len(matched) != len(want) {
		t.Fatalf("expected %d matched subjects, got %d: %+v", len(want), len(matched), matched)
	}
	for i, subject := range matched {
		if subject != want[i] {
			t.Errorf("matched[%d]: expected %+v, got %+v", i, want[i], subject)
		}
	}
}

func TestMatchSubjects_NoMatch(t *testing.T) {
	subjects := []api.Subject{
		{Kind: api.SubjectKindUser, Name: "bob"},
		{Kind: api.SubjectKindGroup, Name: "data-science"},
	}
	identity := api.Identity{
		User:   "alice",
		Groups: []string{"engineering"},
		Tenant: "acme-corp",
	}

	if matched := MatchSubjects(subjects, identity); len(matched) != 0 {
		t.Fatalf("expected no matches, got %+v", matched)
	}
}

// --- MatchRateItem ---

func TestMatchRateItem_ExactMatch(t *testing.T) {
	item := api.RateItem{Resource: map[string]string{"model": "claude", "provider": "anthropic"}, Costs: nil}
	items := []api.RateItem{item}
	if _, ok := MatchRateItem(items, map[string]string{"model": "claude", "provider": "anthropic", "region": "us-east"}); !ok {
		t.Fatal("exact item must match a resource with extra keys")
	}
	if _, ok := MatchRateItem(items, map[string]string{"model": "claude"}); ok {
		t.Fatal("item must not match a resource missing an item key")
	}
	if _, ok := MatchRateItem(items, map[string]string{"model": "granite", "provider": "anthropic"}); ok {
		t.Fatal("item must not match a different value")
	}
}

// A "*" value has no matching semantics: item values compare literally, so a
// stored wildcard (rejected by rate validation, compared literally here as
// defense in depth) matches only a resource literally carrying "*".
func TestMatchRateItem_WildcardValueIsLiteral(t *testing.T) {
	items := []api.RateItem{{Resource: map[string]string{"model": "*", "provider": "anthropic"}}}
	if _, ok := MatchRateItem(items, map[string]string{"model": "claude", "provider": "anthropic"}); ok {
		t.Fatal("literal comparison: a \"*\" value must not match a concrete model")
	}
	if _, ok := MatchRateItem(items, map[string]string{"model": "*", "provider": "anthropic"}); !ok {
		t.Fatal("a resource literally carrying \"*\" matches the literal item")
	}
}

// Among matching items, the one with the most keys — the tightest constraint
// set — prices the event, regardless of list order. This is the fix for the
// wildcard-count tie-break, which could not distinguish a one-key item from a
// two-key item and let list order decide.
func TestMatchRateItem_MostKeysWins(t *testing.T) {
	items := []api.RateItem{
		{Resource: map[string]string{"model": "claude"}},                          // vague, listed first
		{Resource: map[string]string{"model": "claude", "provider": "anthropic"}}, // specific, listed second
		{Resource: map[string]string{}},                                           // catch-all
	}
	item, ok := MatchRateItem(items, map[string]string{"model": "claude", "provider": "anthropic"})
	if !ok {
		t.Fatal("expected a match")
	}
	if len(item.Resource) != 2 {
		t.Fatalf("expected the two-key item to win over the vague one-key item, got %+v", item.Resource)
	}

	// Equal key counts keep the first item in the list.
	tie := []api.RateItem{
		{Resource: map[string]string{"model": "claude", "provider": "anthropic"}, Costs: map[string]int64{"input_tokens": 1}},
		{Resource: map[string]string{"model": "claude", "region": "us-east"}, Costs: map[string]int64{"input_tokens": 2}},
	}
	item, ok = MatchRateItem(tie, map[string]string{"model": "claude", "provider": "anthropic", "region": "us-east"})
	if !ok {
		t.Fatal("expected a match")
	}
	if item.Costs["input_tokens"] != 1 {
		t.Fatalf("expected the first equally-specific item to win, got %+v", item)
	}
}

func TestMatchRateItem_EmptyResourceIsCatchAll(t *testing.T) {
	items := []api.RateItem{
		{Resource: map[string]string{"model": "granite"}},
		{Resource: map[string]string{}}, // matches any resource
	}
	item, ok := MatchRateItem(items, map[string]string{"model": "claude"})
	if !ok {
		t.Fatal("empty-resource item must match")
	}
	if len(item.Resource) != 0 {
		t.Fatalf("expected the empty-resource item (the only match), got %+v", item.Resource)
	}

	// When a concrete item also matches, its tighter constraint set wins
	// over the catch-all.
	both := []api.RateItem{
		{Resource: map[string]string{}},
		{Resource: map[string]string{"model": "claude"}},
	}
	item, ok = MatchRateItem(both, map[string]string{"model": "claude"})
	if !ok {
		t.Fatal("expected a match")
	}
	if item.Resource["model"] != "claude" {
		t.Fatalf("expected the one-key item to beat the catch-all, got %+v", item.Resource)
	}
}

// --- ComputeBUCost ---

func TestComputeBUCost(t *testing.T) {
	usage := map[string]int64{"input_tokens": 10, "output_tokens": 4, "unrelated": 100}
	costs := map[string]int64{"input_tokens": 3, "output_tokens": 15, "unused_key": 7}

	if got := ComputeBUCost(usage, costs); got != 10*3+4*15 {
		t.Fatalf("expected %d, got %d", 10*3+4*15, got)
	}
	if got := ComputeBUCost(map[string]int64{}, costs); got != 0 {
		t.Fatalf("expected 0 for empty usage, got %d", got)
	}
	if got := ComputeBUCost(usage, map[string]int64{}); got != 0 {
		t.Fatalf("expected 0 for empty costs, got %d", got)
	}
	if got := ComputeBUCost(nil, nil); got != 0 {
		t.Fatalf("expected 0 for nil maps, got %d", got)
	}
}
