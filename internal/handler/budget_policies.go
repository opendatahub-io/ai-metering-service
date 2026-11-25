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

type BudgetPoliciesHandler struct {
	store store.Store
}

func NewBudgetPoliciesHandler(s store.Store) *BudgetPoliciesHandler {
	return &BudgetPoliciesHandler{store: s}
}

func (h *BudgetPoliciesHandler) List(c *gin.Context) {
	policies, err := h.store.ListBudgetPolicies(c.Request.Context(), getTenantID(c), utils.ParsePagination(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotListBudgetPolicies, "failed to list budget policies"))
		return
	}
	c.JSON(http.StatusOK, policies)
}

func (h *BudgetPoliciesHandler) Get(c *gin.Context) {
	policy, err := h.store.GetBudgetPolicy(c.Request.Context(), getTenantID(c), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, api.NewError(api.ErrCannotGetBudgetPolicy, "budget policy not found"))
		return
	}
	c.JSON(http.StatusOK, policy)
}

func (h *BudgetPoliciesHandler) Create(c *gin.Context) {
	var policy api.BudgetPolicy
	if err := c.ShouldBindJSON(&policy); err != nil {
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrBadRequest, err.Error()))
		return
	}
	tenantID := getTenantID(c)
	if err := h.validate(c.Request.Context(), tenantID, &policy); err != nil {
		c.JSON(http.StatusBadRequest, err)
		return
	}
	res, err := h.store.CreateBudgetPolicy(c.Request.Context(), tenantID, &policy)
	if err != nil {
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotCreateBudgetPolicy, "failed to create budget policy"))
		return
	}
	c.JSON(http.StatusCreated, res)
}

func (h *BudgetPoliciesHandler) Update(c *gin.Context) {
	var policy api.BudgetPolicy
	if err := c.ShouldBindJSON(&policy); err != nil {
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrBadRequest, err.Error()))
		return
	}
	tenantID := getTenantID(c)
	if err := h.validate(c.Request.Context(), tenantID, &policy); err != nil {
		c.JSON(http.StatusBadRequest, err)
		return
	}
	res, err := h.store.UpdateBudgetPolicy(c.Request.Context(), tenantID, c.Param("id"), &policy)
	if err != nil {
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotUpdateBudgetPolicy, "failed to update budget policy"))
		return
	}
	c.JSON(http.StatusOK, res)
}

// validate enforces the ADR's policy-creation rules: budget_ref and rate_ref
// must resolve within the tenant, and only soft overage is supported —
// fallback levels (group, organization, tenant) are a non-goal for the
// initial implementation. An overage rate_ref, when present, must also
// resolve.
func (h *BudgetPoliciesHandler) validate(ctx context.Context, tenantID string, policy *api.BudgetPolicy) *api.Error {
	if len(policy.Subjects) == 0 {
		return api.NewError(api.ErrInvalidBudgetPolicy, "at least one subject is required")
	}
	for _, subject := range policy.Subjects {
		if !subject.Kind.Valid() {
			return api.NewError(api.ErrInvalidBudgetPolicy,
				fmt.Sprintf("subject kind %q is not supported; must be one of user, group, organization, tenant", subject.Kind))
		}
		if subject.Name == "" {
			return api.NewError(api.ErrInvalidBudgetPolicy, "subject name must not be empty")
		}
	}
	if _, err := h.store.GetBudget(ctx, tenantID, policy.BudgetRef); err != nil {
		return api.NewError(api.ErrInvalidBudgetPolicy,
			fmt.Sprintf("budget_ref %q does not resolve to an existing budget", policy.BudgetRef))
	}
	if _, err := h.store.GetRate(ctx, tenantID, policy.RateRef); err != nil {
		return api.NewError(api.ErrInvalidBudgetPolicy,
			fmt.Sprintf("rate_ref %q does not resolve to an existing rate", policy.RateRef))
	}
	if policy.Overage != nil {
		if policy.Overage.Type != "soft" {
			return api.NewError(api.ErrInvalidBudgetPolicy,
				fmt.Sprintf("overage type %q is not supported; only \"soft\" is supported", policy.Overage.Type))
		}
		if policy.Overage.RateRef != "" {
			if _, err := h.store.GetRate(ctx, tenantID, policy.Overage.RateRef); err != nil {
				return api.NewError(api.ErrInvalidBudgetPolicy,
					fmt.Sprintf("overage rate_ref %q does not resolve to an existing rate", policy.Overage.RateRef))
			}
		}
	}
	return nil
}

func (h *BudgetPoliciesHandler) Delete(c *gin.Context) {
	if err := h.store.DeleteBudgetPolicy(c.Request.Context(), getTenantID(c), c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotDeleteBudgetPolicy, "failed to delete budget policy"))
		return
	}
	c.Status(http.StatusNoContent)
}
