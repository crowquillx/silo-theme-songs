package provider

import (
	"context"
	"os"
	"testing"
)

// yt-dlp exits 101 when --max-downloads 1 is reached, after it has
// successfully downloaded and postprocessed the requested video.
func TestExtractionAcceptsDownloadLimitExitOnlyWithValidAudio(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       Code
	}{
		{"completed", "printf audio > theme.mp3; exit 101", ""},
		{"no audio", "exit 101", UnavailableMedia},
		{"actual failure", "printf audio > theme.mp3; exit 1", UnavailableMedia},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage := t.TempDir()
			d := &Downloader{youtube: newYouTubeLimiter(0), Tools: tools(t, tc.body, `echo '{"format":{"format_name":"mp3"},"streams":[{"codec_type":"audio","codec_name":"mp3","channels":2}]}'`), StageParent: stage}
			audio, err := d.Fetch(context.Background(), Source{URL: "https://youtu.be/abcdefghijk", Format: "mp3", Extract: true})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("completed single-video extraction rejected: %v", err)
				}
				if audio.Size != 5 {
					t.Fatalf("unexpected audio: %+v", audio)
				}
				if err := audio.Close(); err != nil {
					t.Fatal(err)
				}
			} else if !IsCode(err, tc.want) {
				t.Fatalf("want %s, got %v", tc.want, err)
			}
			entries, err := os.ReadDir(stage)
			if err != nil || len(entries) != 0 {
				t.Fatalf("staging leaked: %v %v", entries, err)
			}
		})
	}
}
