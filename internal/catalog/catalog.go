package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strings"

	"github.com/opendatahub-io/ai-metering-service/internal/api"
	"github.com/opendatahub-io/ai-metering-service/internal/config"
	"github.com/opendatahub-io/ai-metering-service/internal/store"
)

type CatalogEntry struct {
	OutputCostPerReasoningToken *float64 `json:"output_cost_per_reasoning_token"`
	InputCostPerToken           *float64 `json:"input_cost_per_token"`
	OutputCostPerToken          *float64 `json:"output_cost_per_token"`
	CacheReadInputTokenCost     *float64 `json:"cache_read_input_token_cost"`
	CacheWriteInputTokenCost    *float64 `json:"cache_write_input_token_cost"`
}

type Loader interface {
	Load() (io.Reader, error)
}

type FileLoader struct {
	File string
}

func (l *FileLoader) Load() (io.Reader, error) {
	return os.Open(l.File)
}

// buCost multiplies a USD-per-unit cost by the BU scale and rounds up, so
// a converted cost never undercharges: a fractional BU becomes the next
// whole unit. A nil cost (the field absent from the source) contributes 0
// — an explicit zero in the source stays a real zero rate.
func buCost(cost *float64, costToBu int32) int64 {
	if cost == nil {
		return 0
	}
	return int64(math.Ceil(*cost * float64(costToBu)))
}

// ImportCatalog decodes the pricing catalog and upserts every
// "provider/model"-keyed entry as a CatalogRate with integer BU costs.
// Entries with other key shapes (metadata, bare model names, or
// multi-segment prefixes) are skipped. A changed pricing file reprices the
// catalog: entries whose costs changed are updated, so future rate copies
// pick up the new prices — existing rate copies are snapshots and are
// never touched. An unchanged file writes nothing.
func ImportCatalog(loader Loader, cfg *config.Config, store store.Store) error {
	reader, err := loader.Load()
	if err != nil {
		return err
	}

	var catalog map[string]CatalogEntry
	if err := json.NewDecoder(reader).Decode(&catalog); err != nil {
		return fmt.Errorf("decode catalog: %w", err)
	}

	var written, unchanged int
	for key, entry := range catalog {
		split := strings.Split(key, "/")
		if len(split) != 2 {
			continue // not a provider/model key
		}
		provider, model := split[0], split[1]

		rate := api.CatalogRate{
			Resource: map[string]string{
				"provider": provider,
				"model":    model,
			},
			Costs: map[string]int64{
				"input_tokens":       buCost(entry.InputCostPerToken, cfg.Catalog.CostToBu),
				"output_tokens":      buCost(entry.OutputCostPerToken, cfg.Catalog.CostToBu),
				"cache_read_tokens":  buCost(entry.CacheReadInputTokenCost, cfg.Catalog.CostToBu),
				"cache_write_tokens": buCost(entry.CacheWriteInputTokenCost, cfg.Catalog.CostToBu),
				"reasoning_tokens":   buCost(entry.OutputCostPerReasoningToken, cfg.Catalog.CostToBu),
			},
		}

		res, err := store.ApplyCatalogRate(context.Background(), &rate)
		if err != nil {
			log.Printf("failed to import catalog entry %q: %v. Continuing ...", key, err)
			continue
		}
		if res != nil {
			written++
		} else {
			unchanged++
		}
	}
	log.Printf("catalog import: %d entries written, %d unchanged", written, unchanged)
	return nil
}
