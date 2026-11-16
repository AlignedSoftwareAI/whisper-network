package testscale

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLoad(t *testing.T) {
	assert.InDelta(t, RaceFactor, load(""), 1e-9)
	assert.InDelta(t, 2.5*RaceFactor, load("2.5"), 1e-9)
	for _, bad := range []string{"0", "-1", "fast", "Inf", "NaN"} {
		assert.Panics(t, func() { load(bad) }, bad)
	}
}

func TestScaling(t *testing.T) {
	old := factor
	t.Cleanup(func() { factor = old })
	factor = 1.5
	assert.Equal(t, 3*time.Second, D(2*time.Second))
	assert.Equal(t, 2*time.Second, Seconds(time.Second), "1.5 s rounds to 2 s")
	factor = 0.1
	assert.Equal(t, time.Second, Seconds(time.Second), "never under 1 s")
}
