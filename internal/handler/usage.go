package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/redhat-et/pricetag-metering/internal/api"
	"github.com/redhat-et/pricetag-metering/internal/store"
	"github.com/redhat-et/pricetag-metering/internal/utils"
)

type UsageHandler struct {
	store store.Store
}

func NewUsageHandler(s store.Store) *UsageHandler {
	return &UsageHandler{store: s}
}

func (h *UsageHandler) Query(c *gin.Context) {
	query := buildUsageQuery(c)
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
		Limit:       pg.Limit,
		Offset:      pg.Offset,
	}
}
