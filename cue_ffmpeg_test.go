package xtractr_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"golift.io/xtractr"
)

// ffmpegOrSkip skips the test when ffmpeg/ffprobe are not installed.
func ffmpegOrSkip(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found in PATH; skipping ffmpeg-backed test")
	}

	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not found in PATH; skipping ffmpeg-backed test")
	}
}

// makeSineSource writes a `seconds`-long stereo source at outPath using ffmpeg's
// sine generator, encoded by file extension (.wav/.flac/.ape/.wv/.m4a).
func makeSineSource(t *testing.T, outPath string, seconds int) {
	t.Helper()

	cmd := exec.Command("ffmpeg", "-y", "-v", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration="+strconv.Itoa(seconds),
		"-ac", "2", outPath)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "ffmpeg gen: %s", string(out))
}

func TestProbeAudio_StreamLevelTags(t *testing.T) {
	t.Parallel()
	ffmpegOrSkip(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src.wv") // WavPack: ffmpeg can encode it; tags go via APEv2
	cmd := exec.Command("ffmpeg", "-y", "-v", "error", "-f", "lavfi",
		"-i", "sine=frequency=440:sample_rate=44100:duration=2", "-ac", "2",
		"-metadata", "GENRE=TestGenre", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("no wavpack encoder in this ffmpeg build: %s", out)
	}

	tags := xtractr.ProbeTagsForTest(t, src)
	require.Equal(t, "TestGenre", tags["GENRE"], "source tag must be read (from stream or format level)")
}

func TestProbeAudio_Duration(t *testing.T) {
	t.Parallel()
	ffmpegOrSkip(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src.flac")
	makeSineSource(t, src, 5)

	got := xtractr.ProbeDurationForTest(t, src)
	require.InDelta(t, 5.0, got, 0.2)
}

func TestCutTrackFLAC_Duration(t *testing.T) {
	t.Parallel()
	ffmpegOrSkip(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src.flac")
	makeSineSource(t, src, 10)

	out := filepath.Join(dir, "track.flac")
	require.NoError(t, xtractr.CutTrackFLACForTest(src, out, 2.0, 3.0)) // 2s..5s -> 3s

	got := xtractr.ProbeDurationForTest(t, out)
	require.InDelta(t, 3.0, got, 0.05) // sample-accurate within ~50ms
}

func TestRetagFLAC_WritesTags(t *testing.T) {
	t.Parallel()
	ffmpegOrSkip(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src.flac")
	makeSineSource(t, src, 2)

	pairs := [][2]string{{"TITLE", "Hello"}, {"TRACKNUMBER", "1"}, {"ALBUM", "Demo"}}
	require.NoError(t, xtractr.RetagFLACForTest(src, pairs, "", 0o644))

	got := xtractr.ReadVorbisTagForTest(t, src, "TITLE")
	require.Equal(t, "Hello", got)
}

func TestSplitViaFFmpeg_EndToEnd(t *testing.T) {
	t.Parallel()
	ffmpegOrSkip(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "album.wav")
	makeSineSource(t, src, 9) // 9-second source

	cue := `PERFORMER "The Band"
TITLE "The Album"
FILE "album.wav" WAVE
  TRACK 01 AUDIO
    TITLE "One"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "Two"
    INDEX 01 00:03:00
  TRACK 03 AUDIO
    TITLE "Three"
    INDEX 01 00:06:00`
	cuePath := filepath.Join(dir, "album.cue")
	require.NoError(t, os.WriteFile(cuePath, []byte(cue), 0o644))

	out := t.TempDir()
	size, files := xtractr.SplitViaFFmpegForTest(t, dir, "album.wav", cuePath, out)

	require.Equal(t, 3, len(files), "expected 3 track files (no cover)")
	require.Greater(t, size, uint64(0))

	require.Equal(t, "Two", xtractr.ReadVorbisTagForTest(t, filepath.Join(out, "02 - Two.flac"), "TITLE"))
	require.Equal(t, "The Album", xtractr.ReadVorbisTagForTest(t, filepath.Join(out, "02 - Two.flac"), "ALBUM"))
}
