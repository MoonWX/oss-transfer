package task

import (
	"encoding/json"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func (c Child) MarshalStateJSON() ([]byte, error) {
	type fields Child
	var record json.Marshaler
	if c.Record != nil {
		record = state.ForStorage(*c.Record)
	}
	return json.Marshal(struct {
		fields
		Job    json.Marshaler `json:"job"`
		Record json.Marshaler `json:"record,omitempty"`
	}{fields(c), state.ForStorage(c.Job), record})
}
