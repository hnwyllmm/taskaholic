package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// New returns a time-sortable, process-independent identifier. The timestamp is
// useful while debugging SQLite directly; the random suffix provides uniqueness.
func New(prefix string) string {
	var random [10]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic(fmt.Sprintf("generate id: %v", err))
	}
	return fmt.Sprintf("%s_%013d_%s", prefix, time.Now().UTC().UnixMilli(), hex.EncodeToString(random[:]))
}
