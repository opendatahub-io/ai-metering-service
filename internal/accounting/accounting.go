package accounting

import (
	"context"
	"fmt"
	"slices"

	"github.com/opendatahub-io/ai-metering-service/internal/admission"
	"github.com/opendatahub-io/ai-metering-service/internal/api"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
)

// Applicable is one policy that governs an (identity, resource) pair: the
// policy, its rate item matching the resource, and the admission flag keys
// for the subjects the identity matches. It is the shared resolution step
// for both paths that need it: the ingest path (which charges the event's
// cost against the policy's budget) and the renewal recompute (which
// derives the admission state from the budget's current balance).
type Applicable struct {
	Policy   api.BudgetPolicyResource
	RateItem api.RateItem
	// AdmissionKeys holds the admission flag keys for every identity object
	// the policy's subjects matched, combined with the resource. Key names
	// only — values are derived later, from balances.
	AdmissionKeys []string
}

// ResolveApplicable returns every policy that governs the (identity,
// resource) pair, with its matching rate item and admission keys. Policies
// whose rate does not cover the resource contribute nothing and are
// skipped. Resolution is one round trip: policies arrive joined with their
// rate's items.
func ResolveApplicable(ctx context.Context, s store.Store, tenantID string, identity api.Identity, resource map[string]string) ([]Applicable, error) {
	resolved, err := s.FindPoliciesWithRates(ctx, tenantID, identity)
	if err != nil {
		return nil, fmt.Errorf("find policies: %w", err)
	}
	if len(resolved) == 0 {
		return nil, nil
	}

	var applicable []Applicable
	for _, entry := range resolved {
		policy := entry.Policy
		item, ok := MatchRateItem(entry.Items, resource)
		if !ok {
			continue
		}

		keys := make([]string, 0)
		for _, subject := range MatchSubjects(policy.Spec.Subjects, identity) {
			keys = append(keys, admission.BuildAdmissionKey(tenantID, subject, resource))
		}

		applicable = append(applicable, Applicable{
			Policy:        policy,
			RateItem:      item,
			AdmissionKeys: keys,
		})
	}

	return applicable, nil
}

// ResolvedCharge is an Applicable policy plus the BU cost the event's usage
// incurs under its rate item.
type ResolvedCharge struct {
	Applicable
	BUCost int64
}

func ResolveCharges(ctx context.Context, s store.Store, tenantID string, event *api.CloudEvent) ([]ResolvedCharge, error) {
	identity := api.Identity{
		User:         event.Data.Identity.User,
		Groups:       event.Data.Identity.Groups,
		Organization: event.Data.Identity.Organization,
		Tenant:       event.Data.Identity.Tenant,
	}

	applicable, err := ResolveApplicable(ctx, s, tenantID, identity, event.Data.Resource)
	if err != nil {
		return nil, err
	}

	var charges []ResolvedCharge
	for _, a := range applicable {
		charges = append(charges, ResolvedCharge{
			Applicable: a,
			BUCost:     ComputeBUCost(event.Data.Usage, a.RateItem.Costs),
		})
	}

	return charges, nil
}

// MatchSubjects returns the policy subjects that match the event's identity.
// A subject matches when its kind and name equal the corresponding identity
// dimension; group membership is only known at request time, so this is the
// sole place where policy subjects and event identities are intersected.
// A policy returned by FindPoliciesForIdentity always has at least one
// matching subject.
func MatchSubjects(subjects []api.Subject, identity api.Identity) []api.Subject {
	matched := make([]api.Subject, 0, len(subjects))
	for _, subject := range subjects {
		switch subject.Kind {
		case api.SubjectKindUser:
			if subject.Name == identity.User {
				matched = append(matched, subject)
			}
		case api.SubjectKindGroup:
			if slices.Contains(identity.Groups, subject.Name) {
				matched = append(matched, subject)
			}
		case api.SubjectKindOrganization:
			if subject.Name == identity.Organization {
				matched = append(matched, subject)
			}
		case api.SubjectKindTenant:
			if subject.Name == identity.Tenant {
				matched = append(matched, subject)
			}
		}
	}
	return matched
}

// MatchRateItem finds the best matching rate item for the given resource keys.
// An item matches when every key in its resource map equals the corresponding
// event resource key — a subset constraint: keys absent from the item are
// unconstrained, and an empty resource map matches every resource (the
// catch-all). There are no wildcards: an unconstrained dimension is expressed
// by leaving the key out, never with a "*" value (rate validation rejects
// those). Among all matches, the item with the most keys — the tightest
// constraint set — wins; ties keep the first item in the list.
func MatchRateItem(items []api.RateItem, resource map[string]string) (api.RateItem, bool) {
	bestIdx := -1
	bestKeys := -1

	for i, item := range items {
		matched := true
		for key, value := range item.Resource {
			val, exists := resource[key]
			if !exists || val != value {
				matched = false
				break
			}
		}
		if matched && len(item.Resource) > bestKeys {
			bestIdx = i
			bestKeys = len(item.Resource)
		}
	}

	if bestIdx < 0 {
		return api.RateItem{}, false
	}
	return items[bestIdx], true
}

// ComputeBUCost calculates cost_bu = sum(usage[key] * costs[key]) for all
// keys present in the costs map.
func ComputeBUCost(usage map[string]int64, costs map[string]int64) int64 {
	var total int64
	for key, rate := range costs {
		if count, ok := usage[key]; ok {
			total += count * rate
		}
	}
	return total
}
