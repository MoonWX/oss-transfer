package state

import (
	"encoding/json"
	"reflect"
)

// Marshaler provides a compact database representation that decodes into the
// original type. It does not change API JSON or task identity fingerprints.
type Marshaler interface {
	MarshalStateJSON() ([]byte, error)
}

type storedValue struct{ value any }

func (v storedValue) MarshalJSON() ([]byte, error) {
	if value := reflect.ValueOf(v.value); value.IsValid() && value.Kind() == reflect.Pointer && value.IsNil() {
		return []byte("null"), nil
	}
	if value, ok := v.value.(Marshaler); ok {
		return value.MarshalStateJSON()
	}
	return json.Marshal(v.value)
}

// ForStorage applies the database encoding to an explicitly selected value,
// including a nested field. Ordinary json.Marshal keeps its existing format.
func ForStorage(value any) json.Marshaler { return storedValue{value} }
