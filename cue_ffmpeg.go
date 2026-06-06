package xtractr

import (
	"encoding/json"
	"fmt"
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
func cutTrackFLAC(src, outPath string, startSec, durSec float64) error {
	args := []string{"-nostdin", "-v", "error", "-ss", formatSeconds(startSec), "-i", src}
	if durSec > 0 {
		args = append(args, "-t", formatSeconds(durSec))
	}

	args = append(args, "-vn", "-c:a", "flac", "-compression_level", "8",
		"-map_metadata", "-1", "-y", outPath)

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

// extractCover writes the embedded cover art from src to destNoExt + the
// extension implied by codec ("png" -> .png, mjpeg/jpeg -> .jpg). Returns the
// written path, or "" if there is no cover. Choosing the extension from the real
// codec keeps the file name and the embedded MIME (derived from the extension in
// retagFLAC) correct — matching the pure-Go FLAC path.
func extractCover(src, destNoExt, codec string) string {
	ext := ".jpg"
	if strings.EqualFold(codec, "png") {
		ext = ".png"
	}

	dest := destNoExt + ext

	cmd := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y",
		"-i", src, "-an", "-c:v", "copy", "-frames:v", "1", dest)
	if err := cmd.Run(); err == nil {
		if fi, statErr := os.Stat(dest); statErr == nil && fi.Size() > 0 {
			return dest
		}
	}

	_ = os.Remove(dest)

	return ""
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
	var coverPath string
	if probe.hasCover {
		coverPath = extractCover(audioPath, filepath.Join(xFile.OutputDir, "cover"), probe.coverCodec)
	}

	var (
		total uint64
		files = make([]string, 0, len(cue.Tracks)+1)
	)

	for i := range cue.Tracks {
		track := &cue.Tracks[i]
		outName := formatTrackFilename(track) // shared with the FLAC path
		outPath := filepath.Join(xFile.OutputDir, outName)

		if err := cutTrackFLAC(audioPath, outPath, starts[i], durs[i]); err != nil {
			return total, files, err
		}

		pairs := MergeTrackTags(cue, track, probe.tags)
		if err := retagFLAC(outPath, pairs, coverPath, xFile.FileMode); err != nil {
			return total, files, err
		}

		if fi, statErr := os.Stat(outPath); statErr == nil {
			total += uint64(fi.Size())
		}

		files = append(files, outPath)
		xFile.Debugf("Wrote track %d via ffmpeg: %s", track.Number, outPath)
	}

	if coverPath != "" {
		files = append(files, coverPath)
	}

	return total, files, nil
}
