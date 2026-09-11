package xtractr

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	flacpicture "github.com/go-flac/flacpicture/v2"
	flacvorbis "github.com/go-flac/flacvorbis/v2"
	goflac "github.com/go-flac/go-flac/v2"
)

// ffmpegAvailable reports whether both ffmpeg and ffprobe are on PATH.
func ffmpegAvailable() bool {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return false
	}

	_, err := exec.LookPath("ffprobe")

	return err == nil
}

// audioProbe holds the bits of ffprobe output we use.
type audioProbe struct {
	durationSec float64
	tags        [][2]string // upper-cased album-level source tags
	hasCover    bool        // an attached_pic video stream is present
	coverCodec  string      // codec_name of the attached_pic stream (e.g. "png", "mjpeg")
}

// ffprobeOutput mirrors the JSON we read from ffprobe.
type ffprobeOutput struct {
	Format struct {
		Duration string            `json:"duration"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		CodecType   string `json:"codec_type"`
		CodecName   string `json:"codec_name"`
		Disposition struct {
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
		Tags map[string]string `json:"tags"`
	} `json:"streams"`
}

// probeAudio runs ffprobe and returns duration, album-level source tags (upper-cased
// keys), and whether the file carries embedded cover art.
func probeAudio(path string) (*audioProbe, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json",
		"-show_format", "-show_streams", path)

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe %s: %w", path, err)
	}

	var parsed ffprobeOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parsing ffprobe json: %w", err)
	}

	probe := &audioProbe{}
	probe.durationSec, _ = strconv.ParseFloat(strings.TrimSpace(parsed.Format.Duration), 64)

	for key, val := range parsed.Format.Tags {
		probe.tags = append(probe.tags, [2]string{strings.ToUpper(key), val})
	}

	// APEv2 tags for Monkey's Audio (.ape) and WavPack (.wv) live on the audio
	// stream, not in format. Merge stream tags too; format-level tags win on conflict.
	have := map[string]bool{}
	for _, kv := range probe.tags {
		have[kv[0]] = true
	}

	for _, s := range parsed.Streams {
		if s.CodecType == "video" && s.Disposition.AttachedPic == 1 {
			probe.hasCover = true
			probe.coverCodec = s.CodecName
		}

		if s.CodecType != "audio" {
			continue
		}

		for key, val := range s.Tags {
			upper := strings.ToUpper(key)
			if have[upper] {
				continue
			}

			probe.tags = append(probe.tags, [2]string{upper, val})
			have[upper] = true
		}
	}

	return probe, nil
}

// cutTrackFLAC encodes one track to FLAC from src, starting at startSec.
// If durSec > 0 a -t duration is applied; durSec == 0 means "to EOF" (last track).
// No metadata is copied from the source (-map_metadata -1); tagging happens later.
// Input -ss + default accurate_seek yields sample-accurate cuts for lossless audio.
// maxBytes is the remaining MaxBytes/MaxRatio budget (unlimitedBytes for none);
// see ffmpegSizeLimit for what the cap does and does not guarantee.
func cutTrackFLAC(src, outPath string, startSec, durSec float64, maxBytes uint64) error {
	args := []string{"-nostdin", "-v", "error", "-ss", formatSeconds(startSec), "-i", src}
	if durSec > 0 {
		args = append(args, "-t", formatSeconds(durSec))
	}

	args = append(args, "-vn", "-c:a", "flac", "-compression_level", "8", "-map_metadata", "-1")
	args = append(args, ffmpegSizeLimit(maxBytes)...)
	args = append(args, "-y", outPath)

	cmd := exec.Command("ffmpeg", args...)

	stderr, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg cut %s: %w: %s", outPath, err, strings.TrimSpace(string(stderr)))
	}

	return nil
}

// formatSeconds renders seconds for ffmpeg with microsecond precision.
func formatSeconds(s float64) string {
	return strconv.FormatFloat(s, 'f', 6, 64)
}

// ffmpegSizeLimit renders ffmpeg's -fs output option for the remaining byte
// budget, or nothing when the budget is unlimited. ffmpeg stops writing once
// the output reaches the limit, which bounds a runaway encode to the budget
// (plus the packet in flight) instead of a whole track. The limit is one byte
// above the budget so output that exactly fills it is still written complete;
// -fs is a brake, not the check — countOutput is what returns ErrMaxBytes.
// The two agree by construction: a file ffmpeg cut short here is at least
// budget+1 bytes, which countOutput always rejects, so a truncated track can
// never be mistaken for a complete one.
func ffmpegSizeLimit(remaining uint64) []string {
	// ffmpeg parses -fs into an int64, so anything at or above MaxInt64 is
	// effectively unlimited and must not be passed.
	if remaining >= math.MaxInt64 {
		return nil
	}

	return []string{"-fs", strconv.FormatUint(remaining+1, 10)}
}

// ffmpegTarget is an output path that has been made safe to hand to ffmpeg,
// plus the identity of the file that was created there.
type ffmpegTarget struct {
	path string      // the path actually created (may be truncated for NAME_MAX)
	info os.FileInfo // identity of the file we created, for the post-run recheck
}

// ffmpegWriteMode forces owner write onto the created placeholder: ffmpeg has
// to be able to open it. retagFLAC applies the configured FileMode afterwards,
// exactly as it did when ffmpeg created the file itself.
const ffmpegWriteMode = 0o600

// prepareFFmpegOutput creates the destination file for an ffmpeg write with the
// same protections an in-process writer gets, and records its identity.
//
// ffmpeg opens the path itself, from another process, so openExtractFile's
// O_NOFOLLOW guarantee cannot be extended over the write the way it is for an
// io.Writer. What is achievable is this: refuse a destination that escapes
// OutputDir (including through a pre-existing symlinked parent), then create
// the final component here with openExtractFile, which unlinks a planted
// symlink and fails closed on a directory, device, or pipe. ffmpeg -y then
// truncates the regular file we already own instead of following a link out of
// the output folder.
//
// Residual gap, stated plainly: between the close below and ffmpeg's open,
// something with write access to OutputDir can still swap that name for a
// symlink, and ffmpeg would follow it. ffmpegTarget.verify closes the window
// after the fact — the output is rejected and removed when its device+inode no
// longer match the file we created — but by then the foreign write has already
// happened. Only handing ffmpeg an open descriptor would remove the window,
// which is not portable (Windows has no ExtraFiles) and would cost the
// seekable output the FLAC muxer needs to finalize STREAMINFO.
func prepareFFmpegOutput(xFile *XFile, path string) (*ffmpegTarget, error) {
	// Containment is checked on the parent, exactly as writeFile does through
	// mkDir: a symlink at the leaf is openExtractFile's job (it unlinks it),
	// while a symlinked parent is what has to be refused here.
	if !xFile.pathWithinOutput(path) || !xFile.resolvedWithinOutput(filepath.Dir(path)) {
		return nil, fmt.Errorf("%s: %w: %s resolves outside the output folder", xFile.FilePath, ErrInvalidPath, path)
	}

	fout, usedPath, err := openExtractFile(path, xFile.safeFileMode(xFile.FileMode)|ffmpegWriteMode)
	if err != nil {
		return nil, err
	}

	info, err := fout.Stat()
	closeErr := fout.Close()

	switch {
	case err != nil:
		_ = os.Remove(usedPath)

		return nil, fmt.Errorf("stat output file '%s': %w", usedPath, err)
	case closeErr != nil:
		_ = os.Remove(usedPath)

		return nil, fmt.Errorf("closing output file '%s': %w", usedPath, closeErr)
	}

	return &ffmpegTarget{path: usedPath, info: info}, nil
}

// verify reports whether the file at the target path is still the regular file
// prepareFFmpegOutput created. Lstat, not Stat, so a symlink swapped in behind
// our back is caught instead of resolved; a different file at the same name is
// the same race seen after the fact, and is reported as a path conflict.
func (t *ffmpegTarget) verify() (os.FileInfo, error) {
	info, err := os.Lstat(t.path)
	if err != nil {
		return nil, fmt.Errorf("stat output file '%s': %w", t.path, err)
	}

	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return nil, fmt.Errorf("%w: %s", errExtractSymlink, t.path)
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("%w: %s", errExtractNotRegular, t.path)
	case !os.SameFile(t.info, info):
		return nil, fmt.Errorf("%w: %s", errExtractConflict, t.path)
	}

	return info, nil
}

// countOutput verifies the file an external writer just produced is still ours,
// then charges whatever it has grown by since charged bytes were last counted.
// It returns the new charged total. On error the caller removes the file: the
// bytes are on disk already, so this is accounting after the fact.
func (t *ffmpegTarget) countOutput(xFile *XFile, charged uint64) (uint64, error) {
	info, err := t.verify()
	if err != nil {
		return charged, err
	}

	size := uint64(info.Size())
	if size <= charged {
		return charged, nil
	}

	err = xFile.countExternalWrite(size - charged)
	if err != nil {
		return charged, err
	}

	return size, nil
}

// extractCover writes the embedded cover art from src to destNoExt + the
// extension implied by codec ("png" -> .png, mjpeg/jpeg -> .jpg). Returns the
// written path and its size, or ("", 0) if there is no usable cover. Choosing
// the extension from the real codec keeps the file name and the embedded MIME
// (derived from the extension in retagFLAC) correct — matching the pure-Go FLAC
// path. The art counts against MaxFiles and MaxBytes like any other output;
// callers must abort on IsLimitError and may ignore every other failure.
func extractCover(xFile *XFile, src, destNoExt, codec string) (string, uint64, error) {
	ext := ".jpg"
	if strings.EqualFold(codec, "png") {
		ext = ".png"
	}

	err := xFile.countExtracted()
	if err != nil {
		return "", 0, err
	}

	target, err := prepareFFmpegOutput(xFile, destNoExt+ext)
	if err != nil {
		xFile.uncountExtracted()

		return "", 0, err
	}

	size, err := writeCover(xFile, target, src)
	if err != nil {
		_ = os.Remove(target.path)

		xFile.uncountExtracted()

		return "", 0, err
	}

	return target.path, size, nil
}

// writeCover runs the cover extraction into an already-prepared target and
// charges the bytes it produced.
func writeCover(xFile *XFile, target *ffmpegTarget, src string) (uint64, error) {
	args := []string{"-nostdin", "-v", "error", "-y", "-i", src, "-an", "-c:v", "copy", "-frames:v", "1"}
	args = append(args, ffmpegSizeLimit(xFile.extractBytesRemaining())...)
	args = append(args, target.path)

	err := exec.Command("ffmpeg", args...).Run()
	if err != nil {
		return 0, fmt.Errorf("ffmpeg cover %s: %w", target.path, err)
	}

	size, err := target.countOutput(xFile, 0)
	if err != nil {
		return 0, err
	}

	if size == 0 {
		return 0, fmt.Errorf("%w: %s", errEmptyCover, target.path)
	}

	return size, nil
}

// retagFLAC writes Vorbis tags (and an optional cover) onto an existing FLAC
// using metadata-only edits — the audio frames are not re-encoded.
func retagFLAC(path string, tagPairs [][2]string, coverPath string, fileMode os.FileMode) error {
	f, err := goflac.ParseFile(path)
	if err != nil {
		return fmt.Errorf("parsing flac for retag: %w", err)
	}

	// Drop any existing VORBIS_COMMENT / PICTURE blocks (ffmpeg wrote none, but be safe).
	// Filter in place: kept shares f.Meta's backing array, which is safe because
	// len(kept) <= range index at every step (we only ever drop, never reorder), so
	// the slot kept writes has already been read by the loop. Elements are pointers.
	kept := f.Meta[:0]
	for _, b := range f.Meta {
		if b.Type == goflac.VorbisComment || b.Type == goflac.Picture {
			continue
		}

		kept = append(kept, b)
	}

	f.Meta = kept

	cmt := flacvorbis.New()
	cmt.Vendor = "golift.io/xtractr"

	for _, kv := range tagPairs {
		if addErr := cmt.Add(kv[0], kv[1]); addErr != nil {
			return fmt.Errorf("adding tag %s: %w", kv[0], addErr)
		}
	}

	cmtBlock := cmt.Marshal()
	f.Meta = append(f.Meta, &cmtBlock)

	// Cover embedding is best-effort: a missing/unreadable/undecodable cover must not
	// fail the whole re-tag (the tracks are still valid; art is a nice-to-have).
	if coverPath != "" {
		data, readErr := os.ReadFile(coverPath)
		if readErr == nil {
			mime := "image/jpeg"
			if strings.HasSuffix(strings.ToLower(coverPath), ".png") {
				mime = "image/png"
			}

			pic, picErr := flacpicture.NewFromImageData(
				flacpicture.PictureTypeFrontCover, "", data, mime)
			if picErr == nil {
				picBlock := pic.Marshal()
				f.Meta = append(f.Meta, &picBlock)
			}
		}
	}

	if err := f.Save(path); err != nil {
		return fmt.Errorf("saving retagged flac: %w", err)
	}

	_ = os.Chmod(path, fileMode)

	return nil
}

// splitViaFFmpeg splits a non-FLAC source referenced by a CUE into per-track
// FLACs in xFile.OutputDir, tagging each via go-flac. Returns (totalBytes,
// outputFiles, error) — the tracks plus an optional extracted cover file.
// Every file it produces is charged against MaxFiles and MaxBytes/MaxRatio and
// is created through the no-follow path; see prepareFFmpegOutput and ffmpegCut.run.
func splitViaFFmpeg(xFile *XFile, audioPath string, cue *CueSheet, timestamps []cueTimestamp) (uint64, []string, error) {
	if !ffmpegAvailable() {
		return 0, nil, ErrFFmpegNotFound
	}

	probe, err := probeAudio(audioPath)
	if err != nil {
		return 0, nil, err
	}

	if err := os.MkdirAll(xFile.OutputDir, xFile.DirMode); err != nil {
		return 0, nil, fmt.Errorf("creating output directory: %w", err)
	}

	starts := make([]float64, len(timestamps))
	for i, ts := range timestamps {
		starts[i] = ts.toSeconds()
	}

	durs := trackDurations(starts)

	// Extract cover art once (named like the FLAC path: cover.jpg/png), shared by all tracks.
	coverPath, coverSize, err := coverForSplit(xFile, audioPath, probe)
	if err != nil {
		return 0, nil, err
	}

	var (
		total = coverSize
		files = make([]string, 0, len(cue.Tracks)+1)
	)

	for i := range cue.Tracks {
		track := &cue.Tracks[i]
		cut := &ffmpegCut{
			xFile:     xFile,
			src:       audioPath,
			start:     starts[i],
			dur:       durs[i],
			tagPairs:  MergeTrackTags(cue, track, probe.tags),
			coverPath: coverPath,
		}

		// Shared with the FLAC path; the ffmpeg path always re-encodes to FLAC.
		outPath, size, err := cut.run(filepath.Join(xFile.OutputDir, formatTrackFilename(track, ".flac")))
		if err != nil {
			return total, files, err
		}

		total += size

		files = append(files, outPath)
		xFile.Debugf("Wrote track %d via ffmpeg: %s", track.Number, outPath)
	}

	if coverPath != "" {
		files = append(files, coverPath)
	}

	return total, files, nil
}

// coverForSplit extracts the source's embedded art, if any. A cap hit aborts
// the split (the art is output like any other file); every other failure means
// "no art" and the tracks are still written, as they were before.
func coverForSplit(xFile *XFile, audioPath string, probe *audioProbe) (string, uint64, error) {
	if !probe.hasCover {
		return "", 0, nil
	}

	coverPath, size, err := extractCover(xFile, audioPath, filepath.Join(xFile.OutputDir, "cover"), probe.coverCodec)
	switch {
	case IsLimitError(err):
		return "", 0, err
	case err != nil:
		xFile.Debugf("Extracting cover art: %s", err)

		return "", 0, nil
	}

	return coverPath, size, nil
}

// ffmpegCut is one track hand-off to ffmpeg: what to encode and how to tag it.
type ffmpegCut struct {
	xFile     *XFile
	src       string  // source audio path
	start     float64 // track start, seconds into src
	dur       float64 // track duration in seconds; 0 means "to EOF"
	tagPairs  [][2]string
	coverPath string
}

// run writes one track to outPath under the extract caps and returns the path
// actually written (it may be truncated for NAME_MAX) and its size on disk.
//
// The MaxFiles slot is reserved before ffmpeg is started so an over-budget
// extract does not pay for a decode it must then throw away, and it is handed
// back whenever the output is removed. Bytes are charged after the fact,
// because ffmpeg writes them itself: the cap can only be enforced once the
// file exists, so an over-cap track is deleted rather than never written. The
// -fs brake in cutTrackFLAC keeps that overshoot near the remaining budget
// instead of a whole track. This is the residual difference from the streamed
// paths, where the write that would cross MaxBytes is refused outright.
func (c *ffmpegCut) run(outPath string) (string, uint64, error) {
	err := c.xFile.countExtracted()
	if err != nil {
		return "", 0, err
	}

	target, err := prepareFFmpegOutput(c.xFile, outPath)
	if err != nil {
		c.xFile.uncountExtracted()

		return "", 0, err
	}

	size, err := c.encode(target)
	if err != nil {
		_ = os.Remove(target.path)

		c.xFile.uncountExtracted()

		return target.path, 0, err
	}

	return target.path, size, nil
}

// encode cuts the track, charges what ffmpeg wrote, then tags the result in
// place and charges whatever the tags and cover art added on top.
func (c *ffmpegCut) encode(target *ffmpegTarget) (uint64, error) {
	err := cutTrackFLAC(c.src, target.path, c.start, c.dur, c.xFile.extractBytesRemaining())
	if err != nil {
		return 0, err
	}

	// Charged before the re-tag: a track truncated by the -fs brake is over
	// budget and must fail as ErrMaxBytes, not as a FLAC parse error.
	size, err := target.countOutput(c.xFile, 0)
	if err != nil {
		return 0, err
	}

	err = retagFLAC(target.path, c.tagPairs, c.coverPath, c.xFile.FileMode)
	if err != nil {
		return 0, err
	}

	// go-flac rewrites the file in place (same inode), so this both charges the
	// metadata growth and re-checks that nothing swapped the path underneath it.
	return target.countOutput(c.xFile, size)
}
