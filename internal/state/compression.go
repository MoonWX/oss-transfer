package state

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"sync"
)

var ErrCorruptValue = errors.New("invalid compressed state record")

var compressedMagic = []byte{0xff, 'O', 'S', 'T', 'D', 'B', 1, 0}

const compressedHeaderSize = 20
const minimumCompressionSize = 512

// Larger legacy values remain plain JSON. Never allocate from an unchecked
// on-disk length, including when a corrupt file claims an enormous record.
const maximumDecodedSize = 256 << 20

var compressors = sync.Pool{New: func() any {
	writer, err := flate.NewWriter(io.Discard, flate.BestSpeed)
	if err != nil {
		panic(err)
	}
	return writer
}}

type reusableInflater interface {
	io.ReadCloser
	flate.Resetter
}

// Each concurrent read owns its inflater until decoding and validation finish.
// Reuse the decoder's history window, never the returned record bytes.
var inflaters = sync.Pool{New: func() any {
	return flate.NewReader(bytes.NewReader(nil)).(reusableInflater)
}}

func (t *Tx) compressionEnabled() bool {
	bucket := t.tx.Bucket([]byte("metadata"))
	if bucket == nil {
		return false
	}
	var format databaseFormat
	return json.Unmarshal(bucket.Get([]byte("format")), &format) == nil &&
		(format.Role == "master" || format.Role == "agent") &&
		format.SchemaVersion >= SchemaCompressedValues && supportedSchema(format.SchemaVersion)
}

func encodeValue(raw []byte) ([]byte, error) {
	if len(raw) < minimumCompressionSize || len(raw) > maximumDecodedSize {
		return raw, nil
	}
	var buffer bytes.Buffer
	buffer.Write(compressedMagic)
	var header [12]byte
	binary.BigEndian.PutUint64(header[:8], uint64(len(raw)))
	binary.BigEndian.PutUint32(header[8:], crc32.ChecksumIEEE(raw))
	buffer.Write(header[:])
	writer := compressors.Get().(*flate.Writer)
	writer.Reset(&buffer)
	_, err := writer.Write(raw)
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	writer.Reset(io.Discard)
	compressors.Put(writer)
	if err != nil {
		return nil, err
	}
	if buffer.Len()+64 > len(raw) || buffer.Len() > len(raw)-len(raw)/8 {
		return raw, nil
	}
	return buffer.Bytes(), nil
}

func decodeValue(stored []byte) ([]byte, error) {
	if len(stored) == 0 || stored[0] != compressedMagic[0] {
		return stored, nil
	}
	if len(stored) < compressedHeaderSize || !bytes.Equal(stored[:len(compressedMagic)], compressedMagic) {
		return nil, ErrCorruptValue
	}
	size := binary.BigEndian.Uint64(stored[8:16])
	if size < minimumCompressionSize || size > maximumDecodedSize {
		return nil, ErrCorruptValue
	}
	// bytes.Reader implements io.ByteReader, so flate cannot consume trailing
	// bytes while reading ahead. Extra data or concatenated streams are rejected.
	input := bytes.NewReader(stored[compressedHeaderSize:])
	reader := inflaters.Get().(reusableInflater)
	if err := reader.Reset(input, nil); err != nil {
		_ = reader.Close()
		return nil, ErrCorruptValue
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, int64(size)+1))
	closeErr := reader.Close()
	// Do not retain a reference to a transaction's mmap or a large input value.
	if resetErr := reader.Reset(bytes.NewReader(nil), nil); resetErr == nil {
		inflaters.Put(reader)
	}
	if err != nil || closeErr != nil || uint64(len(decoded)) != size || input.Len() != 0 || crc32.ChecksumIEEE(decoded) != binary.BigEndian.Uint32(stored[16:20]) {
		return nil, ErrCorruptValue
	}
	return decoded, nil
}

// Compressed values already decode into owned memory. Only plain values need a
// copy before a scanner hands them to code that can outlive the transaction.
func copyDecodedValue(stored []byte) ([]byte, error) {
	if len(stored) == 0 || stored[0] != compressedMagic[0] {
		return append([]byte(nil), stored...), nil
	}
	return decodeValue(stored)
}
