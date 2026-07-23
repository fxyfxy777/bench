package forensics

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// sha256Pool reuses hash.Hash instances to avoid 2 allocations (~400B) per digest.
var sha256Pool = sync.Pool{
	New: func() any { return sha256.New() },
}

// BodyDigest returns the lowercase hex-encoded SHA256 digest of body.
// Returns "" for nil or empty body.
//
// Uses a pooled hasher to avoid per-call allocation.
// Typical cost: ~3μs for 1KB body on modern CPUs.
func BodyDigest(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	h := sha256Pool.Get().(interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
		Reset()
	})
	defer sha256Pool.Put(h)

	h.Reset()
	_, _ = h.Write(body) // sha256.Write never returns an error
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)
}
