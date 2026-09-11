package xtractr

/* CUE sheet parse and ExtractCUE dispatch. */

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// supportedCueAudioExts lists the audio file extensions (lowercase, with dot)
// that ExtractCUE can split. FLAC uses the pure-Go path; the rest use ffmpeg.
var supportedCueAudioExts = []string{".flac", ".ape", ".wv", ".m4a", ".wav"}

// cueUseFFmpegForAPE reports whether an .ape CUE source should go through the ffmpeg
// path (per-track FLAC, fully tagged, with cover art) instead of the pure-Go APE
// splitter (per-track .ape carrying no APEv2 footer at all). Tagged output is the
// point of this fork, so ffmpeg wins whenever it is installed. It is a variable so
// tests can pin either branch without depending on the host's PATH.
var cueUseFFmpegForAPE = ffmpegAvailable

// isSupportedCueAudioExt reports whether ext (lowercase, with leading dot) is splittable.
func isSupportedCueAudioExt(ext string) bool {
	for _, e := range supportedCueAudioExts {
		if e == ext {
			return true
		}
	}

	return false
}

// ErrUTF16LengthInvalid is returned when the length of a UTF-16 encoded byte slice is not even.
var ErrUTF16LengthInvalid = errors.New("invalid UTF-16 length")

// CueSheet represents a parsed CUE sheet.
type CueSheet struct {
	// Performer is the album-level performer.
	Performer string
	// Title is the album title.
	Title string
	// File is the referenced audio file name (e.g. "album.flac").
	File string
	// FileType is the file type from the CUE sheet (e.g. "WAVE", "BINARY").
	FileType string
	// Tracks contains the list of tracks in order.
	Tracks []CueTrack
}

// CueTrack represents a single track in a CUE sheet.
type CueTrack struct {
	// Number is the track number (1-based).
	Number int
	// Title is the track title.
	Title string
	// Performer is the track-level performer (falls back to album performer).
	Performer string
	// StartSample is the starting sample position for this track.
	StartSample uint64
}

// cueTimestamp holds the raw parsed CUE time (MM:SS:FF).
type cueTimestamp struct {
	minutes int
	seconds int
	frames  int // CD frames, 75 per second
}

// cdFramesPerSecond is the number of frames per second in CD audio (75 fps).
const cdFramesPerSecond = 75

// toSamples converts a CUE timestamp to a sample position at the given sample rate.
func (t cueTimestamp) toSamples(sampleRate uint32) uint64 {
	const secondsPerMinute = 60

	totalSeconds := uint64(t.minutes)*secondsPerMinute + uint64(t.seconds)
	samples := totalSeconds * uint64(sampleRate)
	// Add fractional second from CD frames.
	samples += uint64(t.frames) * uint64(sampleRate) / cdFramesPerSecond

	return samples
}

// toSeconds converts a CUE timestamp (MM:SS:FF) to seconds.
func (t cueTimestamp) toSeconds() float64 {
	const secondsPerMinute = 60

	return float64(t.minutes*secondsPerMinute+t.seconds) + float64(t.frames)/cdFramesPerSecond
}

// trackDurations returns the duration (seconds) of each track given the ordered
// start times. The final track's duration is 0, signalling "encode to EOF" (no -t
// flag) so it captures any trailing samples.
func trackDurations(starts []float64) []float64 {
	durs := make([]float64, len(starts))
	for i := range starts {
		if i < len(starts)-1 {
			durs[i] = starts[i+1] - starts[i]
		} else {
			durs[i] = 0 // last track -> to EOF
		}
	}

	return durs
}

// ExtractCUE extracts individual tracks from a FLAC file referenced by a CUE sheet.
// The xFile.FilePath should point to the .cue file (or .cue.txt).
func ExtractCUE(xFile *XFile) (size uint64, files, archives []string, err error) {
	cue, timestamps, err := parseCueSheetFile(xFile.FilePath)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("parsing cue sheet: %w", err)
	}

	// Resolve the audio file path relative to the CUE file.
	// Some CUE sheets say FILE "album.wav" WAVE but the file on disk is album.flac; try .flac when .wav is missing.
	// If the FILE line still does not match (e.g. O vs Ö), try the FLAC with the same base name as the CUE file.
	cueDir := filepath.Dir(xFile.FilePath)

	audioPath, err := resolveCueAudioPath(cueDir, cue.File, xFile.FilePath)
	if err != nil {
		return 0, nil, nil, err
	}

	defer xFile.newProgress(0, archiveFileSize(audioPath), len(cue.Tracks)).done()

	ext := strings.ToLower(filepath.Ext(audioPath))

	switch {
	case ext == ".flac":
		size, files, err = splitFLAC(xFile, audioPath, cue, timestamps)
	case ext == ".ape" && !cueUseFFmpegForAPE():
		// Native, pure-Go Monkey's Audio decoder (upstream). Only reached when ffmpeg is
		// missing: it emits untagged .ape tracks, where the ffmpeg path below emits tagged
		// FLAC with cover art. Degraded output beats no output.
		size, files, err = splitAPE(xFile, audioPath, cue, timestamps)
	case isSupportedCueAudioExt(ext): // .ape/.wv/.m4a/.wav -> ffmpeg path
		size, files, err = splitViaFFmpeg(xFile, audioPath, cue, timestamps)
	default:
		return 0, nil, nil, fmt.Errorf("%w: %s", ErrUnsupportedAudio, ext)
	}

	if err != nil {
		return 0, nil, nil, err
	}

	// Write the CUE sheet into the output directory so the folder is self-contained
	// (tracks, art, and the exact split definition for archival and re-rip verification).
	// filepath.Base strips any directory components, and the join is verified to
	// stay inside the output folder.
	cueBase := filepath.Base(xFile.FilePath)
	cueDest := filepath.Join(xFile.OutputDir, cueBase)

	if !xFile.pathWithinOutput(cueDest) {
		return 0, nil, nil, fmt.Errorf("%s: %w: %s", xFile.FilePath, ErrInvalidPath, cueDest)
	}

	files, err = copyCueSheetToOutput(xFile, cueDest, files)
	if err != nil {
		return size, files, nil, err
	}

	// The archive list includes both the CUE file and the FLAC file.
	archives = []string{xFile.FilePath, audioPath}

	return size, files, archives, nil
}

// copyCueSheetToOutput writes the source CUE into the extract folder. Limit
// errors abort the extract; other copy failures are logged and ignored.
func copyCueSheetToOutput(xFile *XFile, cueDest string, files []string) ([]string, error) {
	writeErr := copyCueToOutput(xFile, xFile.FilePath, cueDest, xFile.FileMode)
	if IsLimitError(writeErr) {
		return files, writeErr
	}

	if writeErr != nil {
		xFile.Debugf("Copying CUE sheet to output: %s", writeErr)

		return files, nil
	}

	files = append(files, cueDest)
	// Mark so recursion does not try to extract this copied CUE again.
	xFile.SkipOnRecursion = append(xFile.SkipOnRecursion, cueDest)

	return files, nil
}

// parseCueSheetFile parses a CUE sheet from a file path and returns the sheet plus raw timestamps.
// It supports UTF-8, UTF-8 with BOM, and UTF-16 (LE/BE with BOM) encoded CUE files.
// TL;dr Some CUE sheets really suck.
//
//nolint:cyclop // tell me about it.
func parseCueSheetFile(path string) (*CueSheet, []cueTimestamp, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening cue sheet: %w", err)
	}

	// Detect BOM and decode to UTF-8 so the scanner sees valid text.
	// UTF-16 LE file has BOM bytes FF FE -> LittleEndian 0xFEFF.
	// UTF-16 BE file has BOM bytes FE FF -> LittleEndian 0xFFFE.
	const (
		utf16LEBOM = 0xFEFF
		utf16BEBOM = 0xFFFE
	)

	var reader io.Reader

	if len(data) > 1 {
		switch bom := binary.LittleEndian.Uint16(data[:2]); bom {
		case utf16LEBOM:
			// UTF-16 little-endian; decode data[2:] as LE.
			decoded, errDec := decodeUTF16(data[2:], binary.LittleEndian)
			if errDec != nil {
				return nil, nil, fmt.Errorf("decoding UTF-16 LE cue sheet: %w", errDec)
			}

			reader = bytes.NewReader(decoded)
		case utf16BEBOM:
			// UTF-16 big-endian; decode data[2:] as BE.
			decoded, errDec := decodeUTF16(data[2:], binary.BigEndian)
			if errDec != nil {
				return nil, nil, fmt.Errorf("decoding UTF-16 BE cue sheet: %w", errDec)
			}

			reader = bytes.NewReader(decoded)
		}
	}

	if reader == nil {
		// No UTF-16 BOM; strip UTF-8 BOM if present.
		if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
			data = data[3:]
		}

		reader = bytes.NewReader(data)
	}

	return parseCueSheet(reader)
}

// decodeUTF16 decodes a UTF-16 encoded byte slice to UTF-8.
//
//nolint:mnd
func decodeUTF16(data []byte, order binary.ByteOrder) ([]byte, error) {
	if len(data)%2 != 0 {
		return nil, fmt.Errorf("%w: %d", ErrUTF16LengthInvalid, len(data))
	}

	u16 := make([]uint16, len(data)/2)
	for i := range u16 {
		u16[i] = order.Uint16(data[2*i:])
	}

	runes := utf16.Decode(u16)
	// Encode runes to UTF-8.
	buf := make([]byte, 0, len(runes)*utf8.UTFMax)
	for _, r := range runes {
		buf = utf8.AppendRune(buf, r)
	}

	return buf, nil
}

// parseCueSheet parses a CUE sheet from an io.Reader.
func parseCueSheet(reader io.Reader) (*CueSheet, []cueTimestamp, error) { //nolint:gocognit,cyclop,funlen
	cue := &CueSheet{}
	scanner := bufio.NewScanner(reader)
	timestamps := []cueTimestamp{}

	var (
		currentTrack     *CueTrack
		currentTimestamp cueTimestamp
		hasTimestamp     bool
	)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "REM ") {
			continue
		}

		cmd, args := splitCueLine(line)

		switch cmd {
		case "PERFORMER":
			performer := unquoteCue(args)
			if currentTrack != nil {
				currentTrack.Performer = performer
			} else {
				cue.Performer = performer
			}
		case "TITLE":
			title := unquoteCue(args)
			if currentTrack != nil {
				currentTrack.Title = title
			} else {
				cue.Title = title
			}
		case "FILE":
			fileName, fileType := parseCueFileCmd(args)
			cue.File = fileName
			cue.FileType = fileType
		case "TRACK":
			if currentTrack != nil {
				cue.Tracks = append(cue.Tracks, *currentTrack)

				if hasTimestamp {
					timestamps = append(timestamps, currentTimestamp)
				} else {
					timestamps = append(timestamps, cueTimestamp{})
				}
			}

			trackNum := parseCueTrackNum(args)
			currentTrack = &CueTrack{Number: trackNum}
			hasTimestamp = false
			currentTimestamp = cueTimestamp{}
		case "INDEX":
			if currentTrack != nil {
				indexNum, timestamp := parseCueIndex(args)
				if indexNum == 1 {
					currentTimestamp = timestamp
					hasTimestamp = true
				}
			}
		}
	}

	// Save the last track.
	if currentTrack != nil {
		cue.Tracks = append(cue.Tracks, *currentTrack)

		if hasTimestamp {
			timestamps = append(timestamps, currentTimestamp)
		} else {
			timestamps = append(timestamps, cueTimestamp{})
		}
	}

	err := scanner.Err()
	if err != nil {
		return nil, nil, fmt.Errorf("reading cue sheet: %w", err)
	}

	if cue.File == "" {
		return nil, nil, ErrNoCueFile
	}

	if len(cue.Tracks) == 0 {
		return nil, nil, ErrNoTracks
	}

	// Fill in album-level performer for tracks that don't specify one.
	for idx := range cue.Tracks {
		if cue.Tracks[idx].Performer == "" {
			cue.Tracks[idx].Performer = cue.Performer
		}
	}

	return cue, timestamps, nil
}

// resolveCueAudioPath returns the path to the audio file referenced by the CUE.
// It first tries the exact FILE reference, then the same basename with each
// supported audio extension, then the CUE's own basename with each supported
// extension (handles encoding mismatches like O vs Ö and wrong-container CUEs).
func resolveCueAudioPath(cueDir, cueFile, cueFilePath string) (string, error) {
	// 1) Exact path from the FILE line.
	exact := filepath.Join(cueDir, cueFile)

	// A CUE sheet must not reference audio outside its own folder; a crafted
	// FILE entry like "../../secret.flac" would read and copy that file.
	if !pathWithin(cueDir, exact) {
		return "", fmt.Errorf("%w: %s", ErrInvalidPath, cueFile)
	}

	if _, err := os.Stat(exact); err == nil {
		return exact, nil
	}

	// 2) FILE basename + each supported extension.
	fileBase := strings.TrimSuffix(cueFile, filepath.Ext(cueFile))
	// 3) CUE basename + each supported extension (fallback for name mismatches).
	cueBase := cueBaseName(cueFilePath)

	for _, base := range []string{fileBase, cueBase} {
		for _, ext := range supportedCueAudioExts {
			candidate := filepath.Join(cueDir, base+ext)
			// fileBase is derived from the FILE line, so the containment check
			// above does not cover every path this loop can build. Skip rather
			// than fail: a stray candidate is not the sheet's real reference.
			if !pathWithin(cueDir, candidate) {
				continue
			}

			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}

	return "", fmt.Errorf("%w: %s", ErrAudioNotFound, exact)
}

// cueBaseName returns the CUE sheet filename without its sheet suffix.
// Release groups sometimes ship the sheet as .cue.txt; filepath.Ext would only
// strip .txt and leave a ".cue" stem that does not match album.flac.
func cueBaseName(path string) string {
	base := filepath.Base(path)
	lower := strings.ToLower(base)

	const cueTxtSuffix = ".cue.txt"
	if strings.HasSuffix(lower, cueTxtSuffix) {
		return base[:len(base)-len(cueTxtSuffix)]
	}

	return strings.TrimSuffix(base, filepath.Ext(base))
}

// splitCueLine splits a CUE line into its command and arguments.
func splitCueLine(line string) (string, string) {
	parts := strings.SplitN(line, " ", 2) //nolint:mnd
	if len(parts) < 2 {                   //nolint:mnd
		return strings.ToUpper(parts[0]), ""
	}

	return strings.ToUpper(parts[0]), parts[1]
}

// unquoteCue removes surrounding double quotes from a CUE sheet value.
func unquoteCue(val string) string {
	val = strings.TrimSpace(val)
	if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
		return val[1 : len(val)-1]
	}

	return val
}

// parseCueFileCmd parses the FILE command arguments: "filename.flac" WAVE.
func parseCueFileCmd(args string) (string, string) {
	var fileName, fileType string

	if args != "" && args[0] == '"' {
		// Find closing quote.
		end := strings.Index(args[1:], "\"")
		if end >= 0 {
			fileName = args[1 : end+1]
			fileType = strings.TrimSpace(args[end+2:])
		} else {
			fileName = unquoteCue(args)
		}
	} else {
		parts := strings.SplitN(args, " ", 2) //nolint:mnd
		fileName = parts[0]

		if len(parts) > 1 {
			fileType = strings.TrimSpace(parts[1])
		}
	}

	return fileName, fileType
}

// parseCueTrackNum parses the track number from TRACK args like "01 AUDIO".
func parseCueTrackNum(args string) int {
	parts := strings.Fields(args)
	if len(parts) == 0 {
		return 0
	}

	num, _ := strconv.Atoi(parts[0])

	return num
}

// parseCueIndex parses the INDEX command args like "01 03:45:12".
func parseCueIndex(args string) (int, cueTimestamp) {
	parts := strings.Fields(args)
	if len(parts) < 2 { //nolint:mnd
		return 0, cueTimestamp{}
	}

	indexNum, _ := strconv.Atoi(parts[0])
	timestamp := parseCueTime(parts[1])

	return indexNum, timestamp
}

// cueTimeRegex matches the MM:SS:FF timestamp format.
var cueTimeRegex = regexp.MustCompile(`^(\d+):(\d+):(\d+)$`)

// parseCueTime parses a CUE timestamp string in MM:SS:FF format.
func parseCueTime(s string) cueTimestamp {
	matches := cueTimeRegex.FindStringSubmatch(s)
	if matches == nil {
		return cueTimestamp{}
	}

	minutes, _ := strconv.Atoi(matches[1])
	seconds, _ := strconv.Atoi(matches[2])
	frames, _ := strconv.Atoi(matches[3])

	return cueTimestamp{
		minutes: minutes,
		seconds: seconds,
		frames:  frames,
	}
}

// copyCueToOutput copies the CUE sheet file into the output directory so the
// extracted folder contains tracks, album art, and the CUE for verification/archival.
func copyCueToOutput(xFile *XFile, srcPath, destPath string, fileMode os.FileMode) error {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("reading cue sheet: %w", err)
	}

	err = xFile.writeExtractFile(destPath, data, fileMode)
	if err != nil {
		return fmt.Errorf("writing cue sheet: %w", err)
	}

	return nil
}

// formatTrackFilename generates a filename for an extracted track.
// The ext parameter should include the dot (e.g. ".flac", ".ape").
func formatTrackFilename(track *CueTrack, ext string) string {
	title := track.Title
	if title == "" {
		title = fmt.Sprintf("Track %d", track.Number)
	}

	title = sanitizeFilename(title)

	return fmt.Sprintf("%02d - %s%s", track.Number, title, ext)
}

// sanitizeFilename removes or replaces characters that are problematic in filenames.
// It normalizes smart/curly quotes to ASCII so tools like Lidarr can find files reliably.
func sanitizeFilename(name string) string {
	// Normalize smart quotes and curly quotes to ASCII (fixes Lidarr "could not find file" when CUE has U+2019 etc).
	name = strings.ReplaceAll(name, "\u2018", "'")  // LEFT SINGLE QUOTATION MARK
	name = strings.ReplaceAll(name, "\u2019", "'")  // RIGHT SINGLE QUOTATION MARK
	name = strings.ReplaceAll(name, "\u201C", "\"") // LEFT DOUBLE QUOTATION MARK
	name = strings.ReplaceAll(name, "\u201D", "\"") // RIGHT DOUBLE QUOTATION MARK
	// Remove other characters that are problematic in filenames or for downstream tools.
	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		":", "-",
		"*", "",
		"?", "",
		"\"", "",
		"<", "",
		">", "",
		"|", "",
	)
	name = replacer.Replace(name)

	// Strip control characters and Unicode replacement character.
	var data strings.Builder
	data.Grow(len(name))

	for _, r := range name {
		if r == '\uFFFD' || r < 32 || r == 127 {
			continue
		}

		data.WriteRune(r)
	}

	return data.String()
}
