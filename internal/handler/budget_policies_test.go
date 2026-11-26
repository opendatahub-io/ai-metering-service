package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opendatahub-io/ai-metering-service/internal/api"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
)

// policyStore serves the budget-policy validation tests: GetBudget and
// GetRate resolve everything, FindPoliciesForBudget returns the configured
// set, and the write methods echo the policy back with an id.
type policyStore struct {
	store.Store
	byBudget map[string][]api.BudgetPolicyResource
	nextID   int
}

func (s *policyStore) GetBudget(ctx context.Context, tenantID, id string) (*api.BudgetResource, error) {
	return &api.BudgetResource{}, nil
}

func (s *policyStore) GetRate(ctx context.Context, tenantID, id string) (*api.RateResource, error) {
	return &api.RateResource{}, nil
}

func (s *policyStore) FindPoliciesForBudget(ctx context.Context, tenantID, budgetID string) ([]api.BudgetPolicyResource, error) {
	return s.byBudget[budgetID], nil
}

func (s *policyStore) CreateBudgetPolicy(ctx context.Context, tenantID string, policy *api.BudgetPolicy) (*api.BudgetPolicyResource, error) {
	s.nextID++
	return &api.BudgetPolicyResource{Metadata: api.Metadata{ID: fmt.Sprintf("p%d", s.nextID)}, Spec: *policy}, nil
}

func (s *policyStore) UpdateBudgetPolicy(ctx context.Context, tenantID, id string, policy *api.BudgetPolicy) (*api.BudgetPolicyResource, error) {
	return &api.BudgetPolicyResource{Metadata: api.Metadata{ID: id}, Spec: *policy}, nil
}

func submitPolicy(t *testing.T, ps *policyStore, method, path string, policy api.BudgetPolicy) int {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := NewBudgetPoliciesHandler(ps)
	router.POST("/policies", RequireTenant(), h.Create)
	router.PUT("/policies/:id", RequireTenant(), h.Update)

	body, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant", "acme-corp")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Code
}

func existingPolicy(id, budgetRef, rateRef string, subjects ...api.Subject) api.BudgetPolicyResource {
	return api.BudgetPolicyResource{
		Metadata: api.Metadata{ID: id},
		Spec:     api.BudgetPolicy{BudgetRef: budgetRef, RateRef: rateRef, Subjects: subjects},
	}
}

func subj(kind api.SubjectKind, name string) api.Subject {
	return api.Subject{Kind: kind, Name: name}
}

// The literal case the ADR's creation rule names: the same subject entry in
// two policies referencing one budget with different rate_refs is rejected.
func TestCreatePolicyRejectsSharedSubjectDifferentRate(t *testing.T) {
	ps := &policyStore{byBudget: map[string][]api.BudgetPolicyResource{
		"shared": {existingPolicy("p1", "shared", "r1", subj(api.SubjectKindUser, "alice"))},
	}}
	new := api.BudgetPolicy{BudgetRef: "shared", RateRef: "r2", Subjects: []api.Subject{subj(api.SubjectKindUser, "alice")}}
	if code := submitPolicy(t, ps, http.MethodPost, "/policies", new); code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (shared subject, different rate)", code, http.StatusBadRequest)
	}
}

// The same shared subject with the same rate_ref is fine: any overlap
// prices the event identically.
func TestCreatePolicyAllowsSharedSubjectSameRate(t *testing.T) {
	ps := &policyStore{byBudget: map[string][]api.BudgetPolicyResource{
		"shared": {existingPolicy("p1", "shared", "r1", subj(api.SubjectKindUser, "alice"))},
	}}
	new := api.BudgetPolicy{BudgetRef: "shared", RateRef: "r1", Subjects: []api.Subject{subj(api.SubjectKindUser, "alice")}}
	if code := submitPolicy(t, ps, http.MethodPost, "/policies", new); code != http.StatusCreated {
		t.Errorf("status = %d, want %d (shared subject, same rate)", code, http.StatusCreated)
	}
}

// A live tenant subject matches every identity in the tenant, so it
// conflicts with any other policy for the same budget at a different rate —
// in both directions.
func TestCreatePolicyRejectsTenantSubjectOverlap(t *testing.T) {
	newTenant := api.BudgetPolicy{BudgetRef: "shared", RateRef: "r2",
		Subjects: []api.Subject{subj(api.SubjectKindTenant, "acme-corp")}}
	existingUser := &policyStore{byBudget: map[string][]api.BudgetPolicyResource{
		"shared": {existingPolicy("p1", "shared", "r1", subj(api.SubjectKindUser, "bob"))},
	}}
	if code := submitPolicy(t, existingUser, http.MethodPost, "/policies", newTenant); code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (new tenant subject vs existing user policy)", code, http.StatusBadRequest)
	}

	newUser := api.BudgetPolicy{BudgetRef: "shared", RateRef: "r2",
		Subjects: []api.Subject{subj(api.SubjectKindUser, "bob")}}
	existingTenant := &policyStore{byBudget: map[string][]api.BudgetPolicyResource{
		"shared": {existingPolicy("p1", "shared", "r1", subj(api.SubjectKindTenant, "acme-corp"))},
	}}
	if code := submitPolicy(t, existingTenant, http.MethodPost, "/policies", newUser); code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (new user policy vs existing tenant subject)", code, http.StatusBadRequest)
	}
}

// Cross-level overlaps (user vs group) and dead tenant subjects (naming a
// different tenant) are not provable at write time — creation must not
// reject them; ingest warns if they ever collide with different costs.
func TestCreatePolicyAllowsUndetectableOverlaps(t *testing.T) {
	ps := &policyStore{byBudget: map[string][]api.BudgetPolicyResource{
		"shared": {existingPolicy("p1", "shared", "r1", subj(api.SubjectKindUser, "alice"))},
	}}

	userVsGroup := api.BudgetPolicy{BudgetRef: "shared", RateRef: "r2",
		Subjects: []api.Subject{subj(api.SubjectKindGroup, "team")}}
	if code := submitPolicy(t, ps, http.MethodPost, "/policies", userVsGroup); code != http.StatusCreated {
		t.Errorf("status = %d, want %d (user vs group: membership unknown at write time)", code, http.StatusCreated)
	}

	deadTenant := api.BudgetPolicy{BudgetRef: "shared", RateRef: "r2",
		Subjects: []api.Subject{subj(api.SubjectKindTenant, "other-corp")}}
	if code := submitPolicy(t, ps, http.MethodPost, "/policies", deadTenant); code != http.StatusCreated {
		t.Errorf("status = %d, want %d (tenant subject naming another tenant never matches)", code, http.StatusCreated)
	}
}

// An update changing the rate_ref must not trip on the policy being
// updated — only on genuinely other policies.
func TestUpdatePolicyExcludesSelf(t *testing.T) {
	ps := &policyStore{byBudget: map[string][]api.BudgetPolicyResource{
		"shared": {existingPolicy("p1", "shared", "r1", subj(api.SubjectKindUser, "alice"))},
	}}
	update := api.BudgetPolicy{BudgetRef: "shared", RateRef: "r2", Subjects: []api.Subject{subj(api.SubjectKindUser, "alice")}}
	if code := submitPolicy(t, ps, http.MethodPut, "/policies/p1", update); code != http.StatusOK {
		t.Errorf("status = %d, want %d (self excluded from the conflicting-rate check)", code, http.StatusOK)
	}
}
