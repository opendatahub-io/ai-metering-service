package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opendatahub-io/ai-metering-service/internal/api"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
)

// rateStore serves the rate validation tests: the write methods echo the
// rate back with an id.
type rateStore struct {
	store.Store
}

func (s *rateStore) CreateRate(ctx context.Context, tenantID string, rate *api.Rate) (*api.RateResource, error) {
	return &api.RateResource{Metadata: api.Metadata{ID: "r1"}, Spec: *rate}, nil
}

func (s *rateStore) UpdateRate(ctx context.Context, tenantID, id string, rate *api.Rate) (*api.RateResource, error) {
	return &api.RateResource{Metadata: api.Metadata{ID: id}, Spec: *rate}, nil
}

func submitRate(t *testing.T, method, path string, rate api.Rate) int {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := NewRatesHandler(&rateStore{})
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
	return rec.Code
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
