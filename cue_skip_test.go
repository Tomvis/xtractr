package xtractr_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/encoding/charmap"
	"golift.io/xtractr"
)

// TestCueWindows1251 covers Russian rips whose CUE is cp1251 while the audio
// file name on disk is UTF-8. Undecoded, the FILE line matched nothing and the
// titles became empty track names ("01 - .flac").
func TestCueWindows1251(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	outputDir := filepath.Join(tmpDir, "output")

	flacName := "Кино - 1984 - Начальник Камчатки (Демо).flac"
	generateTestFLAC(t, filepath.Join(tmpDir, flacName), uint64(30*testSampleRate))

	cueContent := strings.Join([]string{
		`PERFORMER "Кино"`,
		`TITLE "Начальник Камчатки"`,
		`FILE "` + flacName + `" WAVE`,
		`  TRACK 01 AUDIO`,
		`    TITLE "Последний герой"`,
		`    INDEX 01 00:00:00`,
		`  TRACK 02 AUDIO`,
		`    TITLE "Каждую ночь"`,
		`    INDEX 01 00:15:00`,
	}, "\r\n") + "\r\n"

	encoded, err := charmap.Windows1251.NewEncoder().String(cueContent)
	require.NoError(t, err)

	cuePath := filepath.Join(tmpDir, "Кино - 1984 - Начальник Камчатки (Демо).cue")
	require.NoError(t, os.WriteFile(cuePath, []byte(encoded), 0o600))

	_, files, _, err := xtractr.ExtractCUE(&xtractr.XFile{
		FilePath:  cuePath,
		OutputDir: outputDir,
		FileMode:  0o600,
		DirMode:   0o755,
	})
	require.NoError(t, err)
	assert.Contains(t, files, filepath.Join(outputDir, "01 - Последний герой.flac"))
	assert.Contains(t, files, filepath.Join(outputDir, "02 - Каждую ночь.flac"))
}

// TestFindSkipsPerTrackCue: a CUE with one FILE per track describes a rip that
// is already split. It is not an archive; listing it made every extract fail.
func TestFindSkipsPerTrackCue(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	for _, name := range []string{"01 - Like A Virgin.flac", "02 - Stay.flac"} {
		generateTestFLAC(t, filepath.Join(tmpDir, name), uint64(testSampleRate))
	}

	cueContent := strings.Join([]string{
		`FILE "Madonna - Like A Virgin\01 - Like A Virgin.wav" WAVE`,
		`  TRACK 01 AUDIO`,
		`    INDEX 01 00:00:00`,
		`FILE "Madonna - Like A Virgin\02 - Stay.wav" WAVE`,
		`  TRACK 02 AUDIO`,
		`    INDEX 01 00:00:00`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "Like A Virgin.cue"), []byte(cueContent), 0o600))

	found := xtractr.FindCompressedFiles(xtractr.Filter{Path: tmpDir})
	assert.Empty(t, found.List())
}

// TestFindSkipsCueWithoutAudio: a CUE whose image is not in the folder has
// nothing to split (typically a per-track rip that kept the image's CUE).
func TestFindSkipsCueWithoutAudio(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	generateTestFLAC(t, filepath.Join(tmpDir, "01 - Ameno.flac"), uint64(testSampleRate))

	cueContent := strings.Join([]string{
		`FILE "Era - Era (Special Edition).wav" WAVE`,
		`  TRACK 01 AUDIO`,
		`    INDEX 01 00:00:00`,
		`  TRACK 02 AUDIO`,
		`    INDEX 01 04:00:00`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "Era - Era (Special Edition).cue"), []byte(cueContent), 0o600))

	found := xtractr.FindCompressedFiles(xtractr.Filter{Path: tmpDir})
	assert.Empty(t, found.List())
}
