package xtractr

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// symlinkOrSkip creates a probe symlink and skips the test where links are
// unavailable (Windows without developer mode).
func symlinkOrSkip(t *testing.T, dir string) {
	t.Helper()

	err := os.Symlink("target", filepath.Join(dir, "symlink-probe"))
	if err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
}

// TestPrepareFFmpegOutputRefusesEscape ensures a destination that leaves the
// output folder is never handed to ffmpeg, whether it escapes lexically or only
// after a pre-existing symlinked parent is resolved.
func TestPrepareFFmpegOutputRefusesEscape(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	symlinkOrSkip(t, tmp)

	outside := filepath.Join(tmp, "outside")
	output := filepath.Join(tmp, "out")
	require.NoError(t, os.MkdirAll(outside, 0o750))
	require.NoError(t, os.MkdirAll(output, 0o750))
	// A planted folder link inside the output folder: lexically contained,
	// resolves outside.
	require.NoError(t, os.Symlink(outside, filepath.Join(output, "linked")))

	xFile := &XFile{FilePath: filepath.Join(tmp, "album.cue"), OutputDir: output, FileMode: 0o644, DirMode: 0o750}

	for _, dest := range []string{
		filepath.Join(outside, "escaped.flac"),
		filepath.Join(output, "..", "escaped.flac"),
		filepath.Join(output, "linked", "escaped.flac"),
	} {
		target, err := prepareFFmpegOutput(xFile, dest)
		require.Error(t, err, dest)
		assert.ErrorIs(t, err, ErrInvalidPath, dest)
		assert.Nil(t, target, dest)
		assert.NoFileExists(t, dest, dest)
	}
}

// TestPrepareFFmpegOutputReplacesPlantedSymlink ensures a symlink planted at a
// track's destination is unlinked and replaced with a regular file we own, so
// the file ffmpeg is later pointed at cannot lead outside the output folder.
func TestPrepareFFmpegOutputReplacesPlantedSymlink(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	symlinkOrSkip(t, tmp)

	victim := filepath.Join(tmp, "victim.txt")
	require.NoError(t, os.WriteFile(victim, []byte("original"), 0o600))

	output := filepath.Join(tmp, "out")
	require.NoError(t, os.MkdirAll(output, 0o750))

	dest := filepath.Join(output, "01 - One.flac")
	require.NoError(t, os.Symlink(victim, dest))

	xFile := &XFile{OutputDir: output, FileMode: 0o644, DirMode: 0o750}

	target, err := prepareFFmpegOutput(xFile, dest)
	require.NoError(t, err)
	require.Equal(t, dest, target.path)

	info, err := os.Lstat(dest)
	require.NoError(t, err)
	assert.Zero(t, info.Mode()&os.ModeSymlink, "planted symlink must be replaced by a regular file")
	assert.Zero(t, info.Size())

	data, err := os.ReadFile(victim)
	require.NoError(t, err)
	assert.Equal(t, "original", string(data), "the file outside the output folder must be untouched")
}

// TestFFmpegTargetVerifyRejectsSwap ensures the post-write recheck fails when
// the destination is no longer the file we created. This is the window an
// external writer leaves open: ffmpeg opens the path itself, so a swap between
// our create and its open can only be caught afterwards.
func TestFFmpegTargetVerifyRejectsSwap(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	symlinkOrSkip(t, tmp)

	victim := filepath.Join(tmp, "victim.txt")
	require.NoError(t, os.WriteFile(victim, []byte("original"), 0o600))

	output := filepath.Join(tmp, "out")
	require.NoError(t, os.MkdirAll(output, 0o750))

	xFile := &XFile{OutputDir: output, FileMode: 0o644, DirMode: 0o750}
	dest := filepath.Join(output, "01 - One.flac")

	target, err := prepareFFmpegOutput(xFile, dest)
	require.NoError(t, err)

	// Hold the original open: an unlinked inode stays allocated while open, so the
	// files planted below cannot reuse its number. SameFile compares dev+inode, and
	// ext4/overlayfs hand a freed inode straight back (APFS never does).
	held, err := os.Open(dest)
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Close() })

	_, err = target.verify()
	require.NoError(t, err, "the file we just created must verify")

	// Swap the destination for a symlink, the way a racing attacker would.
	require.NoError(t, os.Remove(dest))
	require.NoError(t, os.Symlink(victim, dest))

	_, err = target.verify()
	require.Error(t, err)
	assert.ErrorIs(t, err, errExtractSymlink)

	// A different regular file at the same path is refused too.
	require.NoError(t, os.Remove(dest))
	require.NoError(t, os.WriteFile(dest, []byte("planted"), 0o600))

	_, err = target.verify()
	require.Error(t, err)
	assert.ErrorIs(t, err, errExtractConflict)
}

// TestCountOutputEnforcesMaxBytes ensures bytes an external writer produced are
// charged against MaxBytes, and that the overage is reported as a limit error.
func TestCountOutputEnforcesMaxBytes(t *testing.T) {
	t.Parallel()

	output := t.TempDir()
	xFile := &XFile{OutputDir: output, FileMode: 0o644, DirMode: 0o750, MaxBytes: 100}
	xFile.newProgress(0, 0, 1)

	target, err := prepareFFmpegOutput(xFile, filepath.Join(output, "track.flac"))
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(target.path, make([]byte, 60), 0o600))

	charged, err := target.countOutput(xFile, 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(60), charged)
	assert.Equal(t, uint64(60), xFile.prog.wrote())

	// Only the growth is charged the second time around.
	require.NoError(t, os.WriteFile(target.path, make([]byte, 90), 0o600))

	charged, err = target.countOutput(xFile, charged)
	require.NoError(t, err)
	assert.Equal(t, uint64(90), charged)
	assert.Equal(t, uint64(90), xFile.prog.wrote())

	// And crossing MaxBytes fails as a limit error without charging the bytes.
	require.NoError(t, os.WriteFile(target.path, make([]byte, 101), 0o600))

	_, err = target.countOutput(xFile, charged)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMaxBytes)
	assert.True(t, IsLimitError(err), "MaxBytes must be reported as a limit error")
	assert.Equal(t, uint64(90), xFile.prog.wrote(), "a refused write must not be charged")
}

// TestFFmpegSizeLimit checks the -fs brake: nothing when the budget is
// unlimited, and one byte above the budget otherwise so output that exactly
// fills the budget is still written whole.
func TestFFmpegSizeLimit(t *testing.T) {
	t.Parallel()

	assert.Nil(t, ffmpegSizeLimit(unlimitedBytes))
	assert.Nil(t, ffmpegSizeLimit(math.MaxInt64))
	assert.Equal(t, []string{"-fs", "1"}, ffmpegSizeLimit(0))
	assert.Equal(t, []string{"-fs", "4097"}, ffmpegSizeLimit(4096))
}

// TestExtractBytesRemaining checks the budget handed to the -fs brake.
func TestExtractBytesRemaining(t *testing.T) {
	t.Parallel()

	var unset *XFile

	assert.Equal(t, unlimitedBytes, unset.extractBytesRemaining())

	xFile := &XFile{OutputDir: t.TempDir()}
	assert.Equal(t, unlimitedBytes, xFile.extractBytesRemaining(), "no tracker means no budget")

	xFile.newProgress(0, 0, 1)
	assert.Equal(t, unlimitedBytes, xFile.extractBytesRemaining(), "no caps means unlimited")

	xFile.MaxBytes = 500
	assert.Equal(t, uint64(500), xFile.extractBytesRemaining())

	require.NoError(t, xFile.countExternalWrite(200))
	assert.Equal(t, uint64(300), xFile.extractBytesRemaining())

	// MaxRatio tightens the same budget: 100 compressed bytes at ratio 2.
	xFile.MaxRatio = 2
	xFile.prog.Compressed = 100
	assert.Equal(t, uint64(0), xFile.extractBytesRemaining())
}

// TestExtractCoverCountsAgainstMaxFiles ensures cover art is charged like any
// other output, and that the cap hit surfaces as a limit error (which the split
// must abort on) rather than being swallowed as "no art". No ffmpeg needed: the
// budget is refused before the process would be started.
func TestExtractCoverCountsAgainstMaxFiles(t *testing.T) {
	t.Parallel()

	output := t.TempDir()
	xFile := &XFile{OutputDir: output, FileMode: 0o644, DirMode: 0o750, MaxFiles: 1}
	xFile.newProgress(0, 0, 1)

	require.NoError(t, xFile.countExtracted(), "spend the budget's only slot")

	path, size, err := extractCover(xFile, filepath.Join(output, "src.flac"), filepath.Join(output, "cover"), "png")
	require.ErrorIs(t, err, ErrMaxFiles)
	assert.True(t, IsLimitError(err), "MaxFiles must be reported as a limit error")
	assert.Empty(t, path)
	assert.Zero(t, size)
	assert.NoFileExists(t, filepath.Join(output, "cover.png"))
}

// TestFFmpegCutRollsBackReservedSlot ensures a failed encode hands the reserved
// MaxFiles slot back and removes the placeholder, so a later track is not
// refused for a file that was never produced.
func TestFFmpegCutRollsBackReservedSlot(t *testing.T) {
	t.Parallel()

	if !ffmpegAvailable() {
		t.Skip("ffmpeg not found in PATH; skipping ffmpeg-backed test")
	}

	output := t.TempDir()
	xFile := &XFile{OutputDir: output, FileMode: 0o644, DirMode: 0o750, MaxFiles: 2}
	xFile.newProgress(0, 0, 1)

	dest := filepath.Join(output, "01 - One.flac")
	cut := &ffmpegCut{xFile: xFile, src: filepath.Join(output, "no-such-source.wav")}

	_, _, err := cut.run(dest)
	require.Error(t, err, "ffmpeg must fail on a missing source")
	assert.Zero(t, xFile.prog.Files, "the reserved MaxFiles slot must be handed back")
	assert.NoFileExists(t, dest, "the placeholder must not be left behind")
}
