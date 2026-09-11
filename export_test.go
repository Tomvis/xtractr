package xtractr

import (
	"os"
	"path/filepath"

	flacvorbis "github.com/go-flac/flacvorbis/v2"
	goflac "github.com/go-flac/go-flac/v2"
)

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

// CutTrackFLACForTest exposes cutTrackFLAC for tests, uncapped.
func CutTrackFLACForTest(src, out string, startSec, durSec float64) error {
	return cutTrackFLAC(src, out, startSec, durSec, unlimitedBytes)
}

// RetagFLACForTest exposes retagFLAC for tests.
func RetagFLACForTest(path string, tagPairs [][2]string, coverPath string, mode os.FileMode) error {
	return retagFLAC(path, tagPairs, coverPath, mode)
}

// ReadVorbisTagForTest reads back a single Vorbis tag value (first match).
func ReadVorbisTagForTest(t interface{ Fatalf(string, ...any) }, path, key string) string {
	f, err := goflac.ParseFile(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	for _, b := range f.Meta {
		if b.Type != goflac.VorbisComment {
			continue
		}

		cmt, perr := flacvorbis.ParseFromMetaDataBlock(*b)
		if perr != nil {
			t.Fatalf("parse vorbis: %v", perr)
		}

		vals, _ := cmt.Get(key)
		if len(vals) > 0 {
			return vals[0]
		}
	}

	return ""
}

// SplitViaFFmpegForTest wires parse + resolve + split for end-to-end tests.
func SplitViaFFmpegForTest(t interface{ Fatalf(string, ...any) }, cueDir, cueFile, cuePath, outDir string) (uint64, []string) {
	cue, timestamps, err := parseCueSheetFile(cuePath)
	if err != nil {
		t.Fatalf("parse cue: %v", err)
	}

	audioPath, err := resolveCueAudioPath(cueDir, cueFile, cuePath)
	if err != nil {
		t.Fatalf("resolve audio: %v", err)
	}

	xFile := &XFile{OutputDir: outDir, FileMode: 0o644, DirMode: 0o755}

	size, files, err := splitViaFFmpeg(xFile, audioPath, cue, timestamps)
	if err != nil {
		t.Fatalf("split: %v", err)
	}

	return size, files
}

// ProbeCoverForTest exposes probeAudio's cover codec + presence for tests.
func ProbeCoverForTest(t interface{ Fatalf(string, ...any) }, path string) (string, bool) {
	p, err := probeAudio(path)
	if err != nil {
		t.Fatalf("probeAudio: %v", err)
	}

	return p.coverCodec, p.hasCover
}

// ExtractCoverForTest exposes extractCover for tests. The art's own folder acts
// as the output dir so the destination containment check has a base.
func ExtractCoverForTest(src, destNoExt, codec string) (string, error) {
	xFile := &XFile{OutputDir: filepath.Dir(destNoExt), FileMode: 0o644, DirMode: 0o755}

	path, _, err := extractCover(xFile, src, destNoExt, codec)

	return path, err
}

// ExtractCUEForTest runs the full ExtractCUE on a .cue path for integration tests.
func ExtractCUEForTest(t interface{ Fatalf(string, ...any) }, cuePath, outDir string) (uint64, []string, []string) {
	xFile := &XFile{FilePath: cuePath, OutputDir: outDir, FileMode: 0o644, DirMode: 0o755}

	size, files, archives, err := ExtractCUE(xFile)
	if err != nil {
		t.Fatalf("ExtractCUE: %v", err)
	}

	return size, files, archives
}
