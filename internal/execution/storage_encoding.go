package execution

import (
	"encoding/json"
	"reflect"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func (j Job) MarshalStateJSON() ([]byte, error) {
	type fields Job
	type itemFields Item
	type storedItem struct {
		itemFields
		Source json.Marshaler `json:"source"`
	}
	var items []storedItem
	if j.Items != nil {
		items = make([]storedItem, len(j.Items))
		for i, item := range j.Items {
			items[i] = storedItem{itemFields(item), state.ForStorage(item.Source)}
		}
	}
	return json.Marshal(struct {
		fields
		Items []storedItem `json:"items"`
	}{fields(j), items})
}

// MarshalStateJSON leaves the public Record representation and its immutable
// fingerprint unchanged. Missing targets decode to the same zero Object.
func (r Record) MarshalStateJSON() ([]byte, error) { return r.marshalStateJSON(true) }

// MarshalStateJSONWithoutJob lets the Master store its existing Child.Job once.
// The containing record must carry an explicit reference and restore that Job.
func (r Record) MarshalStateJSONWithoutJob() ([]byte, error) { return r.marshalStateJSON(false) }

func (r Record) marshalStateJSON(includeJob bool) ([]byte, error) {
	type fields Record
	type resultFields ItemResult
	type storedResult struct {
		resultFields
		Target         json.Marshaler `json:"target,omitempty"`
		ResolvedSource json.Marshaler `json:"resolvedSource,omitempty"`
	}
	var results []storedResult
	if r.Results != nil {
		results = make([]storedResult, len(r.Results))
		for i, result := range r.Results {
			results[i].resultFields = resultFields(result)
			if !reflect.ValueOf(result.Target).IsZero() {
				results[i].Target = state.ForStorage(result.Target)
			}
			if result.ResolvedSource != nil {
				results[i].ResolvedSource = state.ForStorage(*result.ResolvedSource)
			}
		}
	}
	var job json.Marshaler
	if includeJob {
		job = state.ForStorage(r.Job)
	}
	return json.Marshal(struct {
		fields
		Job     json.Marshaler `json:"job,omitempty"`
		Results []storedResult `json:"results"`
	}{fields(r), job, results})
}
