package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/redhat-et/pricetag-metering/internal/admission"
	"github.com/redhat-et/pricetag-metering/internal/api"
	"github.com/redhat-et/pricetag-metering/internal/period"
	"github.com/redhat-et/pricetag-metering/internal/renewal"
	"github.com/redhat-et/pricetag-metering/internal/store"
	"github.com/redhat-et/pricetag-metering/internal/utils"
)

type BudgetsHandler struct {
	store   store.Store
	kvStore store.KeyStore
}

func NewBudgetsHandler(s store.Store, kv store.KeyStore) *BudgetsHandler {
	return &BudgetsHandler{store: s, kvStore: kv}
}

func (h *BudgetsHandler) List(c *gin.Context) {
	budgets, err := h.store.ListBudgets(c.Request.Context(), getTenantID(c), utils.ParsePagination(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotListBudgets, "failed to list budgets"))
		return
	}
	c.JSON(http.StatusOK, budgets)
}

func (h *BudgetsHandler) Get(c *gin.Context) {
	budget, err := h.store.GetBudget(c.Request.Context(), getTenantID(c), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, api.NewError(api.ErrCannotGetBudget, "budget not found"))
		return
	}
	c.JSON(http.StatusOK, budget)
}

func (h *BudgetsHandler) Create(c *gin.Context) {
	var budget api.Budget
	if err := c.ShouldBindJSON(&budget); err != nil {
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrBadRequest, err.Error()))
		return
	}
	if _, err := period.Parse(budget.PeriodDuration); err != nil {
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrBadRequest, err.Error()))
		return
	}
	tenantID := getTenantID(c)
	res, err := h.store.CreateBudget(c.Request.Context(), tenantID, &budget,
		func(res *api.BudgetResource) error {
			balanceKey := admission.BuildBalanceKey(tenantID, res.Metadata.ID)
			return h.kvStore.SetKey(c.Request.Context(), tenantID, balanceKey, strconv.FormatInt(res.Spec.Limit, 10))
		})
	if err != nil {
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotCreateBudget, "failed to create budget"))
		return
	}

	c.JSON(http.StatusCreated, res)
}

func (h *BudgetsHandler) Update(c *gin.Context) {
	var budget api.Budget
	if err := c.ShouldBindJSON(&budget); err != nil {
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrBadRequest, err.Error()))
		return
	}
	if _, err := period.Parse(budget.PeriodDuration); err != nil {
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrBadRequest, err.Error()))
		return
	}
	tenantID := getTenantID(c)
	res, _, err := h.store.UpdateBudget(c.Request.Context(), tenantID, c.Param("id"), &budget,
		func(_ int64, res *api.BudgetResource) error {
			// A limit change raises (or lowers) the ceiling going forward;
			// it does not reset the counter. Re-derive balance and flags
			// from the ledger under the new spec — the same derivation
			// renewal and reconcile use — so every writer produces the
			// same value and a later reconcile cannot partially revert
			// the update. Runs inside the update transaction like the
			// create callback: the Redis writes precede the commit, so a
			// crash rolls the claim back rather than leaving the new
			// balance behind an uncommitted spec.
			return renewal.Rebuild(c.Request.Context(), h.store, h.kvStore, *res)
		})
	if err != nil {
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotUpdateBudget, "failed to update budget"))
		return
	}

	c.JSON(http.StatusOK, res)
}

func (h *BudgetsHandler) Delete(c *gin.Context) {
	tenantID := getTenantID(c)
	budgetID := c.Param("id")
	err := h.store.DeleteBudget(c.Request.Context(), tenantID, budgetID,
		func() error {
			balanceKey := admission.BuildBalanceKey(tenantID, budgetID)
			return h.kvStore.DeleteKey(c.Request.Context(), tenantID, balanceKey)
		})
	if err != nil {
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotDeleteBudget, "failed to delete budget"))
		return
	}
	c.Status(http.StatusNoContent)
}
