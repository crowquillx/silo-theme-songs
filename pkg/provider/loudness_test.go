package provider

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

func TestNormalizationGain(t *testing.T) {
	for _, tc := range []struct {
		name, i, tp string
		want        float64
	}{
		{"quiet theme is raised", "-30", "-20", 14},
		{"loud theme is lowered", "-8", "-0.2", -8},
		{"peaks limit the boost", "-26", "-4", 3},
		{"silence keeps its level", "-inf", "-inf", 0},
		{"boost is bounded", "-69", "-60", 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizationGain(-16, tc.i, tc.tp)
			if err != nil || math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("gain=%v err=%v, want %v", got, err, tc.want)
			}
		})
	}
	if _, err := normalizationGain(-16, "loud", "-1"); !IsCode(err, InvalidAudio) {
		t.Fatalf("unreadable measurement: %v", err)
	}
}

func TestExtractedAudioKeepsTheSourceCodec(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"theme.webm.part", "deno", "theme.opus"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path, format, err := extractedAudio(dir)
	if err != nil || filepath.Base(path) != "theme.opus" || format != "opus" {
		t.Fatalf("path=%s format=%s err=%v", path, format, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "theme.m4a"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := extractedAudio(dir); !IsCode(err, InvalidAudio) {
		t.Fatalf("several files accepted: %v", err)
	}
	if _, _, err := extractedAudio(t.TempDir()); !IsCode(err, UnavailableMedia) {
		t.Fatalf("no file: %v", err)
	}
}

var integratedLoudness = regexp.MustCompile(`"input_i" : "(-?[0-9.]+)"`)

func measure(t *testing.T, ffmpeg, path string) float64 {
	t.Helper()
	out := ffmpegCommand(t, ffmpeg, "-v", "info", "-i", path, "-af", "loudnorm=print_format=json", "-f", "null", "-")
	m := integratedLoudness.FindSubmatch(out)
	if m == nil {
		t.Fatalf("no measurement: %s", out)
	}
	v, _ := strconv.ParseFloat(string(m[1]), 64)
	return v
}

// Themes as quiet as an old film score and as loud as an anime opening come
// out at the same level.
func TestRealLoudnessNormalization(t *testing.T) {
	paths := requireFFmpeg(t)
	for _, tc := range []struct {
		name, ext, codec string
		volume           float64
	}{
		{"quiet-ogg", "ogg", "libvorbis", 0.03},
		{"loud-ogg", "ogg", "libvorbis", 3},
		{"quiet-mp3", "mp3", "libmp3lame", 0.03},
		{"loud-mp3", "mp3", "libmp3lame", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := filepath.Join(t.TempDir(), "source."+tc.ext)
			// Pink noise has music's spread of frequencies; a sine wave
			// would measure differently through the K-weighting.
			ffmpegCommand(t, paths.FFmpeg, "-f", "lavfi", "-i", "anoisesrc=color=pink:duration=4:sample_rate=48000:seed=1", "-af", "volume="+strconv.FormatFloat(tc.volume, 'f', 2, 64), "-ac", "2", "-c:a", tc.codec, input)
			body, err := os.ReadFile(input)
			if err != nil {
				t.Fatal(err)
			}
			d, source := fixtureDownload(t, paths, tc.ext, body)
			d.Loudness = -16
			staged, err := d.Fetch(context.Background(), source)
			if err != nil {
				t.Fatal(err)
			}
			defer staged.Close()
			before, after := measure(t, paths.FFmpeg, input), measure(t, paths.FFmpeg, staged.Path)
			if math.Abs(before+16) < 2 {
				t.Fatalf("fixture already at the target: %.1f LUFS", before)
			}
			t.Logf("%.1f LUFS → %.1f LUFS", before, after)
			if math.Abs(after+16) > 1 {
				t.Fatalf("%.1f LUFS became %.1f, want -16", before, after)
			}
			if staged.Close() != nil {
				t.Fatal("close")
			}
			assertNoStaging(t, d.StageParent)
		})
	}
}

// Off keeps an MP3 byte for byte; TestRealMP3Downloads covers that.
func TestLoudnessNeedsFFmpegEvenForMP3(t *testing.T) {
	paths := tools(t, "exit 1", "exit 1")
	paths.FFmpeg = "/nonexistent/ffmpeg"
	d, source := fixtureDownload(t, paths, "mp3", nil)
	if _, err := d.Preflight(context.Background(), source); err != nil {
		t.Fatalf("MP3 without loudness needs no FFmpeg: %v", err)
	}
	d.Loudness = -16
	if _, err := d.Preflight(context.Background(), source); !IsCode(err, MissingTool) {
		t.Fatalf("loudness preflight: %v", err)
	}
}
