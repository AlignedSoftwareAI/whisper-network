package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fast keeps the unit tests quick; DefaultPasswordParams is exercised once.
var fast = PasswordParams{MemoryKiB: 8, Time: 1, Threads: 1}

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse", fast)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(h, "$argon2id$v=19$m=8,t=1,p=1$"), h)

	ok, err := VerifyPassword("correct horse", h)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = VerifyPassword("wrong", h)
	require.NoError(t, err)
	assert.False(t, ok)

	ok, err = VerifyPassword("", h)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestPasswordSaltIsRandom(t *testing.T) {
	a, _ := HashPassword("x", fast)
	b, _ := HashPassword("x", fast)
	assert.NotEqual(t, a, b)
}

func TestPasswordDefaultParams(t *testing.T) {
	p := DefaultPasswordParams()
	start := time.Now()
	h, err := HashPassword("pw", p)
	require.NoError(t, err)
	t.Logf("default argon2id hash took %v", time.Since(start))
	assert.Contains(t, h, "m=65536,t=3,p=4")
	ok, err := VerifyPassword("pw", h)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.False(t, NeedsRehash(h, p))
}

func TestPasswordTooLong(t *testing.T) {
	long := strings.Repeat("a", MaxPasswordLen+1)
	_, err := HashPassword(long, fast)
	assert.ErrorIs(t, err, ErrPasswordTooLong)

	h, _ := HashPassword("a", fast)
	ok, err := VerifyPassword(long, h)
	assert.NoError(t, err)
	assert.False(t, ok)

	_, err = HashPassword(strings.Repeat("a", MaxPasswordLen), fast)
	assert.NoError(t, err)
}

func TestHashPasswordRejectsZeroParams(t *testing.T) {
	_, err := HashPassword("x", PasswordParams{})
	assert.Error(t, err)
}

func TestNeedsRehash(t *testing.T) {
	h, _ := HashPassword("x", fast)
	assert.False(t, NeedsRehash(h, fast))
	assert.True(t, NeedsRehash(h, PasswordParams{MemoryKiB: 16, Time: 1, Threads: 1}))
	assert.True(t, NeedsRehash(h, PasswordParams{MemoryKiB: 8, Time: 2, Threads: 1}))
	assert.True(t, NeedsRehash(h, PasswordParams{MemoryKiB: 8, Time: 1, Threads: 2}))
	assert.True(t, NeedsRehash("garbage", fast))
}

func TestVerifyMalformedHashes(t *testing.T) {
	good, _ := HashPassword("x", fast)
	parts := strings.Split(good, "$")
	with := func(i int, v string) string {
		c := append([]string(nil), parts...)
		c[i] = v
		return strings.Join(c, "$")
	}
	bad := []string{
		"", "garbage", "$argon2i$v=19$m=8,t=1,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",
		with(2, "v=18"), with(2, "x"), with(3, "m=8,t=1"), with(3, "m=0,t=1,p=1"),
		with(3, "m=99999999,t=1,p=1"), with(3, "m=8,t=999,p=1"), with(3, "m=8,t=1,p=99"),
		with(4, "!!!"), with(4, ""), with(5, "!!!"), with(5, "AAAA"),
		good + "$extra", strings.TrimPrefix(good, "$"),
	}
	for _, b := range bad {
		ok, err := VerifyPassword("x", b)
		assert.False(t, ok, b)
		assert.ErrorIs(t, err, ErrBadHash, b)
	}
}

func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		p, err := GeneratePassword()
		require.NoError(t, err)
		assert.Len(t, p, 24)
		assert.True(t, isBase62(p, 24), p)
		assert.False(t, seen[p])
		seen[p] = true
	}
}
