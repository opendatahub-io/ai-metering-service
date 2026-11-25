package utils

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/redhat-et/pricetag-metering/internal/store"
)

func ParsePagination(c *gin.Context) store.Pagination {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	return store.Pagination{Limit: limit, Offset: offset}
}
