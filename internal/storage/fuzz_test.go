package storage

import "testing"

func FuzzDecodeRecordHeader(f *testing.F) {
	f.Add(make([]byte, recordHeaderSize))
	f.Fuzz(func(t *testing.T, header []byte) {
		_, _, _, _ = decodeRecordHeader(header)
	})
}
