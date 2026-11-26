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
}
