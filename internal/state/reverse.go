package state

import (
	"encoding/json"
	"errors"
)

// ScanReverse visits the newest keys first. before is exclusive; an empty
// before starts at the last key. Callers retain only copied values.
func (t *Tx) ScanReverse(bucket, before string, limit int, fn func(string, json.RawMessage) error) error {
	if limit <= 0 {
		return errors.New("scan limit must be positive")
	}
	b := t.tx.Bucket([]byte(bucket))
	if b == nil {
		return nil
	}
	c := b.Cursor()
	k, v := c.Last()
	if before != "" {
		k, v = c.Seek([]byte(before))
		if k == nil {
			k, v = c.Last()
		} else {
			k, v = c.Prev()
		}
	}
	for count := 0; k != nil && count < limit; count++ {
		if v != nil {
			if err := fn(string(k), append(json.RawMessage(nil), v...)); err != nil {
				return err
			}
		}
		k, v = c.Prev()
	}
	return nil
}
