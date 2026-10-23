package discovery

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
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
	return json.Marshal(struct {
		Object      indexObject `json:"o"`
		SeenAttempt string      `json:"a"`
	}{toIndexObject(o.Object), o.SeenAttempt})
}

// The index key already contains the exact source key. The recovery index
// retains every other Object field; full source objects remain in the batches.
// Short field names avoid repeating JSON labels millions of times in rows too
// small to benefit from per-value compression.
type indexObject struct {
	Size               int64             `json:"s,omitempty"`
	ModifiedAt         time.Time         `json:"m,omitzero"`
	ETag               string            `json:"e,omitempty"`
	VersionID          string            `json:"v,omitempty"`
	ContentType        string            `json:"t,omitempty"`
	ContentEncoding    string            `json:"n,omitempty"`
	ContentDisposition string            `json:"d,omitempty"`
	CacheControl       string            `json:"c,omitempty"`
	StorageClass       string            `json:"sc,omitempty"`
	ExpiresAt          time.Time         `json:"x,omitzero"`
	LifecycleExpiresAt time.Time         `json:"lx,omitzero"`
	Metadata           map[string]string `json:"u,omitzero"`
}

func toIndexObject(o transferstore.Object) indexObject {
	return indexObject{Size: o.Size, ModifiedAt: o.ModifiedAt, ETag: o.ETag, VersionID: o.VersionID,
		ContentType: o.ContentType, ContentEncoding: o.ContentEncoding, ContentDisposition: o.ContentDisposition,
		CacheControl: o.CacheControl, StorageClass: o.StorageClass, ExpiresAt: o.ExpiresAt,
		LifecycleExpiresAt: o.LifecycleExpiresAt, Metadata: o.Metadata}
}

func (o indexObject) object() transferstore.Object {
	return transferstore.Object{Size: o.Size, ModifiedAt: o.ModifiedAt, ETag: o.ETag, VersionID: o.VersionID,
		ContentType: o.ContentType, ContentEncoding: o.ContentEncoding, ContentDisposition: o.ContentDisposition,
		CacheControl: o.CacheControl, StorageClass: o.StorageClass, ExpiresAt: o.ExpiresAt,
		LifecycleExpiresAt: o.LifecycleExpiresAt, Metadata: o.Metadata}
}

func (o *discoveredObject) UnmarshalJSON(raw []byte) error {
	var compact struct {
		Object      *indexObject `json:"o"`
		SeenAttempt string       `json:"a"`
	}
	if err := json.Unmarshal(raw, &compact); err != nil {
		return err
	}
	if compact.Object != nil {
		*o = discoveredObject{Object: compact.Object.object(), SeenAttempt: compact.SeenAttempt, keyOmitted: true}
		return nil
	}
	type original discoveredObject
	*o = discoveredObject{}
	return json.Unmarshal(raw, (*original)(o))
}

func getDiscoveredObject(tx *state.Tx, parentID int64, key string) (prior discoveredObject, found bool, err error) {
	return getDiscoveredObjectAtKey(tx, objectKey(parentID, key), key)
}

func getDiscoveredObjectAtKey(tx *state.Tx, index, key string) (prior discoveredObject, found bool, err error) {
	found, err = tx.Get(objects, index, &prior)
	if err == nil && found {
		if prior.Object.Key == "" && !prior.keyOmitted || prior.Object.Key != "" && prior.Object.Key != key {
			return discoveredObject{}, false, ErrCorrupt
		}
		prior.Object.Key = key
	}
	return
}

// CompactObjectIndexValue lets offline compaction rewrite legacy index rows
// without rescanning the source. It accepts either encoding and verifies the
// duplicated legacy key before removing it from the value. Unknown fields are
// retained byte-for-byte rather than silently discarded by a typed decoder.
func CompactObjectIndexValue(index string, raw json.RawMessage) (json.RawMessage, error) {
	prefix, suffix, ok := strings.Cut(index, "/")
	if !ok {
		return nil, ErrCorrupt
	}
	parentID, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil || parentID < 1 || prefix != parentKey(parentID) {
		return nil, ErrCorrupt
	}
	key, err := base64.RawURLEncoding.DecodeString(suffix)
	if err != nil || len(key) == 0 || base64.RawURLEncoding.EncodeToString(key) != suffix {
		return nil, ErrCorrupt
	}
	known, err := knownIndexFields(raw)
	if err != nil {
		return nil, err
	}
	if !known {
		return raw, nil
	}
	var row discoveredObject
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	if row.Object.Key == "" && !row.keyOmitted || row.Object.Key != "" && row.Object.Key != string(key) {
		return nil, ErrCorrupt
	}
	return row.MarshalStateJSON()
}

func knownIndexFields(raw json.RawMessage) (bool, error) {
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil {
		return false, err
	}
	if row == nil {
		return false, ErrCorrupt
	}
	objectField, attemptField := "object", "seenAttempt"
	allowed := legacyObjectFields
	if _, compact := row["o"]; compact {
		objectField, attemptField, allowed = "o", "a", compactObjectFields
	}
	for field := range row {
		if field != objectField && field != attemptField {
			return false, nil
		}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(row[objectField], &object); err != nil {
		return false, ErrCorrupt
	}
	if object == nil {
		return false, ErrCorrupt
	}
	for field := range object {
		if !allowed[field] {
			return false, nil
		}
	}
	return true, nil
}

var legacyObjectFields = map[string]bool{
	"key": true, "size": true, "modifiedAt": true, "etag": true, "versionId": true,
	"contentType": true, "contentEncoding": true, "contentDisposition": true,
	"cacheControl": true, "storageClass": true, "expiresAt": true, "lifecycleExpiresAt": true, "metadata": true,
}

var compactObjectFields = map[string]bool{
	"s": true, "m": true, "e": true, "v": true, "t": true, "n": true, "d": true,
	"c": true, "sc": true, "x": true, "lx": true, "u": true,
}
