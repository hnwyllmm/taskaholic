package model

// ModelOption is an adapter-specific model identifier, never a global model ID.
type ModelOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ModelCatalog is discovery metadata, not a promise of account quota or access.
// It does not change a member configuration or a pinned Session.
type ModelCatalog struct {
	Models      []ModelOption `json:"models"`
	Status      string        `json:"status"` // ready, unsupported, unavailable
	CheckedAtMS int64         `json:"checked_at_ms,omitempty"`
}
