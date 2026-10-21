package discovery

import (
	"encoding/json"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func (b Batch) MarshalStateJSON() ([]byte, error) {
	type fields Batch
	var objects []json.Marshaler
	if b.Objects != nil {
		objects = make([]json.Marshaler, len(b.Objects))
		for i, object := range b.Objects {
			objects[i] = state.ForStorage(object)
		}
	}
	return json.Marshal(struct {
		fields
		Objects []json.Marshaler `json:"objects"`
	}{fields(b), objects})
}

func (o discoveredObject) MarshalStateJSON() ([]byte, error) {
	type fields discoveredObject
	return json.Marshal(struct {
		fields
		Object json.Marshaler `json:"object"`
	}{fields(o), state.ForStorage(o.Object)})
}
