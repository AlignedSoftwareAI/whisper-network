//go:build race

package testscale

// RaceFactor slows the scale down under the race detector, which makes
// everything several times slower.
const RaceFactor = 2.0
