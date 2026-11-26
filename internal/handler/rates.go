package handler

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opendatahub-io/ai-metering-service/internal/api"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
	"github.com/opendatahub-io/ai-metering-service/internal/utils"
)

type RatesHandler struct {
	store store.Store
}

func NewRatesHandler(s store.Store) *RatesHandler {
	return &RatesHandler{store: s}
}

func (h *RatesHandler) List(c *gin.Context) {
	rates, err := h.store.ListRates(c.Request.Context(), getTenantID(c), utils.ParsePagination(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotListRates, "failed to list rates"))
		return
	}
	c.JSON(http.StatusOK, rates)
}

func (h *RatesHandler) Get(c *gin.Context) {
	rate, err := h.store.GetRate(c.Request.Context(), getTenantID(c), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, api.NewError(api.ErrCannotGetRate, "rate not found"))
		return
	}
	c.JSON(http.StatusOK, rate)
}

func (h *RatesHandler) Create(c *gin.Context) {
	var rate api.Rate
	if err := c.ShouldBindJSON(&rate); err != nil {
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrBadRequest, err.Error()))
		return
	}
	if err := h.resolveCatalogRateRefs(c.Request.Context(), rate.Items); err != nil {
		c.JSON(http.StatusBadRequest, err)
		return
	}
	if err := validateRate(&rate); err != nil {
		c.JSON(http.StatusBadRequest, err)
		return
	}
	res, err := h.store.CreateRate(c.Request.Context(), getTenantID(c), &rate)
	if err != nil {
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotCreateRate, "failed to create rate"))
		return
	}
	c.JSON(http.StatusCreated, res)
}

func (h *RatesHandler) Update(c *gin.Context) {
	var rate api.Rate
	if err := c.ShouldBindJSON(&rate); err != nil {
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrBadRequest, err.Error()))
		return
	}
	if err := h.resolveCatalogRateRefs(c.Request.Context(), rate.Items); err != nil {
		c.JSON(http.StatusBadRequest, err)
		return
	}
	if err := validateRate(&rate); err != nil {
		c.JSON(http.StatusBadRequest, err)
		return
	}
	res, err := h.store.UpdateRate(c.Request.Context(), getTenantID(c), c.Param("id"), &rate)
	if err != nil {
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotUpdateRate, "failed to update rate"))
		return
	}
	c.JSON(http.StatusOK, res)
}

func (h *RatesHandler) Delete(c *gin.Context) {
	if err := h.store.DeleteRate(c.Request.Context(), getTenantID(c), c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotDeleteRate, "failed to delete rate"))
		return
	}
	c.Status(http.StatusNoContent)
}

// validateRate rejects wildcard resource values. Resource matching is a
// subset constraint: an unconstrained dimension is expressed by leaving the
// key out of the item's resource map, and an empty resource map is the
// catch-all — a "*" value has no matching semantics and would silently never
// match (the matcher compares values literally), so it must not be stored.
func validateRate(rate *api.Rate) *api.Error {
	for _, item := range rate.Items {
		for key, value := range item.Resource {
			if value == "*" {
				return api.NewError(api.ErrBadRequest,
					fmt.Sprintf("wildcard values are not supported: resource key %q must name a concrete value; leave the key out to leave that dimension unconstrained, or use an empty resource map as the catch-all", key))
			}
		}
	}
	return nil
}

// resolveCatalogRateRefs fills items that point at a catalog row: the
// item's resource and costs (integer BU) are copied verbatim from the
// resource_rates_catalog entry — a catalog entry is shaped exactly like a
// RateItem — and the ref is kept as provenance. The copy is a snapshot —
// later catalog changes do not reprice existing rates. Hand-written items
// (no catalog_rate_ref) pass through untouched; a ref that does not
// resolve is a client error.
func (h *RatesHandler) resolveCatalogRateRefs(ctx context.Context, items []api.RateItem) *api.Error {
	for i := range items {
		ref := items[i].CatalogRateRef
		if ref == "" {
			continue
		}
		entry, err := h.store.GetCatalogRate(ctx, ref)
		if err != nil {
			return api.NewError(api.ErrBadRequest,
				fmt.Sprintf("catalog_rate_ref %q does not resolve to an existing resource rate", ref))
		}
		items[i].Resource = entry.Spec.Resource
		items[i].Costs = entry.Spec.Costs
	}
	return nil
}
