package server

import (
	"github.com/gin-gonic/gin"
	"github.com/opendatahub-io/ai-metering-service/internal/config"
	"github.com/opendatahub-io/ai-metering-service/internal/handler"
	"github.com/opendatahub-io/ai-metering-service/internal/renewal"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
)

type Server struct {
	engine *gin.Engine
	cfg    *config.Config
}

func New(cfg *config.Config, s store.Store, kv store.KeyStore, renewer *renewal.Renewer) *Server {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	r.GET("/health", handler.NewHealthHandler(renewer).Health)

	v1 := r.Group("/api/v1", handler.RequireTenant())

	rates := handler.NewRatesHandler(s)
	v1.GET("/rates", rates.List)
	v1.GET("/rates/:id", rates.Get)
	v1.POST("/rates", rates.Create)
	v1.PUT("/rates/:id", rates.Update)
	v1.DELETE("/rates/:id", rates.Delete)

	resourceRates := handler.NewCatalogRatesHandler(s)
	v1.GET("/catalog-rates", resourceRates.List)
	v1.GET("/catalog-rates/:id", resourceRates.Get)

	budgets := handler.NewBudgetsHandler(s, kv)
	v1.GET("/budgets", budgets.List)
	v1.GET("/budgets/:id", budgets.Get)
	v1.POST("/budgets", budgets.Create)
	v1.PUT("/budgets/:id", budgets.Update)
	v1.DELETE("/budgets/:id", budgets.Delete)

	policies := handler.NewBudgetPoliciesHandler(s)
	v1.GET("/budget_policies", policies.List)
	v1.GET("/budget_policies/:id", policies.Get)
	v1.POST("/budget_policies", policies.Create)
	v1.PUT("/budget_policies/:id", policies.Update)
	v1.DELETE("/budget_policies/:id", policies.Delete)

	usage := handler.NewUsageHandler(s)
	v1.GET("/usage", usage.Query)
	v1.GET("/usage/me", usage.QueryMe)
	if cfg.Debug {
		v1.DELETE("/usage", usage.DeleteAll)
	}

	events := handler.NewEventsHandler(s, kv)
	v1.POST("/events", events.Ingest)

	return &Server{engine: r, cfg: cfg}
}

func (s *Server) Run(addr string) error {
	if s.cfg.TLS.CertFile != "" && s.cfg.TLS.KeyFile != "" {
		return s.engine.RunTLS(addr, s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
	}

	return s.engine.Run(addr)
}
