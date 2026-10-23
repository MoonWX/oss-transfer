package task

import (
	"encoding/json"
	"errors"
	"reflect"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// The Agent record carries the same immutable assignment as Child.Job. Keep
// one copy on disk and restore it on read; public JSON remains unchanged.
func (c Child) MarshalStateJSON() ([]byte, error) {
	type fields Child
	var record json.Marshaler
	referenced := false
	if c.Record != nil {
		if c.Job.ChildID != "" && reflect.DeepEqual(c.Job, c.Record.Job) {
			raw, err := c.Record.MarshalStateJSONWithoutJob()
			if err != nil {
				return nil, err
			}
			record, referenced = json.RawMessage(raw), true
		} else {
			record = state.ForStorage(*c.Record)
		}
	}
	return json.Marshal(struct {
		fields
		Job          json.Marshaler `json:"job"`
		Record       json.Marshaler `json:"record,omitempty"`
		RecordJobRef bool           `json:"recordJobRef,omitempty"`
	}{fields(c), state.ForStorage(c.Job), record, referenced})
}

func (c *Child) UnmarshalJSON(data []byte) error {
	type fields Child
	var stored struct {
		fields
		RecordJobRef bool `json:"recordJobRef,omitempty"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	if stored.RecordJobRef {
		if stored.Record == nil || stored.Job.ChildID == "" || stored.Record.Job.ChildID != "" {
			return errors.New("invalid stored child assignment reference")
		}
		var shape struct {
			Record map[string]json.RawMessage `json:"record"`
		}
		if err := json.Unmarshal(data, &shape); err != nil {
			return err
		}
		if _, containsJob := shape.Record["job"]; containsJob {
			return errors.New("stored child assignment reference also contains a job")
		}
		// Decode a separate copy so callers can still modify the two snapshots
		// independently, as they could before this storage optimization.
		raw, err := json.Marshal(stored.Job)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &stored.Record.Job); err != nil {
			return err
		}
	}
	*c = Child(stored.fields)
	return nil
}
