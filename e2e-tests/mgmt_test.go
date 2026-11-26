package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"testing"
)

const (
	defaultBaseURL = "http://localhost:8080"
	tenant         = "acme-corp"
)

func baseURL() string {
	if u := os.Getenv("METERING_SERVICE_URL"); u != "" {
		return u
	}
	return defaultBaseURL
}

func TestMain(m *testing.M) {
	cleanup()
	code := m.Run()
	// Leave the stack as we found it: delete the objects created by the
	// suite via the API, then drop any Redis state the API deletes cannot
	// reach (balance counters and admission flags).
	cleanup()
	cleanupRedis()
	os.Exit(code)
}

func cleanup() {
	log.Println("cleaning up test data before suite run...")

	cleanupUsage()
	cleanupResource("/api/v1/budget_policies")
	cleanupResource("/api/v1/budgets")
	cleanupResource("/api/v1/rates")

	log.Println("cleanup complete")
}

func cleanupUsage() {
	req, _ := http.NewRequest("DELETE", baseURL()+"/api/v1/usage", nil)
	req.Header.Set("X-Tenant", tenant)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("cleanup usage: %v (is the service running with --debug?)", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		var result map[string]any
		json.Unmarshal(body, &result)
		log.Printf("deleted %v usage records", result["deleted"])
	} else {
		log.Printf("cleanup usage: status %d (service may not be running with --debug)", resp.StatusCode)
	}
}

func cleanupResource(path string) {
	req, _ := http.NewRequest("GET", baseURL()+path+"?limit=1000", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant", tenant)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("cleanup %s: %v", path, err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var listing map[string]any
	json.Unmarshal(body, &listing)

	items, ok := listing["items"].([]any)
	if !ok || len(items) == 0 {
		log.Printf("%s: nothing to delete", path)
		return
	}

	for _, item := range items {
		id := item.(map[string]any)["metadata"].(map[string]any)["id"].(string)
		req, _ := http.NewRequest("DELETE", baseURL()+path+"/"+id, nil)
		req.Header.Set("X-Tenant", tenant)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("cleanup %s/%s: %v", path, id, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			log.Printf("deleted %s/%s", path, id)
		}
	}
}

func doRequest(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()
	return doRequestTenant(t, method, path, body, tenant)
}

func doRequestTenant(t *testing.T, method, path string, body any, tenantID string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL()+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant", tenantID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

func requireStatus(t *testing.T, resp *http.Response, expected int) {
	t.Helper()
	if resp.StatusCode != expected {
		body := readBody(t, resp)
		t.Fatalf("expected status %d, got %d: %s", expected, resp.StatusCode, string(body))
	}
}

// --- Rates ---

func TestRates_CRUD(t *testing.T) {
	rate := map[string]any{
		"name": "standard",
		"items": []map[string]any{
			{
				"resource": map[string]string{"model": "claude-sonnet-5-5", "provider": "anthropic"},
				"costs":    map[string]int64{"input_tokens": 3, "output_tokens": 15, "cache_read_tokens": 1, "cache_write_tokens": 4, "reasoning_tokens": 15},
			},
		},
	}

	// Create
	resp := doRequest(t, "POST", "/api/v1/rates", rate)
	requireStatus(t, resp, http.StatusCreated)
	var created map[string]any
	json.Unmarshal(readBody(t, resp), &created)
	id := created["metadata"].(map[string]any)["id"].(string)
	t.Logf("created rate id=%s", id)

	// Get
	resp = doRequest(t, "GET", "/api/v1/rates/"+id, nil)
	requireStatus(t, resp, http.StatusOK)
	var fetched map[string]any
	json.Unmarshal(readBody(t, resp), &fetched)
	if fetched["spec"].(map[string]any)["name"] != "standard" {
		t.Fatalf("expected name 'standard', got %v", fetched["spec"].(map[string]any)["name"])
	}

	// List
	resp = doRequest(t, "GET", "/api/v1/rates", nil)
	requireStatus(t, resp, http.StatusOK)
	var listing map[string]any
	json.Unmarshal(readBody(t, resp), &listing)
	if listing["total"].(float64) < 1 {
		t.Fatal("expected at least 1 rate in listing")
	}

	// Update
	rate["name"] = "standard-updated"
	resp = doRequest(t, "PUT", "/api/v1/rates/"+id, rate)
	requireStatus(t, resp, http.StatusOK)
	var updated map[string]any
	json.Unmarshal(readBody(t, resp), &updated)
	if updated["spec"].(map[string]any)["name"] != "standard-updated" {
		t.Fatalf("expected updated name, got %v", updated["spec"].(map[string]any)["name"])
	}

	// Delete
	resp = doRequest(t, "DELETE", "/api/v1/rates/"+id, nil)
	requireStatus(t, resp, http.StatusNoContent)

	// Verify deleted
	resp = doRequest(t, "GET", "/api/v1/rates/"+id, nil)
	requireStatus(t, resp, http.StatusNotFound)
}

func TestRates_MultipleModels(t *testing.T) {
	rate := map[string]any{
		"name": "multi-model",
		"items": []map[string]any{
			{
				"resource": map[string]string{"model": "claude-sonnet-5-5", "provider": "anthropic"},
				"costs":    map[string]int64{"input_tokens": 3, "output_tokens": 15},
			},
			{
				"resource": map[string]string{"model": "granite-3.3-8b", "provider": "ibm"},
				"costs":    map[string]int64{"input_tokens": 1, "output_tokens": 5},
			},
		},
	}

	resp := doRequest(t, "POST", "/api/v1/rates", rate)
	requireStatus(t, resp, http.StatusCreated)

	var created map[string]any
	json.Unmarshal(readBody(t, resp), &created)
	id := created["metadata"].(map[string]any)["id"].(string)

	resp = doRequest(t, "GET", "/api/v1/rates/"+id, nil)
	requireStatus(t, resp, http.StatusOK)
	var fetched map[string]any
	json.Unmarshal(readBody(t, resp), &fetched)
	items := fetched["spec"].(map[string]any)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 rate items, got %d", len(items))
	}
}

// --- Budgets ---

func TestBudgets_CRUD(t *testing.T) {
	budget := map[string]any{
		"name":            "monthly-100k",
		"limit":           100000,
		"period_start":    "2026-11-01T00:00:00Z",
		"period_duration": "P1M",
		"renewal_type":    "drain",
	}

	// Create
	resp := doRequest(t, "POST", "/api/v1/budgets", budget)
	requireStatus(t, resp, http.StatusCreated)
	var created map[string]any
	json.Unmarshal(readBody(t, resp), &created)
	id := created["metadata"].(map[string]any)["id"].(string)
	t.Logf("created budget id=%s", id)

	// Get
	resp = doRequest(t, "GET", "/api/v1/budgets/"+id, nil)
	requireStatus(t, resp, http.StatusOK)
	var fetched map[string]any
	json.Unmarshal(readBody(t, resp), &fetched)
	spec := fetched["spec"].(map[string]any)
	if spec["name"] != "monthly-100k" {
		t.Fatalf("expected name 'monthly-100k', got %v", spec["name"])
	}
	if int64(spec["limit"].(float64)) != 100000 {
		t.Fatalf("expected limit 100000, got %v", spec["limit"])
	}

	// List
	resp = doRequest(t, "GET", "/api/v1/budgets", nil)
	requireStatus(t, resp, http.StatusOK)
	var listing map[string]any
	json.Unmarshal(readBody(t, resp), &listing)
	if listing["total"].(float64) < 1 {
		t.Fatal("expected at least 1 budget in listing")
	}

	// Update
	budget["limit"] = 200000
	resp = doRequest(t, "PUT", "/api/v1/budgets/"+id, budget)
	requireStatus(t, resp, http.StatusOK)
	var updated map[string]any
	json.Unmarshal(readBody(t, resp), &updated)
	if int64(updated["spec"].(map[string]any)["limit"].(float64)) != 200000 {
		t.Fatalf("expected updated limit 200000, got %v", updated["spec"].(map[string]any)["limit"])
	}

	// Delete
	resp = doRequest(t, "DELETE", "/api/v1/budgets/"+id, nil)
	requireStatus(t, resp, http.StatusNoContent)

	// Verify deleted
	resp = doRequest(t, "GET", "/api/v1/budgets/"+id, nil)
	requireStatus(t, resp, http.StatusNotFound)
}

func TestBudgets_Rollover(t *testing.T) {
	maxCarry := int64(50000)
	budget := map[string]any{
		"name":            "quarterly-rollover",
		"limit":           500000,
		"period_start":    "2026-10-01T00:00:00Z",
		"period_duration": "P3M",
		"renewal_type":    "rollover",
		"max_carry":       maxCarry,
	}

	resp := doRequest(t, "POST", "/api/v1/budgets", budget)
	requireStatus(t, resp, http.StatusCreated)

	var created map[string]any
	json.Unmarshal(readBody(t, resp), &created)
	id := created["metadata"].(map[string]any)["id"].(string)

	resp = doRequest(t, "GET", "/api/v1/budgets/"+id, nil)
	requireStatus(t, resp, http.StatusOK)
	var fetched map[string]any
	json.Unmarshal(readBody(t, resp), &fetched)
	spec := fetched["spec"].(map[string]any)
	if spec["renewal_type"] != "rollover" {
		t.Fatalf("expected renewal_type 'rollover', got %v", spec["renewal_type"])
	}
	if int64(spec["max_carry"].(float64)) != maxCarry {
		t.Fatalf("expected max_carry %d, got %v", maxCarry, spec["max_carry"])
	}
}

// --- Budget Policies ---

func TestBudgetPolicies_CRUD(t *testing.T) {
	rate := map[string]any{
		"name": "policy-test-rate",
		"items": []map[string]any{
			{
				"resource": map[string]string{},
				"costs":    map[string]int64{"input_tokens": 1, "output_tokens": 1},
			},
		},
	}
	resp := doRequest(t, "POST", "/api/v1/rates", rate)
	requireStatus(t, resp, http.StatusCreated)
	var rateCreated map[string]any
	json.Unmarshal(readBody(t, resp), &rateCreated)
	rateID := rateCreated["metadata"].(map[string]any)["id"].(string)

	budget := map[string]any{
		"name":            "policy-test-budget",
		"limit":           1000000,
		"period_start":    "2026-11-01T00:00:00Z",
		"period_duration": "P1M",
		"renewal_type":    "drain",
	}
	resp = doRequest(t, "POST", "/api/v1/budgets", budget)
	requireStatus(t, resp, http.StatusCreated)
	var budgetCreated map[string]any
	json.Unmarshal(readBody(t, resp), &budgetCreated)
	budgetID := budgetCreated["metadata"].(map[string]any)["id"].(string)

	policy := map[string]any{
		"budget_ref":     budgetID,
		"rate_ref":       rateID,
		"deduct_overage": false,
		"subjects": []map[string]string{
			{"kind": "user", "name": "alice"},
			{"kind": "user", "name": "bob"},
		},
	}

	// Create
	resp = doRequest(t, "POST", "/api/v1/budget_policies", policy)
	requireStatus(t, resp, http.StatusCreated)
	var created map[string]any
	json.Unmarshal(readBody(t, resp), &created)
	id := created["metadata"].(map[string]any)["id"].(string)
	t.Logf("created budget policy id=%s", id)

	// Get
	resp = doRequest(t, "GET", "/api/v1/budget_policies/"+id, nil)
	requireStatus(t, resp, http.StatusOK)
	var fetched map[string]any
	json.Unmarshal(readBody(t, resp), &fetched)
	spec := fetched["spec"].(map[string]any)
	subjects := spec["subjects"].([]any)
	if len(subjects) != 2 {
		t.Fatalf("expected 2 subjects, got %d", len(subjects))
	}

	// List
	resp = doRequest(t, "GET", "/api/v1/budget_policies", nil)
	requireStatus(t, resp, http.StatusOK)
	var listing map[string]any
	json.Unmarshal(readBody(t, resp), &listing)
	if listing["total"].(float64) < 1 {
		t.Fatal("expected at least 1 policy in listing")
	}

	// Update — add a subject
	policy["subjects"] = []map[string]string{
		{"kind": "user", "name": "alice"},
		{"kind": "user", "name": "bob"},
		{"kind": "group", "name": "engineering"},
	}
	resp = doRequest(t, "PUT", "/api/v1/budget_policies/"+id, policy)
	requireStatus(t, resp, http.StatusOK)
	var updated map[string]any
	json.Unmarshal(readBody(t, resp), &updated)
	updatedSubjects := updated["spec"].(map[string]any)["subjects"].([]any)
	if len(updatedSubjects) != 3 {
		t.Fatalf("expected 3 subjects after update, got %d", len(updatedSubjects))
	}
}

func TestBudgetPolicies_SoftOverage(t *testing.T) {
	rate := map[string]any{
		"name": "overage-test-rate",
		"items": []map[string]any{
			{
				"resource": map[string]string{},
				"costs":    map[string]int64{"input_tokens": 3, "output_tokens": 15},
			},
		},
	}
	resp := doRequest(t, "POST", "/api/v1/rates", rate)
	requireStatus(t, resp, http.StatusCreated)
	var rateCreated map[string]any
	json.Unmarshal(readBody(t, resp), &rateCreated)
	rateID := rateCreated["metadata"].(map[string]any)["id"].(string)

	budget := map[string]any{
		"name":            "overage-test-budget",
		"limit":           50000,
		"period_start":    "2026-11-01T00:00:00Z",
		"period_duration": "P1M",
		"renewal_type":    "drain",
	}
	resp = doRequest(t, "POST", "/api/v1/budgets", budget)
	requireStatus(t, resp, http.StatusCreated)
	var budgetCreated map[string]any
	json.Unmarshal(readBody(t, resp), &budgetCreated)
	budgetID := budgetCreated["metadata"].(map[string]any)["id"].(string)

	policy := map[string]any{
		"budget_ref":     budgetID,
		"rate_ref":       rateID,
		"deduct_overage": true,
		"subjects": []map[string]string{
			{"kind": "user", "name": "charlie"},
		},
		"overage": map[string]any{
			"type": "soft",
		},
	}

	resp = doRequest(t, "POST", "/api/v1/budget_policies", policy)
	requireStatus(t, resp, http.StatusCreated)
	var created map[string]any
	json.Unmarshal(readBody(t, resp), &created)
	id := created["metadata"].(map[string]any)["id"].(string)

	resp = doRequest(t, "GET", "/api/v1/budget_policies/"+id, nil)
	requireStatus(t, resp, http.StatusOK)
	var fetched map[string]any
	json.Unmarshal(readBody(t, resp), &fetched)
	spec := fetched["spec"].(map[string]any)
	overage := spec["overage"].(map[string]any)
	if overage["type"] != "soft" {
		t.Fatalf("expected overage type 'soft', got %v", overage["type"])
	}
	if spec["deduct_overage"] != true {
		t.Fatal("expected deduct_overage to be true")
	}
}

// --- Tenant header ---

func TestMissingTenantHeader(t *testing.T) {
	req, err := http.NewRequest("GET", baseURL()+"/api/v1/rates", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without X-Tenant header, got %d", resp.StatusCode)
	}
}

// --- Health ---

func TestHealth(t *testing.T) {
	resp, err := http.Get(baseURL() + "/health")
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /health, got %d", resp.StatusCode)
	}
}

// --- Tenant isolation ---

func TestTenantIsolation(t *testing.T) {
	rate := map[string]any{
		"name": "tenant-isolation-rate",
		"items": []map[string]any{
			{
				"resource": map[string]string{},
				"costs":    map[string]int64{"input_tokens": 1, "output_tokens": 1},
			},
		},
	}

	resp := doRequest(t, "POST", "/api/v1/rates", rate)
	requireStatus(t, resp, http.StatusCreated)
	var created map[string]any
	json.Unmarshal(readBody(t, resp), &created)
	id := created["metadata"].(map[string]any)["id"].(string)

	// Try to fetch from a different tenant
	req, _ := http.NewRequest("GET", baseURL()+fmt.Sprintf("/api/v1/rates/%s", id), nil)
	req.Header.Set("X-Tenant", "other-tenant")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for different tenant, got %d", resp.StatusCode)
	}
}
