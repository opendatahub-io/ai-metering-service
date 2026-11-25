package api

import "fmt"

const (
	ErrInternal                 = "internal"
	ErrBadRequest               = "bad_request"
	ErrMissingTenant            = "missing_tenant"
	ErrTenantMismatch           = "tenant_mismatch"
	ErrCannotSetBalance         = "cannot_set_balance"
	ErrCannotSetAdmission       = "cannot_set_admission"
	ErrCannotListRates          = "cannot_list_rates"
	ErrCannotGetRate             = "cannot_get_rate"
	ErrCannotCreateRate          = "cannot_create_rate"
	ErrCannotUpdateRate          = "cannot_update_rate"
	ErrCannotDeleteRate          = "cannot_delete_rate"
	ErrCannotListBudgets        = "cannot_list_budgets"
	ErrCannotGetBudget          = "cannot_get_budget"
	ErrCannotCreateBudget       = "cannot_create_budget"
	ErrCannotUpdateBudget       = "cannot_update_budget"
	ErrCannotDeleteBudget       = "cannot_delete_budget"
	ErrCannotListBudgetPolicies = "cannot_list_budget_policies"
	ErrCannotGetBudgetPolicy    = "cannot_get_budget_policy"
	ErrInvalidBudgetPolicy      = "invalid_budget_policy"
	ErrCannotCreateBudgetPolicy = "cannot_create_budget_policy"
	ErrCannotUpdateBudgetPolicy = "cannot_update_budget_policy"
	ErrCannotDeleteBudgetPolicy = "cannot_delete_budget_policy"
	ErrCannotResolveCharges     = "cannot_resolve_charges"
	ErrCannotPersistUsage       = "cannot_persist_usage"
	ErrCannotUpdateBalance      = "cannot_update_balance"
	ErrCannotQueryUsage         = "cannot_query_usage"
	ErrCannotDeleteUsage        = "cannot_delete_usage"
	ErrInvalidGroupBy           = "invalid_group_by"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}
