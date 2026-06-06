package xtractr

// SecondsForTest exposes cueTimestamp.toSeconds for tests.
func SecondsForTest(min, sec, frames int) float64 {
	return cueTimestamp{minutes: min, seconds: sec, frames: frames}.toSeconds()
}

// TrackDurationsForTest exposes trackDurations for tests.
func TrackDurationsForTest(starts []float64) []float64 {
	return trackDurations(starts)
}
