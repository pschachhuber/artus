package secrets

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TOTP returns the current one-time code for a secret holding either a base32
// seed, an otpauth:// URI, or a Proton Pass reference to a TOTP field (which
// Proton Pass turns into the code itself).
func (s *Store) TOTP(ctx context.Context, name, reason string) (string, error) {
	value, err := s.Use(ctx, name, reason)
	if err != nil {
		return "", err
	}
	if isCode(value) {
		return value, nil
	}
	return Code(value, time.Now())
}

func isCode(v string) bool {
	if len(v) < 6 || len(v) > 8 {
		return false
	}
	_, err := strconv.Atoi(v)
	return err == nil
}

// Code computes an RFC 6238 code at time t from a base32 seed or otpauth URI.
func Code(seed string, t time.Time) (string, error) {
	digits, period, newHash := 6, int64(30), sha1.New
	if strings.HasPrefix(seed, "otpauth://") {
		u, err := url.Parse(seed)
		if err != nil {
			return "", err
		}
		q := u.Query()
		seed = q.Get("secret")
		if d, err := strconv.Atoi(q.Get("digits")); err == nil && d >= 6 && d <= 8 {
			digits = d
		}
		if p, err := strconv.ParseInt(q.Get("period"), 10, 64); err == nil && p > 0 {
			period = p
		}
		switch strings.ToUpper(q.Get("algorithm")) {
		case "SHA256":
			newHash = sha256.New
		case "SHA512":
			newHash = sha512.New
		}
	}
	seed = strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "=", "").Replace(seed))
	if seed == "" {
		return "", errors.New("empty TOTP seed")
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(seed)
	if err != nil {
		return "", fmt.Errorf("TOTP seed is not valid base32")
	}
	return hotp(key, uint64(t.Unix()/period), digits, newHash), nil
}

func hotp(key []byte, counter uint64, digits int, newHash func() hash.Hash) string {
	mac := hmac.New(newHash, key)
	binary.Write(mac, binary.BigEndian, counter)
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, code%mod)
}
