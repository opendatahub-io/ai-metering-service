package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/redhat-et/pricetag-metering/internal/api"
	"github.com/redhat-et/pricetag-metering/internal/store"
	"github.com/redhat-et/pricetag-metering/internal/utils"
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
