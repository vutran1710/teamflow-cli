// Package ids makes Teamflow ids: a prefix and a ULID, as Python's tf_models.ids.new_id.
package ids

import (
	"crypto/rand"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Signal is the prefix of signal ids.
const Signal = "sg_"

// New is a fresh id with the prefix: a ULID (48 bits of milliseconds, 80 random bits).
func New(prefix string) string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		panic(err) // crypto/rand never fails on the supported platforms
	}
	var out [26]byte
	// 128 bits as 26 base-32 digits, the first holding the top 3 bits
	hi := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 | uint64(b[4])<<24 |
		uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	lo := uint64(b[8])<<56 | uint64(b[9])<<48 | uint64(b[10])<<40 | uint64(b[11])<<32 | uint64(b[12])<<24 |
		uint64(b[13])<<16 | uint64(b[14])<<8 | uint64(b[15])
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return prefix + string(out[:])
}
