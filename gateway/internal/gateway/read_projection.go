package gateway

import "github.com/silencer/gateway/internal/storage"

// Read projections only replace top-level fields. Nested evidence and items
// remain immutable for this HTTP read; configuration edits use cloneMap.
func cloneReadMetadata(meta map[string]any) map[string]any {
	copy := make(map[string]any, len(meta))
	for key, value := range meta {
		copy[key] = value
	}
	return copy
}

// Keep decoding and historical evidence repair local to a single read. These
// maps are never shared between HTTP requests or cached across config changes.
func prepareReadMetadata(requests []storage.Request, events []storage.Event) {
	for i := range requests {
		requests[i].StatsMeta = requestMeta(requests[i])
		requests[i].StatsMetaCanonical = true
	}
	for i := range events {
		events[i].StatsMeta = eventMeta(events[i])
		events[i].StatsMetaCanonical = true
	}
}
