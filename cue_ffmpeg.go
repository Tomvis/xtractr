package xtractr

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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
}

// ffprobeOutput mirrors the JSON we read from ffprobe.
type ffprobeOutput struct {
	Format struct {
		Duration string            `json:"duration"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		CodecType   string `json:"codec_type"`
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
