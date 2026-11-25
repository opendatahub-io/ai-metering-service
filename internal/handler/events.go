package handler

import (
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/redhat-et/pricetag-metering/internal/accounting"
	"github.com/redhat-et/pricetag-metering/internal/admission"
	"github.com/redhat-et/pricetag-metering/internal/api"
	"github.com/redhat-et/pricetag-metering/internal/store"
)

type EventsHandler struct {
	store   store.Store
	kvStore store.KeyStore
}

func NewEventsHandler(s store.Store, kv store.KeyStore) *EventsHandler {
	return &EventsHandler{store: s, kvStore: kv}
}

func (h *EventsHandler) Ingest(c *gin.Context) {
	var event api.CloudEvent
	if err := c.ShouldBindJSON(&event); err != nil {
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrBadRequest, err.Error()))
		return
	}

	if event.SpecVersion != "1.0" {
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrBadRequest, "unsupported specversion"))
		return
	}
	if event.ID == "" || event.Source == "" || event.Type == "" {
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrBadRequest, "missing required CloudEvent fields"))
		return
	}
	if event.Data.Identity.User == "" || event.Data.Identity.Tenant == "" {
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrBadRequest, "missing required identity fields"))
		return
	}

	tenantID := getTenantID(c)

	// CloudEvent tenant binding: the tenant is derived from the authenticated
	// caller (the X-Tenant header today, the ServiceAccount in-cluster later),
	// never from the payload. An event whose payload tenant disagrees with the
	// binding is rejected — otherwise tenant-subject matching would silently
	// miss (or cross) budgets and admission keys scoped to the wrong tenant.
	if event.Data.Identity.Tenant != tenantID {
		log.Printf("tenant mismatch for event %s: payload %q, authenticated %q", event.ID, event.Data.Identity.Tenant, tenantID)
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrTenantMismatch, "identity.tenant does not match the authenticated tenant"))
		return
	}

	charges, err := accounting.ResolveCharges(c.Request.Context(), h.store, tenantID, &event)
	if err != nil {
		log.Printf("resolve charges for event %s: %v", event.ID, err)
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotResolveCharges, "failed to resolve charges"))
		return
	}

	// Pass 1 — account: persist the event, its per-budget charges, and
	// decrement balance counters. Balances may go negative — that is
	// overage, not an error. Each charge's row is inserted immediately
	// before its decrement, so a retried CloudEvent (duplicate) never
	// charges twice, and duplicate charges are excluded from the flag
	// recompute below.
	var accounted []accounting.ResolvedCharge
	balances := make(map[string]int64) // budget ref -> balance after all decrements
	if len(charges) > 0 {
		record := &api.UsageRecord{
			TenantID:  tenantID,
			EventID:   event.ID,
			RequestID: event.Data.RequestID,
			Timestamp: event.Time,
			Identity: map[string]any{
				"user":         event.Data.Identity.User,
				"groups":       event.Data.Identity.Groups,
				"organization": event.Data.Identity.Organization,
			},
			Resource: event.Data.Resource,
			Usage:    event.Data.Usage,
		}
		usageID, err := h.store.InsertUsage(c.Request.Context(), record)
		if err != nil {
			log.Printf("insert usage for event %s: %v", event.ID, err)
			c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotPersistUsage, "failed to persist usage"))
			return
		}

		for _, charge := range charges {
			inserted, err := h.store.InsertUsageCharge(c.Request.Context(), usageID, &api.UsageCharge{
				BudgetRef: charge.Policy.Spec.BudgetRef,
				BUCost:    charge.BUCost,
			})
			if err != nil {
				log.Printf("insert usage charge for event %s budget %s: %v", event.ID, charge.Policy.Spec.BudgetRef, err)
				c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotPersistUsage, "failed to persist usage"))
				return
			}
			if !inserted {
				log.Printf("duplicate event %s for budget %s, skipping", event.ID, charge.Policy.Spec.BudgetRef)
				continue
			}

			balanceKey := admission.BuildBalanceKey(tenantID, charge.Policy.Spec.BudgetRef)
			remaining, err := h.kvStore.DecrKey(c.Request.Context(), tenantID, balanceKey, charge.BUCost)
			if err != nil {
				log.Printf("decrement balance for event %s budget %s: %v", event.ID, charge.Policy.Spec.BudgetRef, err)
				c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotUpdateBalance, "failed to update balance"))
				return
			}
			balances[charge.Policy.Spec.BudgetRef] = remaining
			if remaining < 0 {
				log.Printf("overage for event %s budget %s: %d", event.ID, charge.Policy.Spec.BudgetRef, remaining)
			}
			accounted = append(accounted, charge)
		}
	}

	// Pass 2 — fold: one admission state per key, most restrictive across
	// every accounted charge that carries it (deny > over > allow). Folding
	// after all decrements keeps the outcome independent of charge order and
	// correct when several charges share a budget or a key.
	states := make(map[string]admission.State)
	for _, charge := range accounted {
		state := admission.StateForBalance(balances[charge.Policy.Spec.BudgetRef], charge.Policy.Spec.Overage)
		for _, key := range charge.AdmissionKeys {
			states[key] = admission.MostRestrictive(states[key], state)
		}
	}

	// Pass 3 — publish: write only the flags that changed. Writing allow
	// again is what un-denies an identity whose budget was topped up or
	// renewed; every accounting pass rewrites the flag from current state,
	// so a stale deny cannot outlive the debt that produced it.
	for key, state := range states {
		current, err := h.kvStore.GetKey(c.Request.Context(), tenantID, key)
		if err != nil && !errors.Is(err, store.ErrKeyNotFound) {
			log.Printf("read admission flag %s for event %s: %v", key, event.ID, err)
			c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotSetAdmission, "failed to read admission flag"))
			return
		}
		if current == string(state) {
			continue
		}
		if err := h.kvStore.SetKey(c.Request.Context(), tenantID, key, string(state)); err != nil {
			log.Printf("set admission flag %s for event %s: %v", key, event.ID, err)
			c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotSetAdmission, "failed to set admission flag"))
			return
		}
	}

	c.Status(http.StatusNoContent)
}
