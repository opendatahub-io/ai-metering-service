package handler

import (
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opendatahub-io/ai-metering-service/internal/accounting"
	"github.com/opendatahub-io/ai-metering-service/internal/admission"
	"github.com/opendatahub-io/ai-metering-service/internal/api"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
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

	// Pass 1 — account: persist the event and its per-budget charges in
	// one store round trip, then decrement balance counters. Balances may
	// go negative — that is overage, not an error. SettleUsage returns the
	// budgets whose charges were newly settled; duplicates — a redelivered
	// event, or another policy resolving to the same budget — settle no
	// ledger row and move no balance, exactly like the per-charge loop
	// this replaces. The flag fold below still covers every matching
	// policy's identity objects.
	balances := make(map[string]int64)    // budget ref -> balance after all decrements
	chargedCost := make(map[string]int64) // budget ref -> BUCost applied by the winning charge
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
		usageCharges := make([]api.UsageCharge, len(charges))
		for i, charge := range charges {
			usageCharges[i] = api.UsageCharge{
				BudgetRef: charge.Policy.Spec.BudgetRef,
				BUCost:    charge.BUCost,
			}
		}

		settled, err := h.store.SettleUsage(c.Request.Context(), record, usageCharges)
		if err != nil {
			log.Printf("settle usage for event %s: %v", event.ID, err)
			c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotPersistUsage, "failed to persist usage"))
			return
		}
		settledRef := make(map[string]bool, len(settled))
		for _, ref := range settled {
			settledRef[ref] = true
		}

		// The settling charge for each budget is the first in resolution
		// order — SettleUsage settles charges in argument order, so the
		// earliest policy for a budget prices the event (the deterministic
		// oldest-policy rule FindPoliciesWithRates orders by).
		winner := make(map[string]int, len(settled))
		for i, charge := range charges {
			ref := charge.Policy.Spec.BudgetRef
			if settledRef[ref] {
				if _, ok := chargedCost[ref]; !ok {
					chargedCost[ref] = charge.BUCost
					winner[ref] = i
				}
			}
		}
		// A charge that did not settle its budget is either part of a
		// redelivery (nothing settled) or an overlapping policy (a
		// different charge won the budget). The two are distinguishable
		// here: an overlapping policy's charge was resolved in this same
		// pass, so the winning cost is known. Equal costs are benign —
		// either rate prices the event identically. Differing costs mean
		// two policies share this budget with different rates, and the
		// earlier one priced the event: an ambiguous configuration the
		// operator must resolve. A full redelivery settles nothing, so it
		// warns about nothing.
		for i, charge := range charges {
			ref := charge.Policy.Spec.BudgetRef
			if settledRef[ref] && winner[ref] == i {
				continue // this charge settled the budget
			}
			if cost, ok := chargedCost[ref]; ok && cost != charge.BUCost {
				log.Printf("WARNING: ambiguous pricing for event %s: budget %s charged %d BU by an earlier matching policy, but policy %s (rate %s) computes %d BU for the same usage — two policies share this budget with different rates; align their rate_refs",
					event.ID, ref, cost,
					charge.Policy.Metadata.ID, charge.Policy.Spec.RateRef, charge.BUCost)
			}
			log.Printf("event %s already charged budget %s (redelivery or overlapping policy), skipping ledger row and balance update", event.ID, ref)
		}

		decremented := make(map[string]bool)
		for _, charge := range charges {
			ref := charge.Policy.Spec.BudgetRef
			if !settledRef[ref] || decremented[ref] {
				continue
			}
			decremented[ref] = true

			balanceKey := admission.BuildBalanceKey(tenantID, ref)
			remaining, err := h.kvStore.DecrKey(c.Request.Context(), tenantID, balanceKey, chargedCost[ref])
			if err != nil {
				log.Printf("decrement balance for event %s budget %s: %v", event.ID, ref, err)
				c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotUpdateBalance, "failed to update balance"))
				return
			}
			balances[ref] = remaining
			if remaining < 0 {
				log.Printf("overage for event %s budget %s: %d", event.ID, ref, remaining)
			}
		}
	}

	// Pass 2 — fold: one admission state per key, most restrictive across
	// every resolved charge that carries it (deny > over > allow). The fold
	// iterates all resolved charges, not just the ones whose ledger row was
	// inserted: two policies sharing a budget collapse to a single
	// decrement, but the state derives from that budget's post-decrement
	// balance, so every identity object any matching policy names still
	// gets its flag rewritten, under its own policy's overage semantics.
	// Folding after all decrements keeps the outcome independent of charge
	// order. The balances membership check keeps a full redelivery a no-op:
	// no budget was decremented, so no flags are written and the first
	// delivery's writes stand.
	states := make(map[string]admission.State)
	for _, charge := range charges {
		balance, ok := balances[charge.Policy.Spec.BudgetRef]
		if !ok {
			continue
		}
		state := admission.StateForBalance(balance, charge.Policy.Spec.Overage)
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
