package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opendatahub-io/ai-metering-service/internal/api"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
	"github.com/opendatahub-io/ai-metering-service/internal/utils"
)

// CatalogRatesHandler serves the global pricing catalog (resource_rates_catalog):
// the templates rate items are copied from. Read-only — rows are assumed to
// exist, populated by an external process (the catalog seed).
type CatalogRatesHandler struct {
	store store.Store
}

func NewCatalogRatesHandler(s store.Store) *CatalogRatesHandler {
	return &CatalogRatesHandler{store: s}
}

// List serves the catalog, paginated, with optional provider (exact) and
// model (case-insensitive prefix) filters. The catalog is global; the
// tenant header authenticates the caller but does not scope the rows.
func (h *CatalogRatesHandler) List(c *gin.Context) {
	rates, err := h.store.ListCatalogRates(c.Request.Context(),
		utils.ParsePagination(c), c.Query("provider"), c.Query("model"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotListCatalogRates, "failed to list catalog rates"))
		return
	}
	c.JSON(http.StatusOK, rates)
}

func (h *CatalogRatesHandler) Get(c *gin.Context) {
	rate, err := h.store.GetCatalogRate(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, api.NewError(api.ErrCannotGetCatalogRate, "catalog rate not found"))
		return
	}
	c.JSON(http.StatusOK, rate)
}
