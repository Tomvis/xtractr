package xtractr

// SecondsForTest exposes cueTimestamp.toSeconds for tests.
func SecondsForTest(min, sec, frames int) float64 {
	return cueTimestamp{minutes: min, seconds: sec, frames: frames}.toSeconds()
}

// TrackDurationsForTest exposes trackDurations for tests.
func TrackDurationsForTest(starts []float64) []float64 {
	return trackDurations(starts)
}

// ResolveCueAudioPathForTest exposes resolveCueAudioPath for tests.
func ResolveCueAudioPathForTest(cueDir, cueFile, cueFilePath string) (string, error) {
	return resolveCueAudioPath(cueDir, cueFile, cueFilePath)
}

// ProbeDurationForTest exposes probeAudio's duration for tests.
func ProbeDurationForTest(t interface{ Fatalf(string, ...any) }, path string) float64 {
	p, err := probeAudio(path)
	if err != nil {
		t.Fatalf("probeAudio: %v", err)
	}

	return p.durationSec
}

// ProbeTagsForTest exposes probeAudio's collected tags as a map for tests.
func ProbeTagsForTest(t interface{ Fatalf(string, ...any) }, path string) map[string]string {
	p, err := probeAudio(path)
	if err != nil {
		t.Fatalf("probeAudio: %v", err)
	}

	m := map[string]string{}
	for _, kv := range p.tags {
		m[kv[0]] = kv[1]
	}

	return m
}
