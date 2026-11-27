package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestEvents_Ingest100(t *testing.T) {
	cleanup()

	// Create a rate covering all models
	rate := map[string]any{
		"name": "e2e-events-rate",
		"items": []map[string]any{
			{
				"resource": map[string]string{},
				"costs":    map[string]int64{"input_tokens": 3, "output_tokens": 15, "cache_read_tokens": 1, "cache_write_tokens": 4, "reasoning_tokens": 15},
			},
		},
	}
	resp := doRequest(t, "POST", "/api/v1/rates", rate)
	requireStatus(t, resp, http.StatusCreated)
	var rateCreated map[string]any
	json.Unmarshal(readBody(t, resp), &rateCreated)
	rateID := rateCreated["metadata"].(map[string]any)["id"].(string)
	t.Logf("created rate id=%s", rateID)

	// Create a budget
	budget := map[string]any{
		"name":            "e2e-events-budget",
		"limit":           10000000,
		"period_start":    "2026-11-01T00:00:00Z",
		"period_duration": "P1M",
		"renewal_type":    "drain",
	}
	resp = doRequest(t, "POST", "/api/v1/budgets", budget)
	requireStatus(t, resp, http.StatusCreated)
	var budgetCreated map[string]any
	json.Unmarshal(readBody(t, resp), &budgetCreated)
	budgetID := budgetCreated["metadata"].(map[string]any)["id"].(string)
	t.Logf("created budget id=%s", budgetID)

	// Create a tenant-level policy so all events match
	policy := map[string]any{
		"budget_ref":     budgetID,
		"rate_ref":       rateID,
		"deduct_overage": false,
		"subjects": []map[string]string{
			{"kind": "tenant", "name": tenant},
		},
	}
	resp = doRequest(t, "POST", "/api/v1/budget_policies", policy)
	requireStatus(t, resp, http.StatusCreated)
	var policyCreated map[string]any
	json.Unmarshal(readBody(t, resp), &policyCreated)
	policyID := policyCreated["metadata"].(map[string]any)["id"].(string)
	t.Logf("created budget policy id=%s", policyID)

	// Ingest 100 events distributed across the past 14 days
	users := []string{"alice", "bob", "charlie", "diana", "eve"}
	groups := [][]string{
		{"engineering", "ml-team"},
		{"engineering"},
		{"data-science"},
		{"data-science", "ml-team"},
		{"platform"},
	}
	orgs := []string{"research", "research", "product", "product", "infra"}
	models := []struct {
		model    string
		provider string
	}{
		{"claude-sonnet-5-5", "anthropic"},
		{"granite-3.3-8b", "ibm"},
		{"llama-3.1-70b", "meta"},
	}

	now := time.Now().UTC()
	base := now.AddDate(0, 0, -2)
	eventCount := 100
	days := 14

	for i := 0; i < eventCount; i++ {
		user := users[i%len(users)]
		model := models[i%len(models)]
		userIdx := i % len(users)

		// Spread events across 14 days ending 2 days ago, so even with
		// hour overflow no event lands in the future.
		daysAgo := days - 1 - (i % days)
		hourOffset := (i / days) * 3
		eventTime := time.Date(base.Year(), base.Month(), base.Day()-daysAgo, 8+hourOffset, i%60, 0, 0, time.UTC)

		event := map[string]any{
			"specversion":     "1.0",
			"id":              fmt.Sprintf("evt-e2e-%04d", i),
			"source":          "praxis/praxis-pod-e2e-0",
			"type":            "io.opendatahub.aigateway.usage.v1",
			"time":            eventTime.Format(time.RFC3339),
			"datacontenttype": "application/json",
			"data": map[string]any{
				"request_id": fmt.Sprintf("req-e2e-%04d", i),
				"identity": map[string]any{
					"user":         user,
					"groups":       groups[userIdx],
					"organization": orgs[userIdx],
					"tenant":       tenant,
				},
				"resource": map[string]string{
					"model":    model.model,
					"provider": model.provider,
				},
				"usage": map[string]int64{
					"input_tokens":       int64(200 + i*10),
					"output_tokens":      int64(50 + i*5),
					"cache_read_tokens":  int64(10 + i*2),
					"cache_write_tokens": int64(2 + i),
					"reasoning_tokens":   int64(5 + i*3),
				},
			},
		}

		resp := doRequest(t, "POST", "/api/v1/events", event)
		requireStatus(t, resp, http.StatusNoContent)
		resp.Body.Close()
	}

	t.Logf("ingested %d CloudEvents across %d days for %d users and %d models",
		eventCount, days, len(users), len(models))

	// Precompute expected totals from the event generation logic above.
	// Rate costs: input_tokens=3, output_tokens=15, cache_read_tokens=1,
	//             cache_write_tokens=4, reasoning_tokens=15
	var expectedRequests int64
	var expectedInputTokens, expectedOutputTokens int64
	var expectedCacheRead, expectedCacheWrite, expectedReasoning int64
	var expectedBUCost int64

	sevenDaysAgo := now.AddDate(0, 0, -7)
	var expected7dRequests int64
	var expected7dBUCost int64

	for i := 0; i < eventCount; i++ {
		input := int64(200 + i*10)
		output := int64(50 + i*5)
		cacheRead := int64(10 + i*2)
		cacheWrite := int64(2 + i)
		reasoning := int64(5 + i*3)
		buCost := input*3 + output*15 + cacheRead*1 + cacheWrite*4 + reasoning*15

		daysAgo := days - 1 - (i % days)
		hourOffset := (i / days) * 3
		eventTime := time.Date(base.Year(), base.Month(), base.Day()-daysAgo, 8+hourOffset, i%60, 0, 0, time.UTC)

		expectedRequests++
		expectedInputTokens += input
		expectedOutputTokens += output
		expectedCacheRead += cacheRead
		expectedCacheWrite += cacheWrite
		expectedReasoning += reasoning
		expectedBUCost += buCost

		if !eventTime.Before(sevenDaysAgo) && !eventTime.After(now) {
			expected7dRequests++
			expected7dBUCost += buCost
		}
	}
	t.Logf("expected 30d: %d requests, %d BU; 7d: %d requests, %d BU",
		expectedRequests, expectedBUCost, expected7dRequests, expected7dBUCost)

	// --- Query: total usage over 30d (covers all 14 days) ---
	t.Run("total_30d", func(t *testing.T) {
		resp := doRequest(t, "GET", "/api/v1/usage?range=30d", nil)
		requireStatus(t, resp, http.StatusOK)
		var result map[string]any
		json.Unmarshal(readBody(t, resp), &result)

		data := result["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("expected 1 total row, got %d", len(data))
		}
		row := data[0].(map[string]any)
		requests := int64(row["requests"].(float64))
		buCost := int64(row["bu_cost"].(float64))
		usage := row["usage"].(map[string]any)

		if requests != expectedRequests {
			t.Errorf("requests: expected %d, got %d", expectedRequests, requests)
		}
		if buCost != expectedBUCost {
			t.Errorf("bu_cost: expected %d, got %d", expectedBUCost, buCost)
		}
		if int64(usage["input_tokens"].(float64)) != expectedInputTokens {
			t.Errorf("input_tokens: expected %d, got %v", expectedInputTokens, usage["input_tokens"])
		}
		if int64(usage["output_tokens"].(float64)) != expectedOutputTokens {
			t.Errorf("output_tokens: expected %d, got %v", expectedOutputTokens, usage["output_tokens"])
		}
		t.Logf("total 30d: %d requests, %d BU cost", requests, buCost)
	})

	// --- Query: total usage over 7d ---
	t.Run("total_7d", func(t *testing.T) {
		resp := doRequest(t, "GET", "/api/v1/usage?range=7d", nil)
		requireStatus(t, resp, http.StatusOK)
		var result map[string]any
		json.Unmarshal(readBody(t, resp), &result)

		data := result["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("expected 1 total row, got %d", len(data))
		}
		row := data[0].(map[string]any)
		requests := int64(row["requests"].(float64))
		buCost := int64(row["bu_cost"].(float64))

		if requests != expected7dRequests {
			t.Errorf("requests: expected %d, got %d", expected7dRequests, requests)
		}
		if buCost != expected7dBUCost {
			t.Errorf("bu_cost: expected %d, got %d", expected7dBUCost, buCost)
		}
		t.Logf("total 7d: %d requests, %d BU cost", requests, buCost)
	})

	// --- Query: daily granularity over 7d ---
	t.Run("daily_7d", func(t *testing.T) {
		resp := doRequest(t, "GET", "/api/v1/usage?range=7d&granularity=day", nil)
		requireStatus(t, resp, http.StatusOK)
		var result map[string]any
		json.Unmarshal(readBody(t, resp), &result)

		data := result["data"].([]any)
		if len(data) == 0 {
			t.Fatal("expected at least 1 daily bucket")
		}

		var totalRequests, totalBUCost int64
		for _, item := range data {
			row := item.(map[string]any)
			totalRequests += int64(row["requests"].(float64))
			totalBUCost += int64(row["bu_cost"].(float64))
		}

		if totalRequests != expected7dRequests {
			t.Errorf("sum of daily requests: expected %d, got %d", expected7dRequests, totalRequests)
		}
		if totalBUCost != expected7dBUCost {
			t.Errorf("sum of daily bu_cost: expected %d, got %d", expected7dBUCost, totalBUCost)
		}
		t.Logf("daily 7d: %d buckets, %d total requests, %d total BU cost", len(data), totalRequests, totalBUCost)
	})

	// --- Query: daily granularity over 30d — sum of daily buckets must equal total ---
	t.Run("daily_30d", func(t *testing.T) {
		resp := doRequest(t, "GET", "/api/v1/usage?range=30d&granularity=day", nil)
		requireStatus(t, resp, http.StatusOK)
		var result map[string]any
		json.Unmarshal(readBody(t, resp), &result)

		data := result["data"].([]any)
		if len(data) == 0 {
			t.Fatal("expected at least 1 daily bucket")
		}

		var totalRequests, totalBUCost int64
		for _, item := range data {
			row := item.(map[string]any)
			totalRequests += int64(row["requests"].(float64))
			totalBUCost += int64(row["bu_cost"].(float64))
		}

		if totalRequests != expectedRequests {
			t.Errorf("sum of daily requests: expected %d, got %d", expectedRequests, totalRequests)
		}
		if totalBUCost != expectedBUCost {
			t.Errorf("sum of daily bu_cost: expected %d, got %d", expectedBUCost, totalBUCost)
		}
		t.Logf("daily 30d: %d buckets, %d total requests, %d total BU cost", len(data), totalRequests, totalBUCost)
	})

	// --- Query: grouped by model — sum across models must equal total ---
	t.Run("by_model_30d", func(t *testing.T) {
		resp := doRequest(t, "GET", "/api/v1/usage?range=30d&group_by=model", nil)
		requireStatus(t, resp, http.StatusOK)
		var result map[string]any
		json.Unmarshal(readBody(t, resp), &result)

		data := result["data"].([]any)
		if len(data) != len(models) {
			t.Fatalf("expected %d model groups, got %d", len(models), len(data))
		}

		if got := int64(result["total"].(float64)); got != int64(len(models)) {
			t.Errorf("total groups: expected %d, got %d", len(models), got)
		}

		var totalRequests, totalBUCost int64
		for _, item := range data {
			row := item.(map[string]any)
			dims := row["dimensions"].(map[string]any)
			reqs := int64(row["requests"].(float64))
			cost := int64(row["bu_cost"].(float64))
			totalRequests += reqs
			totalBUCost += cost
			t.Logf("  model=%s: %d requests, %d BU cost", dims["model"], reqs, cost)
		}

		if totalRequests != expectedRequests {
			t.Errorf("sum of model requests: expected %d, got %d", expectedRequests, totalRequests)
		}
		if totalBUCost != expectedBUCost {
			t.Errorf("sum of model bu_cost: expected %d, got %d", expectedBUCost, totalBUCost)
		}
	})

	// --- Query: grouped by user — sum across users must equal total ---
	t.Run("by_user_30d", func(t *testing.T) {
		resp := doRequest(t, "GET", "/api/v1/usage?range=30d&group_by=user", nil)
		requireStatus(t, resp, http.StatusOK)
		var result map[string]any
		json.Unmarshal(readBody(t, resp), &result)

		data := result["data"].([]any)
		if len(data) != len(users) {
			t.Fatalf("expected %d user groups, got %d", len(users), len(data))
		}

		var totalRequests int64
		for _, item := range data {
			row := item.(map[string]any)
			dims := row["dimensions"].(map[string]any)
			reqs := int64(row["requests"].(float64))
			totalRequests += reqs
			t.Logf("  user=%s: %d requests", dims["user"], reqs)
		}

		if totalRequests != expectedRequests {
			t.Errorf("sum of user requests: expected %d, got %d", expectedRequests, totalRequests)
		}
	})

	// --- Query: daily + grouped by model — cross-check ---
	t.Run("daily_by_model_30d", func(t *testing.T) {
		resp := doRequest(t, "GET", "/api/v1/usage?range=30d&granularity=day&group_by=model", nil)
		requireStatus(t, resp, http.StatusOK)
		var result map[string]any
		json.Unmarshal(readBody(t, resp), &result)

		data := result["data"].([]any)
		if len(data) == 0 {
			t.Fatal("expected at least 1 row")
		}

		var totalRequests, totalBUCost int64
		for _, item := range data {
			row := item.(map[string]any)
			totalRequests += int64(row["requests"].(float64))
			totalBUCost += int64(row["bu_cost"].(float64))
		}

		if totalRequests != expectedRequests {
			t.Errorf("sum of daily×model requests: expected %d, got %d", expectedRequests, totalRequests)
		}
		if totalBUCost != expectedBUCost {
			t.Errorf("sum of daily×model bu_cost: expected %d, got %d", expectedBUCost, totalBUCost)
		}
		t.Logf("daily×model 30d: %d rows, %d total requests", len(data), totalRequests)
	})

	// --- Query: filter by single user ---
	t.Run("filter_user_alice", func(t *testing.T) {
		resp := doRequest(t, "GET", "/api/v1/usage?range=30d&user=alice", nil)
		requireStatus(t, resp, http.StatusOK)
		var result map[string]any
		json.Unmarshal(readBody(t, resp), &result)

		data := result["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("expected 1 row for alice, got %d", len(data))
		}
		row := data[0].(map[string]any)
		requests := int64(row["requests"].(float64))

		// alice is users[0], gets events where i%5==0: i=0,5,10,...,95 → 20 events
		expectedAlice := int64(eventCount / len(users))
		if requests != expectedAlice {
			t.Errorf("alice requests: expected %d, got %d", expectedAlice, requests)
		}
		t.Logf("alice: %d requests", requests)
	})

	// --- Query: grouped by budget — the per-charge view. Every event in
	// this fixture charges the one budget, so the single group's requests
	// and bu_cost must equal the event-level totals.
	t.Run("by_budget_30d", func(t *testing.T) {
		resp := doRequest(t, "GET", "/api/v1/usage?range=30d&group_by=budget", nil)
		requireStatus(t, resp, http.StatusOK)
		var result map[string]any
		json.Unmarshal(readBody(t, resp), &result)

		data := result["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("expected 1 budget group, got %d", len(data))
		}
		row := data[0].(map[string]any)
		dims := row["dimensions"].(map[string]any)
		if dims["budget"] != budgetID {
			t.Errorf("budget group = %v, want %s", dims["budget"], budgetID)
		}
		requests := int64(row["requests"].(float64))
		cost := int64(row["bu_cost"].(float64))
		if requests != expectedRequests {
			t.Errorf("budget requests: expected %d, got %d", expectedRequests, requests)
		}
		if cost != expectedBUCost {
			t.Errorf("budget bu_cost: expected %d, got %d", expectedBUCost, cost)
		}
		t.Logf("budget %s: %d requests, %d BU cost", budgetID, requests, cost)
	})

	// --- Query: order_by with a time granularity — the bucket leads, so
	// the series stays chronological and the metric ranks the groups
	// within each bucket.
	t.Run("daily_by_model_order_by_cost", func(t *testing.T) {
		resp := doRequest(t, "GET", "/api/v1/usage?range=30d&granularity=day&group_by=model&order_by=bu_cost", nil)
		requireStatus(t, resp, http.StatusOK)
		var result map[string]any
		json.Unmarshal(readBody(t, resp), &result)

		data := result["data"].([]any)
		if len(data) < 2 {
			t.Fatalf("expected multiple day/model cells, got %d", len(data))
		}
		var lastBucket string
		var lastCost float64
		for i, item := range data {
			row := item.(map[string]any)
			bucket := row["bucket"].(string)
			cost := row["bu_cost"].(float64)
			if i > 0 {
				if bucket < lastBucket {
					t.Fatalf("buckets out of chronological order at row %d: %s after %s", i, bucket, lastBucket)
				}
				if bucket == lastBucket && cost > lastCost {
					t.Fatalf("within bucket %s, bu_cost increased: %v after %v", bucket, cost, lastCost)
				}
			}
			lastBucket, lastCost = bucket, cost
		}
		t.Logf("daily ordered cells: %d, chronology and per-bucket ranking hold", len(data))
	})

	// --- Query: filtered by budget — flat totals for that budget's charges.
	t.Run("filter_budget_30d", func(t *testing.T) {
		resp := doRequest(t, "GET", "/api/v1/usage?range=30d&budget="+budgetID, nil)
		requireStatus(t, resp, http.StatusOK)
		var result map[string]any
		json.Unmarshal(readBody(t, resp), &result)

		data := result["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("expected 1 row, got %d", len(data))
		}
		row := data[0].(map[string]any)
		requests := int64(row["requests"].(float64))
		cost := int64(row["bu_cost"].(float64))
		if requests != expectedRequests {
			t.Errorf("budget-filtered requests: expected %d, got %d", expectedRequests, requests)
		}
		if cost != expectedBUCost {
			t.Errorf("budget-filtered bu_cost: expected %d, got %d", expectedBUCost, cost)
		}
	})
}

// Events that resolve no charges — no policy matches the resource — stay
// in the ledger (one row per CloudEvent) and surface in the usage queries:
// the empty-budget group in group_by=budget is the unmetered-traffic view.
// A group_by dimension the events do not carry groups under the empty
// string instead of failing.
func TestUsage_UnmeteredEvents(t *testing.T) {
	cleanup()

	// A rate covering exactly one model: events for any other model match
	// the policy but no rate item, so they meter at nothing.
	rate := map[string]any{
		"name": "e2e-unmetered-rate",
		"items": []map[string]any{
			{
				"resource": map[string]string{"model": "e2e-covered-model"},
				"costs":    map[string]int64{"input_tokens": 2},
			},
		},
	}
	resp := doRequest(t, "POST", "/api/v1/rates", rate)
	requireStatus(t, resp, http.StatusCreated)
	var rateCreated map[string]any
	json.Unmarshal(readBody(t, resp), &rateCreated)
	rateID := rateCreated["metadata"].(map[string]any)["id"].(string)

	budget := map[string]any{
		"name":            "e2e-unmetered-budget",
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
		"budget_ref": budgetID,
		"rate_ref":   rateID,
		"subjects":   []map[string]string{{"kind": "tenant", "name": tenant}},
	}
	resp = doRequest(t, "POST", "/api/v1/budget_policies", policy)
	requireStatus(t, resp, http.StatusCreated)

	ingest := func(id, model string, tokens int64) {
		event := map[string]any{
			"specversion": "1.0",
			"id":          id,
			"source":      "praxis/praxis-pod-e2e-0",
			"type":        "io.opendatahub.aigateway.usage.v1",
			"time":        time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339),
			"data": map[string]any{
				"request_id": id,
				"identity":   map[string]any{"user": "alice", "tenant": tenant},
				"resource":   map[string]string{"model": model},
				"usage":      map[string]int64{"input_tokens": tokens},
			},
		}
		resp := doRequest(t, "POST", "/api/v1/events", event)
		requireStatus(t, resp, http.StatusNoContent)
		resp.Body.Close()
	}
	ingest("evt-unmetered-1", "e2e-covered-model", 50)   // charged: 50 * 2 = 100 BU
	ingest("evt-unmetered-2", "e2e-covered-model", 50)   // charged: 100 BU
	ingest("evt-unmetered-3", "e2e-uncovered-model", 50) // no rate item: unmetered

	// --- group_by=budget: the budget's group plus the empty unmetered group
	resp = doRequest(t, "GET", "/api/v1/usage?range=30d&group_by=budget", nil)
	requireStatus(t, resp, http.StatusOK)
	var result map[string]any
	json.Unmarshal(readBody(t, resp), &result)
	data := result["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("expected 2 budget groups (budget + unmetered), got %d: %v", len(data), data)
	}
	groups := map[string]map[string]any{}
	for _, item := range data {
		row := item.(map[string]any)
		groups[row["dimensions"].(map[string]any)["budget"].(string)] = row
	}
	budgetRow, ok := groups[budgetID]
	if !ok {
		t.Fatalf("budget group %s missing: %v", budgetID, groups)
	}
	if got := int64(budgetRow["requests"].(float64)); got != 2 {
		t.Errorf("budget requests = %d, want 2", got)
	}
	if got := int64(budgetRow["bu_cost"].(float64)); got != 200 {
		t.Errorf("budget bu_cost = %d, want 200", got)
	}
	unmetered, ok := groups[""]
	if !ok {
		t.Fatalf("unmetered (empty budget) group missing: %v", groups)
	}
	if got := int64(unmetered["requests"].(float64)); got != 1 {
		t.Errorf("unmetered requests = %d, want 1", got)
	}
	if got := int64(unmetered["bu_cost"].(float64)); got != 0 {
		t.Errorf("unmetered bu_cost = %d, want 0", got)
	}

	// --- group_by=model: the uncovered model is visible, metered at zero
	resp = doRequest(t, "GET", "/api/v1/usage?range=30d&group_by=model", nil)
	requireStatus(t, resp, http.StatusOK)
	json.Unmarshal(readBody(t, resp), &result)
	models := map[string]int64{}
	for _, item := range result["data"].([]any) {
		row := item.(map[string]any)
		models[row["dimensions"].(map[string]any)["model"].(string)] = int64(row["bu_cost"].(float64))
	}
	if models["e2e-covered-model"] != 200 || models["e2e-uncovered-model"] != 0 {
		t.Errorf("model costs = %v, want covered=200 uncovered=0", models)
	}

	// --- group_by on a dimension no event carries: one empty group, no error
	resp = doRequest(t, "GET", "/api/v1/usage?range=30d&group_by=region", nil)
	requireStatus(t, resp, http.StatusOK)
	json.Unmarshal(readBody(t, resp), &result)
	data = result["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected 1 region group, got %d", len(data))
	}
	if dims := data[0].(map[string]any)["dimensions"].(map[string]any); dims["region"] != "" {
		t.Errorf("missing dimension should group under empty string, got %v", dims)
	}

	// --- a dimension that cannot be embedded in SQL is rejected
	resp = doRequest(t, "GET", "/api/v1/usage?range=30d&group_by=model'--", nil)
	requireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()

	// --- order_by: the top-N leaderboard. The covered model cost 200 BU
	// across 2 requests; the uncovered model 0 across 1. Descending by
	// cost puts the covered model first; limit=1 yields the top group.
	resp = doRequest(t, "GET", "/api/v1/usage?range=30d&group_by=model&order_by=bu_cost", nil)
	requireStatus(t, resp, http.StatusOK)
	json.Unmarshal(readBody(t, resp), &result)
	data = result["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("expected 2 model groups, got %d", len(data))
	}
	first := data[0].(map[string]any)
	if first["dimensions"].(map[string]any)["model"] != "e2e-covered-model" {
		t.Errorf("order_by=bu_cost desc: first group = %v, want e2e-covered-model", first["dimensions"])
	}
	if got := int64(first["bu_cost"].(float64)); got != 200 {
		t.Errorf("top group bu_cost = %d, want 200", got)
	}

	resp = doRequest(t, "GET", "/api/v1/usage?range=30d&group_by=model&order_by=bu_cost&direction=asc", nil)
	requireStatus(t, resp, http.StatusOK)
	json.Unmarshal(readBody(t, resp), &result)
	data = result["data"].([]any)
	if data[0].(map[string]any)["dimensions"].(map[string]any)["model"] != "e2e-uncovered-model" {
		t.Errorf("order_by=bu_cost asc: first group = %v, want e2e-uncovered-model", data[0].(map[string]any)["dimensions"])
	}

	resp = doRequest(t, "GET", "/api/v1/usage?range=30d&group_by=model&order_by=bu_cost&limit=1", nil)
	requireStatus(t, resp, http.StatusOK)
	json.Unmarshal(readBody(t, resp), &result)
	data = result["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["dimensions"].(map[string]any)["model"] != "e2e-covered-model" {
		t.Errorf("order_by + limit=1: expected only the top group, got %v", data)
	}
	// Truncation is detectable: total reports the full group count even
	// though limit returned one row.
	if got := int64(result["total"].(float64)); got != 2 {
		t.Errorf("total groups with limit=1: expected 2, got %d", got)
	}

	// --- invalid order_by / direction are rejected
	resp = doRequest(t, "GET", "/api/v1/usage?range=30d&group_by=model&order_by=foo", nil)
	requireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
	resp = doRequest(t, "GET", "/api/v1/usage?range=30d&group_by=model&order_by=bu_cost&direction=down", nil)
	requireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
	resp = doRequest(t, "GET", "/api/v1/usage?range=30d&group_by=model&direction=desc", nil)
	requireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}
