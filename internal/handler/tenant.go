package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opendatahub-io/ai-metering-service/internal/api"
)

const tenantHeader = "X-Tenant"

func RequireTenant() gin.HandlerFunc {
	return func(c *gin.Context) {
		tenant := c.GetHeader(tenantHeader)
		if tenant == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, api.NewError(api.ErrMissingTenant, "missing X-Tenant header"))
			return
		}
		c.Set("tenant_id", tenant)
		c.Next()
	}
}

func getTenantID(c *gin.Context) string {
	return c.GetString("tenant_id")
}
