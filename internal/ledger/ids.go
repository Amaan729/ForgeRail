package ledger

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// NewID returns a prefixed, roughly time-ordered id like "tr_0192f3...".
// 48 bits of unix millis followed by 64 random bits, hex encoded.
func NewID(prefix string) string {
	var b [14]byte
	ms := uint64(time.Now().UnixMilli())
	binary.BigEndian.PutUint64(b[:8], ms<<16)
	if _, err := rand.Read(b[6:]); err != nil {
		panic("ledger: crypto/rand failed: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
