package api

import "time"

type Metadata struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Rate struct {
	Name  string     `json:"name"`
	Items []RateItem `json:"items"`
}

type RateItem struct {
	Resource map[string]string `json:"resource"`
	Costs    map[string]int64  `json:"costs"`
	// CatalogRateRef is provenance: the resource_rates_catalog row this item was
	// copied from. Empty on hand-written items. Kept alongside the copied
	// values so a later refresh can re-copy from the current catalog entry.
	// Ignored by matching — it never affects pricing.
	CatalogRateRef string `json:"catalog_rate_ref,omitempty"`
}

type RateResource struct {
	Metadata Metadata `json:"metadata"`
	Spec     Rate     `json:"spec"`
}

// CatalogRate is one catalog entry: pricing in integer BU per usage unit
// for one resource identity. Its shape is deliberately a RateItem — a rate
// item copied from the catalog is a verbatim copy of resource and costs.
// The resource map carries any dimensions (model, provider, modality,
// region, ...), but every dimension present becomes a required match key
// in the copied item (subset matching): an entry must only carry the
// dimensions the events actually carry, or its copies never match. Catalog
// entries are templates rates are copied from — never referenced by the
// accounting path.
type CatalogRate struct {
	Resource map[string]string `json:"resource"`
	Costs    map[string]int64  `json:"costs"`
}

type ResourceCatalogRate struct {
	Metadata Metadata    `json:"metadata"`
	Spec     CatalogRate `json:"spec"`
}

type Budget struct {
	Name           string    `json:"name"`
	Limit          int64     `json:"limit"`
	PeriodStart    time.Time `json:"period_start"`
	PeriodDuration string    `json:"period_duration"`
	RenewalType    string    `json:"renewal_type"`
	MaxCarry       *int64    `json:"max_carry,omitempty"`

	// NextRenewalAt is the next period boundary at which the Metering
	// Service refills the balance. It is derived from PeriodStart and
	// PeriodDuration by the store on create and update, and advanced by the
	// renewal loop — never set by API clients, and not part of the API
	// contract, hence json:"-".
	NextRenewalAt time.Time `json:"-"`
}

// Budget renewal types (Budget.RenewalType): how the balance resets at a
// period boundary.
const (
	// RenewalDrain resets the balance to the limit each period — use it or
	// lose it.
	RenewalDrain = "drain"
	// RenewalRollover carries unused budget units forward, optionally
	// capped by Budget.MaxCarry.
	RenewalRollover = "rollover"
)

type BudgetResource struct {
	Metadata Metadata `json:"metadata"`
	Spec     Budget   `json:"spec"`
}

type BudgetPolicy struct {
	BudgetRef     string         `json:"budget_ref"`
	RateRef       string         `json:"rate_ref"`
	Subjects      []Subject      `json:"subjects"`
	Overage       *OverageConfig `json:"overage,omitempty"`
	DeductOverage bool           `json:"deduct_overage"`
}

type BudgetPolicyResource struct {
	Metadata Metadata     `json:"metadata"`
	Spec     BudgetPolicy `json:"spec"`
}

// SubjectKind is the closed set of identity kinds a policy subject can have.
// The values are also the admission key segments (admit:{kind}:{name}), which
// the Rust admission crate must mirror exactly.
type SubjectKind string

const (
	SubjectKindUser         SubjectKind = "user"
	SubjectKindGroup        SubjectKind = "group"
	SubjectKindOrganization SubjectKind = "organization"
	SubjectKindTenant       SubjectKind = "tenant"
)

// Valid reports whether the kind is one of the supported subject kinds.
func (k SubjectKind) Valid() bool {
	switch k {
	case SubjectKindUser, SubjectKindGroup, SubjectKindOrganization, SubjectKindTenant:
		return true
	}
	return false
}

type Subject struct {
	Kind SubjectKind `json:"kind"`
	Name string      `json:"name"`
}

type OverageConfig struct {
	Type    string `json:"type"`
	RateRef string `json:"rate_ref,omitempty"`
}

// UsageRecord is one row of the usage table: event-level facts, 1:1 with the
// CloudEvent. Per-charge facts live in UsageCharge.
type UsageRecord struct {
	ID        int64             `json:"id"`
	TenantID  string            `json:"tenant_id"`
	EventID   string            `json:"event_id"`
	RequestID string            `json:"request_id"`
	Timestamp time.Time         `json:"timestamp"`
	Identity  map[string]any    `json:"identity"`
	Resource  map[string]string `json:"resource"`
	Usage     map[string]int64  `json:"usage"`
}

// UsageCharge is one row of the usage_charges table: the budget a usage
// event was charged to and what it cost there. The (usage_id, budget_ref)
// unique constraint is the per-charge idempotency marker.
type UsageCharge struct {
	UsageID   int64  `json:"usage_id"`
	BudgetRef string `json:"budget_ref"`
	BUCost    int64  `json:"bu_cost"`
}

type Identity struct {
	User         string   `json:"user"`
	Groups       []string `json:"groups,omitempty"`
	Organization string   `json:"organization,omitempty"`
	Tenant       string   `json:"tenant,omitempty"`
}

// ChargeTarget is one (identity, resource) pair that has been charged
// against a budget — the unit of admission-flag recompute after a budget's
// balance changes other than through a usage event (renewal, reconciliation).
type ChargeTarget struct {
	Identity Identity
	Resource map[string]string
}

type Listing[T any] struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
	Total  int64 `json:"total"`
	Items  []T   `json:"items"`
}
