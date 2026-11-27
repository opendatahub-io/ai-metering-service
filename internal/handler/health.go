package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opendatahub-io/ai-metering-service/internal/balance"
)

type HealthHandler struct {
	renewer *balance.Renewer
}

func NewHealthHandler(renewer *balance.Renewer) *HealthHandler {
	return &HealthHandler{renewer: renewer}
}

// Health reports service liveness, including the renewal loops' last-pass
// status and configured intervals: a last_sweep_at or last_reconcile_at
// older than a couple of the respective intervals means that loop is stuck
// and admission flags are frozen at their last values. A zero timestamp
// means the pass has not run in this pod yet. The intervals let a watcher
// (probe, test, operator) know how stale a timestamp may legitimately be
// before it indicates a stuck loop.
func (h *HealthHandler) Health(c *gin.Context) {
	resp := gin.H{
		"status":  "ok",
		"service": "MeteringService",
	}
	if h.renewer != nil {
		sweep := h.renewer.Status()
		reconcile := h.renewer.LastReconcile()
		renewalStatus := gin.H{
			"last_sweep_at":      sweep.At,
			"renewed":            sweep.Renewed,
			"interval":           h.renewer.Interval().String(),
			"last_reconcile_at":  reconcile.At,
			"reconciled":         reconcile.Reconciled,
			"reconcile_interval": h.renewer.ReconcileInterval().String(),
		}
		if sweep.Err != "" {
			renewalStatus["error"] = sweep.Err
		}
		if reconcile.Err != "" {
			renewalStatus["reconcile_error"] = reconcile.Err
		}
		resp["renewal"] = renewalStatus
	}
	c.JSON(http.StatusOK, resp)
}
