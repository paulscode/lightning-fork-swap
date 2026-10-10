package channels

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math/bits"
	"sync"
	"time"
)

// PoW is the small proof of work a new order costs: the browser finds a
// nonce whose hash with a fresh challenge starts with Bits zero bits (about
// a second's work), so creating orders in bulk costs more than it is worth.
// Challenges are signed with a key made at start, last ten minutes and
// serve once.
type PoW struct {
	Bits int
	key  []byte
	mu   sync.Mutex
	used map[string]time.Time
}

// ChallengeLife is how long a challenge may be answered.
const ChallengeLife = 10 * time.Minute

// NewPoW makes the key.
func NewPoW(bits int) *PoW {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return &PoW{Bits: bits, key: key, used: map[string]time.Time{}}
}

// Challenge is a new challenge.
func (p *PoW) Challenge(now time.Time) string {
	b := make([]byte, 16, 32)
	binary.BigEndian.PutUint64(b, uint64(now.Unix()))
	if _, err := rand.Read(b[8:]); err != nil {
		panic(err)
	}
	mac := hmac.New(sha256.New, p.key)
	mac.Write(b)
	return base64.RawURLEncoding.EncodeToString(append(b, mac.Sum(nil)[:16]...))
}

// Errors of Verify
var (
	ErrChallenge = errors.New("challenge expired or not ours")
	ErrWork      = errors.New("not enough work")
	ErrUsed      = errors.New("challenge already used")
)

// Verify checks a challenge and its nonce, and spends the challenge.
func (p *PoW) Verify(challenge, nonce string, now time.Time) error {
	b, err := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil || len(b) != 32 || len(nonce) == 0 || len(nonce) > 32 {
		return ErrChallenge
	}
	mac := hmac.New(sha256.New, p.key)
	mac.Write(b[:16])
	if !hmac.Equal(mac.Sum(nil)[:16], b[16:]) {
		return ErrChallenge
	}
	issued := time.Unix(int64(binary.BigEndian.Uint64(b[:8])), 0)
	if now.Sub(issued) > ChallengeLife || issued.After(now.Add(time.Minute)) {
		return ErrChallenge
	}
	if LeadingZeros(Work(challenge, nonce)) < p.Bits {
		return ErrWork
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for c, at := range p.used {
		if now.Sub(at) > ChallengeLife+time.Minute {
			delete(p.used, c)
		}
	}
	if _, ok := p.used[challenge]; ok {
		return ErrUsed
	}
	p.used[challenge] = issued
	return nil
}

// Work is the hash the browser computes: SHA-256 of "challenge:nonce".
func Work(challenge, nonce string) [32]byte {
	return sha256.Sum256([]byte(challenge + ":" + nonce))
}

// LeadingZeros counts the hash's leading zero bits.
func LeadingZeros(h [32]byte) int {
	n := 0
	for _, b := range h {
		if b == 0 {
			n += 8
			continue
		}
		return n + bits.LeadingZeros8(b)
	}
	return n
}
