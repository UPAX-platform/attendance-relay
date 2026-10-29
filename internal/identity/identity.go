// Package identity is the relay's key pair: an Ed25519 key generated on the
// relay, whose public half UPAX stores when the relay is paired. The private
// key never leaves this machine.
//
// v1 keeps the key in a file readable by the service account only
// (relay.key: the 32-byte seed, base64, one line). OS key stores are a later
// migration that doesn't change the wire contract.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/atomicfile"
)

// Alphabet is the base32 alphabet of fingerprints and pairing codes: digits
// and upper-case letters without I, L, O and U, so they read unambiguously.
const Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// encoding is base32 over Alphabet, unpadded, most significant bits first
// (as RFC 4648 base32).
var encoding = base32.NewEncoding(Alphabet).WithPadding(base32.NoPadding)

// Key is the relay's private key.
type Key struct {
	priv ed25519.PrivateKey
	// Created is when the key was generated (the file's modification time
	// for a loaded key); rotation is due a year after it.
	Created time.Time
}

// Generate makes a new key.
func Generate() (*Key, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Key{priv: priv, Created: time.Now()}, nil
}

// FromSeed builds a key from a 32-byte seed (tests and test vectors).
func FromSeed(seed []byte) (*Key, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("identity: seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	return &Key{priv: ed25519.NewKeyFromSeed(seed), Created: time.Now()}, nil
}

// Public is the public key UPAX stores.
func (k *Key) Public() ed25519.PublicKey { return k.priv.Public().(ed25519.PublicKey) }

// PublicBase64 is the public key as sent when pairing.
func (k *Key) PublicBase64() string { return base64.StdEncoding.EncodeToString(k.Public()) }

// Sign signs msg.
func (k *Key) Sign(msg []byte) []byte { return ed25519.Sign(k.priv, msg) }

// Fingerprint identifies the key: also the signature keyid.
func (k *Key) Fingerprint() string { return Fingerprint(k.Public()) }

// Fingerprint is the first 16 bytes of SHA-256(public key) in base32 over
// Alphabet: 26 characters.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return encoding.EncodeToString(sum[:16])
}

// Group formats a fingerprint for people to compare: XXXX-XXXX-…-XX.
func Group(fp string) string {
	var b strings.Builder
	for i, r := range fp {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Load reads a key file.
func Load(path string) (*Key, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("%s: not a relay key: %w", path, err)
	}
	k, err := FromSeed(seed)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if fi, err := os.Stat(path); err == nil {
		k.Created = fi.ModTime()
	}
	return k, nil
}

// Save writes the key to path atomically, readable by the service account
// and administrators only.
func (k *Key) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := atomicfile.SecureDir(dir); err != nil {
		return err
	}
	data := base64.StdEncoding.EncodeToString(k.priv.Seed()) + "\n"
	return atomicfile.WritePrivate(path, []byte(data))
}

// Source hands out the current key, reloading the file when it changes
// (after `upax-relay rotate-key` or `pair` ran while the service was up).
type Source struct {
	Path string

	mu  sync.Mutex
	key *Key
	mod time.Time
	sz  int64
}

// Key returns the current key.
func (s *Source) Key() (*Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fi, err := os.Stat(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoKey
		}
		return nil, err
	}
	if s.key != nil && fi.ModTime().Equal(s.mod) && fi.Size() == s.sz {
		return s.key, nil
	}
	k, err := Load(s.Path)
	if err != nil {
		if s.key != nil {
			// A file caught mid-replace or damaged: keep signing with the key
			// that works rather than stop.
			return s.key, nil
		}
		return nil, err
	}
	s.key, s.mod, s.sz = k, fi.ModTime(), fi.Size()
	return k, nil
}

// ErrNoKey means the relay has no key yet: it hasn't been paired.
var ErrNoKey = errors.New("this relay has no key; pair it with UPAX first (upax-relay setup)")
