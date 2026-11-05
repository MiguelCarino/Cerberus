package users

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
)

// Hashing parameters (docs/SCOPE.md "Password hashing").
const (
	DefaultIterations = 600_000
	saltLen           = 16
	keyLen            = 32
	hashScheme        = "pbkdf2-sha256"
	maxHashing        = 2 // hashes running at once
)

// iterations is the count new hashes get. Tests in other packages lower it
// with SetCostForTest so a sign-in does not take a second under -race.
var iterations atomic.Int64

func init() { iterations.Store(DefaultIterations) }

// SetCostForTest sets the iteration count for new hashes and returns a
// function that restores it. Never call it outside tests.
func SetCostForTest(n int) (restore func()) {
	old := iterations.Swap(int64(n))
	return func() { iterations.Store(old) }
}

// hashSem bounds concurrent hashing: a login flood must not eat the CPU the
// models need.
var hashSem = make(chan struct{}, maxHashing)

func acquire(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case hashSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func release() { <-hashSem }

// HashPassword returns the stored form of pw.
func HashPassword(ctx context.Context, pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	iter := int(iterations.Load())
	if err := acquire(ctx); err != nil {
		return "", err
	}
	defer release()
	key, err := pbkdf2.Key(sha256.New, pw, salt, iter, keyLen)
	if err != nil {
		return "", err
	}
	return hashScheme + "$" + strconv.Itoa(iter) + "$" + base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(key), nil
}

var errHashFormat = errors.New("unknown password hash format")

// parseHash splits a stored hash.
func parseHash(h string) (iter int, salt, key []byte, err error) {
	parts := strings.Split(h, "$")
	if len(parts) != 4 || parts[0] != hashScheme {
		return 0, nil, nil, errHashFormat
	}
	iter, err = strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 100_000_000 {
		return 0, nil, nil, errHashFormat
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[2]); err != nil || len(salt) == 0 {
		return 0, nil, nil, errHashFormat
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[3]); err != nil || len(key) == 0 {
		return 0, nil, nil, errHashFormat
	}
	return iter, salt, key, nil
}

// CheckPasswordHash compares pw with a stored hash in constant time.
// rehash is true when the hash uses an older iteration count.
func CheckPasswordHash(ctx context.Context, stored, pw string) (ok, rehash bool, err error) {
	iter, salt, key, err := parseHash(stored)
	if err != nil {
		return false, false, err
	}
	if err := acquire(ctx); err != nil {
		return false, false, err
	}
	defer release()
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(key))
	if err != nil {
		return false, false, err
	}
	ok = subtle.ConstantTimeCompare(got, key) == 1
	return ok, ok && iter != int(iterations.Load()), nil
}

// dummyHash is checked for unknown names, so the time a sign-in takes does
// not tell which names exist. It is made once per iteration count.
var dummy struct {
	hash atomic.Pointer[string]
}

func dummyHash() string {
	if p := dummy.hash.Load(); p != nil && strings.HasPrefix(*p, hashScheme+"$"+strconv.FormatInt(iterations.Load(), 10)+"$") {
		return *p
	}
	// A fixed salt and key: the comparison always fails, the work is the same.
	h := hashScheme + "$" + strconv.FormatInt(iterations.Load(), 10) + "$" +
		base64.RawStdEncoding.EncodeToString(make([]byte, saltLen)) + "$" +
		base64.RawStdEncoding.EncodeToString(make([]byte, keyLen))
	dummy.hash.Store(&h)
	return h
}

// dummyChecks counts dummy comparisons (tests check the unknown-name path).
var dummyChecks atomic.Int64

func checkDummy(ctx context.Context, pw string) {
	dummyChecks.Add(1)
	_, _, _ = CheckPasswordHash(ctx, dummyHash(), pw)
}
