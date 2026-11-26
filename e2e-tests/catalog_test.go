package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/opendatahub-io/ai-metering-service/internal/api"
)

// The catalog import runs at service startup against the file the running
// service was configured with (catalog.file). This test asserts the running
// stack's catalog against the committed snapshot in
// e2e-tests/resources/model_prices_and_context_window.json: run the service
// with that file and cost_to_bu 1000000000 (1 BU = 1 nanodollar, costs
// rounded up) — the documented .local convention.
func TestCatalogImport(t *testing.T) {
	// Entries with known USD prices in the committed snapshot, and their
	// expected integer-BU conversion at nanodollar scale. The gemini entry
	// exercises all five cost keys; the realtime entry a different
	// provider and cache pricing.
	expect := []struct {
		provider string
		model    string
		costs    map[string]int64
	}{
		{
			provider: "gemini", model: "gemini-2.5-flash",
			costs: map[string]int64{
				"input_tokens": 300, "output_tokens": 2500,
				"cache_read_tokens": 30, "cache_write_tokens": 0,
				"reasoning_tokens": 2500,
			},
		},
		{
			provider: "azure", model: "gpt-realtime-2.1-2026-07-07",
			costs: map[string]int64{
				"input_tokens": 4000, "output_tokens": 24000,
				"cache_read_tokens": 400,
			},
		},
	}

	for _, e := range expect {
		// The model filter is a prefix match, so a family query can return
		// siblings — find the exact model in the listing.
		listing := listCatalogRates(t, "provider="+e.provider+"&model="+e.model)
		var entry *api.ResourceCatalogRate
		for i := range listing.Items {
			if listing.Items[i].Spec.Resource["model"] == e.model {
				entry = &listing.Items[i]
				break
			}
		}
		if entry == nil {
			t.Fatalf("%s/%s missing from the catalog (was the service started with e2e-tests/resources/model_prices_and_context_window.json?)", e.provider, e.model)
		}

		if entry.Spec.Resource["provider"] != e.provider {
			t.Errorf("%s: provider = %q, want %q", e.model, entry.Spec.Resource["provider"], e.provider)
		}
		for key, want := range e.costs {
			if got := entry.Spec.Costs[key]; got != want {
				t.Errorf("%s costs[%s] = %d, want %d", e.model, key, got, want)
			}
		}

		// Single-entry lookup by id returns the same entry.
		byID := getCatalogRate(t, entry.Metadata.ID)
		if byID.Spec.Resource["model"] != e.model {
			t.Errorf("get by id: model = %q, want %q", byID.Spec.Resource["model"], e.model)
		}

		// The full loop: a rate created against this catalog entry copies
		// its resource and costs verbatim, keeping the ref as provenance.
		rateBody := fmt.Sprintf(`{"name":"e2e-catalog-rate","items":[{"catalog_rate_ref":%q}]}`, entry.Metadata.ID)
		req, _ := http.NewRequest(http.MethodPost, baseURL()+"/api/v1/rates", bytes.NewReader([]byte(rateBody)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tenant", tenant)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("create rate: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create rate from catalog entry: status %d: %s", resp.StatusCode, body)
		}
		var created api.RateResource
		if err := json.Unmarshal(body, &created); err != nil {
			t.Fatalf("unmarshal created rate: %v", err)
		}
		item := created.Spec.Items[0]
		if item.Resource["model"] != e.model || item.CatalogRateRef != entry.Metadata.ID {
			t.Fatalf("rate item not copied from catalog: %+v", item)
		}
		for key, want := range e.costs {
			if item.Costs[key] != want {
				t.Errorf("copied costs[%s] = %d, want %d", key, item.Costs[key], want)
			}
		}

		// Clean up the rate this test created — catalog rows are additive
		// by design and stay.
		del, _ := http.NewRequest(http.MethodDelete, baseURL()+"/api/v1/rates/"+created.Metadata.ID, nil)
		del.Header.Set("X-Tenant", tenant)
		if dresp, derr := http.DefaultClient.Do(del); derr != nil || dresp.StatusCode != http.StatusNoContent {
			t.Errorf("cleanup rate: err=%v status=%v", derr, dresp)
		} else {
			dresp.Body.Close()
		}
	}
}

func listCatalogRates(t *testing.T, query string) api.Listing[api.ResourceCatalogRate] {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL()+"/api/v1/catalog-rates?"+query, nil)
	req.Header.Set("X-Tenant", tenant)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list catalog rates: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list catalog rates (?%s): status %d: %s", query, resp.StatusCode, body)
	}
	var listing api.Listing[api.ResourceCatalogRate]
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatalf("unmarshal catalog listing: %v", err)
	}
	return listing
}

func getCatalogRate(t *testing.T, id string) api.ResourceCatalogRate {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL()+"/api/v1/catalog-rates/"+id, nil)
	req.Header.Set("X-Tenant", tenant)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get catalog rate: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get catalog rate: status %d: %s", resp.StatusCode, body)
	}
	var entry api.ResourceCatalogRate
	if err := json.Unmarshal(body, &entry); err != nil {
		t.Fatalf("unmarshal catalog rate: %v", err)
	}
	return entry
}
