// Package testscale is the one time-scale knob for tests that run on real
// time: lease TTLs, heartbeat intervals and the deadlines of their waits.
//
// The factor is WN_TEST_TIME_SCALE (default 1) times RaceFactor, which is
// larger in a -race build. Raise WN_TEST_TIME_SCALE on a slow or heavily
// shared machine; nothing else needs to change.
package testscale

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"time"
)

// EnvVar names the environment variable that multiplies every scaled time.
const EnvVar = "WN_TEST_TIME_SCALE"

var factor = load(os.Getenv(EnvVar))

func load(v string) float64 {
	f := 1.0
	if v != "" {
		p, err := strconv.ParseFloat(v, 64)
		if err != nil || p <= 0 || math.IsInf(p, 0) || math.IsNaN(p) {
			panic(fmt.Sprintf("%s=%q: want a positive number", EnvVar, v))
		}
		f = p
	}
	return f * RaceFactor
}

// Factor is the effective scale: WN_TEST_TIME_SCALE × RaceFactor.
func Factor() float64 { return factor }

// D scales d.
func D(d time.Duration) time.Duration { return time.Duration(float64(d) * factor) }

// Seconds scales d and rounds it to the nearest whole second (at least one), for
// intervals that travel as integer seconds, such as the heartbeat interval.
func Seconds(d time.Duration) time.Duration {
	return max(time.Second, D(d).Round(time.Second))
}
