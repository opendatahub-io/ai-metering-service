package handler

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/opendatahub-io/ai-metering-service/internal/api"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
	"github.com/opendatahub-io/ai-metering-service/internal/utils"
)

type UsageHandler struct {
	store store.Store
}

func NewUsageHandler(s store.Store) *UsageHandler {
	return &UsageHandler{store: s}
}

func (h *UsageHandler) Query(c *gin.Context) {
	query := buildUsageQuery(c)
	if err := validateGroupBy(query.GroupBy); err != nil {
		c.JSON(http.StatusBadRequest, err)
		return
	}
	result, err := h.store.QueryUsage(c.Request.Context(), getTenantID(c), query)
	if err != nil {
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrCannotQueryUsage, err.Error()))
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *UsageHandler) QueryMe(c *gin.Context) {
	query := buildUsageQuery(c)
	query.User = c.GetString("user")
	query.Group = ""
	query.Org = ""

	for _, dim := range query.GroupBy {
		if dim == "user" || dim == "group" || dim == "org" {
			c.JSON(http.StatusBadRequest, api.NewError(api.ErrInvalidGroupBy, "identity dimensions not allowed in /usage/me group_by"))
			return
		}
	}
	if err := validateGroupBy(query.GroupBy); err != nil {
		c.JSON(http.StatusBadRequest, err)
		return
	}

	result, err := h.store.QueryUsage(c.Request.Context(), getTenantID(c), query)
	if err != nil {
		c.JSON(http.StatusBadRequest, api.NewError(api.ErrCannotQueryUsage, err.Error()))
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *UsageHandler) DeleteAll(c *gin.Context) {
	deleted, err := h.store.DeleteAllUsage(c.Request.Context(), getTenantID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, api.NewError(api.ErrCannotDeleteUsage, err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": deleted})
}

// groupByDimPattern bounds group_by dimensions to word characters, dashes
// and underscores: the dimension is embedded in the store's SQL (both as a
// string literal and as a quoted identifier), so anything else is a
// rejection, not a query.
var groupByDimPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// validateGroupBy rejects dimensions that cannot be safely embedded in the
// store's SQL. Returns nil when every dimension is well-formed.
func validateGroupBy(groupBy []string) *api.Error {
	for _, dim := range groupBy {
		if !groupByDimPattern.MatchString(dim) {
			return api.NewError(api.ErrInvalidGroupBy,
				fmt.Sprintf("invalid group_by dimension %q: dimensions must be word characters, dashes or underscores", dim))
		}
	}
	return nil
}

func buildUsageQuery(c *gin.Context) *api.UsageQuery {
	pg := utils.ParsePagination(c)

	var groupBy []string
	if g := c.Query("group_by"); g != "" {
		groupBy = strings.Split(g, ",")
	}

	return &api.UsageQuery{
		Range:       api.TimeRange(c.Query("range")),
		From:        c.Query("from"),
		To:          c.Query("to"),
		GroupBy:     groupBy,
		Granularity: c.Query("granularity"),
		User:        c.Query("user"),
		Group:       c.Query("group"),
		Org:         c.Query("org"),
		Model:       c.Query("model"),
		Provider:    c.Query("provider"),
		Budget:      c.Query("budget"),
		Limit:       pg.Limit,
		Offset:      pg.Offset,
	}
}
