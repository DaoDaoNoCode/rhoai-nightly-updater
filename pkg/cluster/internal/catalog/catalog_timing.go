package catalog

import "time"

// How long a CatalogSource gets to become READY, how often it is polled
// meanwhile, and how long OLM may take to publish its PackageManifest after
// that. Package-level vars so tests can override them with shorter durations
// to avoid real-time waits.
var (
	ReadyTimeout                   = 120 * time.Second
	PollInterval                   = 5 * time.Second
	PackageManifestPropagationWait = 30 * time.Second
)
