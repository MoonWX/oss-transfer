package transferstore

import (
	"encoding/json"
	"time"
)

// MarshalStateJSON omits absent timestamps from durable object inventories.
// API encoding remains unchanged because it also participates in fingerprints.
func (o Object) MarshalStateJSON() ([]byte, error) {
	type fields Object
	return json.Marshal(struct {
		fields
		ModifiedAt         time.Time `json:"modifiedAt,omitzero"`
		ExpiresAt          time.Time `json:"expiresAt,omitzero"`
		LifecycleExpiresAt time.Time `json:"lifecycleExpiresAt,omitzero"`
	}{fields(o), o.ModifiedAt, o.ExpiresAt, o.LifecycleExpiresAt})
}
