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

// rateStore serves the rate validation tests: the write methods echo the
// rate back with an id, and the catalogRates map serves catalog_rate_ref
// resolution.
type rateStore struct {
	store.Store
	catalogRates map[string]*api.ResourceCatalogRate
}

func (s *rateStore) CreateRate(ctx context.Context, tenantID string, rate *api.Rate) (*api.RateResource, error) {
	return &api.RateResource{Metadata: api.Metadata{ID: "r1"}, Spec: *rate}, nil
}

func (s *rateStore) UpdateRate(ctx context.Context, tenantID, id string, rate *api.Rate) (*api.RateResource, error) {
	return &api.RateResource{Metadata: api.Metadata{ID: id}, Spec: *rate}, nil
}

func (s *rateStore) GetCatalogRate(ctx context.Context, id string) (*api.ResourceCatalogRate, error) {
	b, ok := s.catalogRates[id]
	if !ok {
		return nil, fmt.Errorf("resource rate %q not found", id)
	}
	return b, nil
}

func catalogRate(id string, resource map[string]string, costs map[string]int64) *api.ResourceCatalogRate {
	return &api.ResourceCatalogRate{
		Metadata: api.Metadata{ID: id},
		Spec:     api.CatalogRate{Resource: resource, Costs: costs},
	}
}

// submitRateFull runs a rate request against a store with resource rates and
// returns the status and the decoded response body.
func submitRateFull(t *testing.T, method, path string, rate api.Rate, s *rateStore) (int, api.RateResource) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := NewRatesHandler(s)
	router.POST("/rates", RequireTenant(), h.Create)
	router.PUT("/rates/:id", RequireTenant(), h.Update)

	body, err := json.Marshal(rate)
	if err != nil {
		t.Fatalf("marshal rate: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant", "acme-corp")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var res api.RateResource
	if rec.Code < 300 {
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
	}
	return rec.Code, res
}

func submitRate(t *testing.T, method, path string, rate api.Rate) int {
	t.Helper()
	code, _ := submitRateFull(t, method, path, rate, &rateStore{})
	return code
}

// A wildcard resource value is rejected at write time: an unconstrained
// dimension is a key left out of the item's resource map, and the empty map
// is the catch-all. A stored "*" would compare literally and silently never
// match.
func TestCreateRateRejectsWildcardValue(t *testing.T) {
	wildcard := api.Rate{Name: "bad", Items: []api.RateItem{{
		Resource: map[string]string{"model": "*"},
		Costs:    map[string]int64{"input_tokens": 1},
	}}}
	if code := submitRate(t, http.MethodPost, "/rates", wildcard); code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (wildcard value)", code, http.StatusBadRequest)
	}

	// The supported spellings pass: concrete values, keys left out, and the
	// empty catch-all.
	ok := api.Rate{Name: "good", Items: []api.RateItem{{
		Resource: map[string]string{"model": "opus-4.6"},
		Costs:    map[string]int64{"input_tokens": 1},
	}}}
	if code := submitRate(t, http.MethodPost, "/rates", ok); code != http.StatusCreated {
		t.Errorf("status = %d, want %d (concrete value)", code, http.StatusCreated)
	}

	catchAll := api.Rate{Name: "catch-all", Items: []api.RateItem{{
		Resource: map[string]string{},
		Costs:    map[string]int64{"input_tokens": 1},
	}}}
	if code := submitRate(t, http.MethodPost, "/rates", catchAll); code != http.StatusCreated {
		t.Errorf("status = %d, want %d (empty catch-all)", code, http.StatusCreated)
	}

	// Update path validates too.
	if code := submitRate(t, http.MethodPut, "/rates/r1", wildcard); code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (wildcard value on update)", code, http.StatusBadRequest)
	}
}

// An item carrying a catalog_rate_ref has its resource and costs copied from
// the catalog row, with the ref kept as provenance; a hand-written item in
// the same request passes through untouched. A ref that does not resolve
// is a client error.
func TestCreateRateResolvesCatalogRateRef(t *testing.T) {
	s := &rateStore{catalogRates: map[string]*api.ResourceCatalogRate{
		"b1": catalogRate("b1", map[string]string{"model": "claude-haiku-4-5", "provider": "anthropic"},
			map[string]int64{"input_tokens": 1000, "output_tokens": 5000}),
	}}

	rate := api.Rate{Name: "team-rate", Items: []api.RateItem{
		{CatalogRateRef: "b1"},
		{Resource: map[string]string{"model": "my-model"},
			Costs: map[string]int64{"input_tokens": 1}},
	}}
	code, res := submitRateFull(t, http.MethodPost, "/rates", rate, s)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", code, http.StatusCreated)
	}

	if len(res.Spec.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(res.Spec.Items))
	}
	copied := res.Spec.Items[0]
	if copied.CatalogRateRef != "b1" {
		t.Errorf("provenance ref lost: %+v", copied)
	}
	if copied.Resource["model"] != "claude-haiku-4-5" || copied.Resource["provider"] != "anthropic" {
		t.Errorf("resource not copied from catalog entry: %+v", copied.Resource)
	}
	if copied.Costs["input_tokens"] != 1000 || copied.Costs["output_tokens"] != 5000 {
		t.Errorf("costs not copied from catalog entry: %+v", copied.Costs)
	}
	custom := res.Spec.Items[1]
	if custom.CatalogRateRef != "" || custom.Resource["model"] != "my-model" {
		t.Errorf("hand-written item must pass through untouched: %+v", custom)
	}

	// An unresolvable ref is rejected before anything is stored.
	bad := api.Rate{Name: "bad", Items: []api.RateItem{{CatalogRateRef: "nope"}}}
	if code, _ := submitRateFull(t, http.MethodPost, "/rates", bad, s); code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (unknown catalog_rate_ref)", code, http.StatusBadRequest)
	}
}

// The update path resolves catalog_rate_refs identically.
func TestUpdateRateResolvesCatalogRateRef(t *testing.T) {
	s := &rateStore{catalogRates: map[string]*api.ResourceCatalogRate{
		"b1": catalogRate("b1", map[string]string{"model": "granite-3.3-8b", "provider": "ibm"}, map[string]int64{"input_tokens": 200}),
	}}
	rate := api.Rate{Name: "team-rate", Items: []api.RateItem{{CatalogRateRef: "b1"}}}
	code, res := submitRateFull(t, http.MethodPut, "/rates/r1", rate, s)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want %d", code, http.StatusOK)
	}
	if res.Spec.Items[0].Resource["model"] != "granite-3.3-8b" || res.Spec.Items[0].Costs["input_tokens"] != 200 {
		t.Errorf("resource rate not resolved on update: %+v", res.Spec.Items[0])
	}
}

// catalogRateStore serves the catalog endpoints.
type catalogRateStore struct {
	store.Store
	listing     *api.Listing[api.ResourceCatalogRate]
	catalogRate *api.ResourceCatalogRate
}

func (s *catalogRateStore) ListCatalogRates(ctx context.Context, pg store.Pagination, provider, modelPrefix string) (*api.Listing[api.ResourceCatalogRate], error) {
	return s.listing, nil
}

func (s *catalogRateStore) GetCatalogRate(ctx context.Context, id string) (*api.ResourceCatalogRate, error) {
	if s.catalogRate == nil {
		return nil, fmt.Errorf("not found")
	}
	return s.catalogRate, nil
}

func TestResourceRatesEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := NewCatalogRatesHandler(&catalogRateStore{
		listing: &api.Listing[api.ResourceCatalogRate]{
			Total: 1,
			Items: []api.ResourceCatalogRate{*catalogRate("b1", map[string]string{"model": "claude-haiku-4-5", "provider": "anthropic"}, nil)},
		},
		catalogRate: catalogRate("b1", map[string]string{"model": "claude-haiku-4-5", "provider": "anthropic"}, nil),
	})
	router.GET("/catalog-rates", RequireTenant(), h.List)
	router.GET("/catalog-rates/:id", RequireTenant(), h.Get)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/catalog-rates", nil)
	req.Header.Set("X-Tenant", "acme-corp")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d", rec.Code, http.StatusOK)
	}
	var listing api.Listing[api.ResourceCatalogRate]
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatalf("unmarshal listing: %v", err)
	}
	if listing.Total != 1 || listing.Items[0].Spec.Resource["model"] != "claude-haiku-4-5" {
		t.Fatalf("unexpected listing: %+v", listing)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/catalog-rates/b1", nil)
	req.Header.Set("X-Tenant", "acme-corp")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d", rec.Code, http.StatusOK)
	}
}
