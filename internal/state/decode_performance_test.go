package state

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
	"testing"
)

func decodeFixture(t testing.TB) ([]byte, []byte) {
	t.Helper()
	var objects []map[string]any
	for i := 0; i < 128; i++ {
		key := fmt.Sprintf("dataset/scene-%03d/frame-%06d.bin", i/16, i)
		objects = append(objects, map[string]any{"key": key, "size": i * 1024, "etag": fmt.Sprintf("%x", sha256.Sum256([]byte(key))), "modifiedAt": "2026-10-23T10:20:30Z"})
	}
	raw, err := json.Marshal(objects)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeValue(raw)
	if err != nil || !bytes.HasPrefix(encoded, compressedMagic) {
		t.Fatal("fixture must compress", err)
	}
	return raw, encoded
}

func TestConcurrentDecoderReusePreservesOwnedResultsAfterCorruption(t *testing.T) {
	raw, encoded := decodeFixture(t)
	badCRC := bytes.Clone(encoded)
	badCRC[16] ^= 1
	badBlock := bytes.Clone(encoded)
	badBlock[compressedHeaderSize] = 0x07 // Reserved DEFLATE block type.
	corrupted := [][]byte{badCRC, encoded[:len(encoded)-1], badBlock}
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Go(func() {
			var retained [][]byte
			for i := 0; i < 30; i++ {
				if _, err := decodeValue(corrupted[i%len(corrupted)]); !errors.Is(err, ErrCorruptValue) {
					t.Error("corrupted frame accepted", err)
					return
				}
				got, err := decodeValue(encoded)
				if err != nil || !bytes.Equal(got, raw) {
					t.Error("decoder reuse changed data", err)
					return
				}
				retained = append(retained, got)
			}
			for _, got := range retained {
				if !bytes.Equal(got, raw) {
					t.Error("returned bytes were reused by another read")
				}
			}
		})
	}
	workers.Wait()
}

// Exact previous decode path for an explicitly run before/after benchmark.
func decodeWithoutReuse(stored []byte) ([]byte, error) {
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
	input := bytes.NewReader(stored[compressedHeaderSize:])
	reader := flate.NewReader(input)
	decoded, err := io.ReadAll(io.LimitReader(reader, int64(size)+1))
	closeErr := reader.Close()
	if err != nil || closeErr != nil || uint64(len(decoded)) != size || input.Len() != 0 || crc32.ChecksumIEEE(decoded) != binary.BigEndian.Uint32(stored[16:20]) {
		return nil, ErrCorruptValue
	}
	return decoded, nil
}

func BenchmarkStateDecode(b *testing.B) {
	raw, frame := decodeFixture(b)
	for _, variant := range []struct {
		name   string
		decode func([]byte) ([]byte, error)
	}{{"baseline_d171f4b", decodeWithoutReuse}, {"reused", decodeValue}} {
		b.Run(variant.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					got, err := variant.decode(frame)
					if err != nil || !bytes.Equal(got, raw) {
						b.Fatal("decoded bytes differ", err)
					}
				}
			})
		})
	}
}
