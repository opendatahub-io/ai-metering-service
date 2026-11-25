package api

import "time"

type CloudEvent struct {
	SpecVersion     string         `json:"specversion"`
	ID              string         `json:"id"`
	Source          string         `json:"source"`
	Type            string         `json:"type"`
	Time            time.Time      `json:"time"`
	DataContentType string         `json:"datacontenttype,omitempty"`
	Data            CloudEventData `json:"data"`
}

type CloudEventData struct {
	RequestID string            `json:"request_id"`
	Identity  Identity          `json:"identity"`
	Resource  map[string]string `json:"resource"`
	Usage     map[string]int64  `json:"usage"`
}
