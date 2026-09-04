package prompton

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"time"
)

// NewLogID returns a fresh UUIDv7, the idempotency key a monitoring-log
// record is written with. PromptOn stores log ids in a UUIDv7 column: a
// v4 id passes request validation and then fails on write, so the SDK always
// issues v7.
//
// Layout (RFC 9562): 48-bit unix milliseconds | version 7 | 12 random bits |
// variant 0b10 | 62 random bits. Lowercase hex with dashes.
func NewLogID() string {
	return newUUIDv7At(time.Now())
}

func newUUIDv7At(t time.Time) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	if int64(ms) < 0 {
		ms = 0
	}
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)

	var r [10]byte
	if _, err := rand.Read(r[:]); err != nil {
		// crypto/rand never fails on the platforms this SDK supports; fall back
		// to a time-derived filler rather than panicking inside a log call.
		binary.BigEndian.PutUint64(r[:8], ms*2654435761)
	}
	copy(b[6:], r[:10])

	b[6] = (b[6] & 0x0F) | 0x70 // version 7
	b[8] = (b[8] & 0x3F) | 0x80 // variant 10

	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

// LogIDTime reads the timestamp a UUIDv7 log id encodes. It is
// what makes ids sort by time, and it is how a record can be placed on a
// timeline without a separate field. The second return value is false when the
// id is not a UUIDv7.
func LogIDTime(id string) (time.Time, bool) {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[14] != '7' {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(id[0:8]+id[9:13], 16, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(ms).UTC(), true
}
