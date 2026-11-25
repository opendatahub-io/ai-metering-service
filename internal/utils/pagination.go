package utils

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
)

func ParsePagination(c *gin.Context) store.Pagination {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	return store.Pagination{Limit: limit, Offset: offset}
}
