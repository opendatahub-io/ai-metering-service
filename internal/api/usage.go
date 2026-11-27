package api

type TimeRange string

const (
	Range24h    TimeRange = "24h"
	Range7d     TimeRange = "7d"
	Range30d    TimeRange = "30d"
	Range90d    TimeRange = "90d"
	Range180d   TimeRange = "180d"
	Range365d   TimeRange = "365d"
	RangeMTD    TimeRange = "mtd"
	RangeQTD    TimeRange = "qtd"
	RangeYTD    TimeRange = "ytd"
	RangeCustom TimeRange = "custom"
)

type UsageQuery struct {
	Range       TimeRange
	From        string
	To          string
	GroupBy     []string
	Granularity string
	User        string
	Group       string
	Org         string
	Model       string
	Provider    string
	Budget      string
	Limit       int
	Offset      int
}

type UsageResult struct {
	Range       UsageRange    `json:"range"`
	Granularity string        `json:"granularity"`
	GroupBy     []string      `json:"group_by"`
	Data        []UsageBucket `json:"data"`
}

type UsageRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type UsageBucket struct {
	Bucket     string            `json:"bucket,omitempty"`
	Dimensions map[string]string `json:"dimensions"`
	Requests   int64             `json:"requests"`
	Usage      map[string]int64  `json:"usage"`
	BUCost     int64             `json:"bu_cost"`
}
