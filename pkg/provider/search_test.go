package provider

import (
	"context"
	"testing"
)

func TestSearchReturnsReviewCandidatesAndRejectsCoversAndWrongTitles(t *testing.T) {
	dir := t.TempDir()
	yt := script(t, dir, "yt-dlp", `if [ "$1" = '--version' ]; then echo '2026.09.01'; exit 0; fi
printf '%s' '{"entries":[{"id":"abcdefghijk","title":"Example Show 2024 Opening Theme","duration":90,"channel":"Official-looking"},{"id":"bcdefghijkl","title":"Example Show Cover Theme","duration":100},{"id":"cdefghijklm","title":"Unrelated Show Opening Theme","duration":80},{"id":"defghijklmn","title":"Example Show Full Album","duration":400},{"id":"efghijklmno","title":"Example Show Piano Instrumental","duration":80}]}'`)
	d := &Downloader{youtube: newYouTubeLimiter(0), Tools: ToolPaths{YTDLP: yt}}
	got, e := d.Search(context.Background(), SearchOptions{Title: "Example Show", Year: 2024})
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 1 || got[0].ID != "abcdefghijk" || len(got[0].Reasons) == 0 {
		t.Fatalf("candidates %+v", got)
	}
	got, e = d.Search(context.Background(), SearchOptions{Title: "Example Show", AllowCovers: true, AllowInstrumental: true})
	if e != nil || len(got) != 3 {
		t.Fatalf("opt-in candidates %+v %v", got, e)
	}
}
