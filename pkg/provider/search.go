package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

type ReviewCandidate struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	URL      string   `json:"url"`
	Channel  string   `json:"channel,omitempty"`
	Duration float64  `json:"duration_seconds,omitempty"`
	Score    int      `json:"score"`
	Reasons  []string `json:"reasons"`
}
type ReviewRequired struct {
	Candidates []ReviewCandidate
	Reason     string
}

func (e *ReviewRequired) Error() string { return "needs_review: " + e.Reason }

type SearchOptions struct {
	Title             string
	Year              int
	Soundtrack        bool
	AllowCovers       bool
	AllowInstrumental bool
}

var videoID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
var words = regexp.MustCompile(`[^a-z0-9]+`)

// Search produces candidates only. The caller must persist an explicit operator
// URL selection before passing any result to Fetch.
func (d *Downloader) Search(ctx context.Context, o SearchOptions) (_ []ReviewCandidate, resultErr error) {
	if strings.TrimSpace(o.Title) == "" || len(o.Title) > 300 {
		return nil, &Error{Malformed, "search title"}
	}
	if _, e := checkTool(ctx, d.Tools.YTDLP, "yt-dlp", "--version"); e != nil {
		return nil, e
	}
	query := strings.TrimSpace(o.Title)
	if o.Year > 0 {
		query += fmt.Sprintf(" %d", o.Year)
	}
	if o.Soundtrack {
		query += " soundtrack theme"
	} else {
		query += " opening title theme"
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	release, err := d.youtubeLimit().acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { release(resultErr) }()
	args := append(youtubeArgs(), "--skip-download", "--flat-playlist", "--dump-single-json", "--playlist-end", "8", "--socket-timeout", "10", "--", "ytsearch8:"+query)
	cmd := exec.Command(d.Tools.YTDLP, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "XDG_CONFIG_HOME=/nonexistent"}
	var output limitedBuffer
	output.max = 1 << 20
	cmd.Stdout = &output
	stderr := &limitedBuffer{max: 4096}
	cmd.Stderr = stderr
	if e := runBounded(ctx, cmd, "", 0); e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		output, _ := stderr.snapshot()
		return nil, extractorError(output)
	}
	data, overflow := output.snapshot()
	if overflow {
		return nil, &Error{LimitExceeded, "candidate metadata"}
	}
	var listing struct {
		Entries []struct {
			ID           string  `json:"id"`
			Title        string  `json:"title"`
			Channel      string  `json:"channel"`
			Duration     float64 `json:"duration"`
			LiveStatus   string  `json:"live_status"`
			Availability string  `json:"availability"`
		} `json:"entries"`
	}
	if json.Unmarshal(data, &listing) != nil || len(listing.Entries) > 8 {
		return nil, &Error{Malformed, "candidate metadata"}
	}
	needle := strings.TrimSpace(words.ReplaceAllString(strings.ToLower(o.Title), " "))
	var result []ReviewCandidate
	for _, v := range listing.Entries {
		if !videoID.MatchString(v.ID) || v.Title == "" || v.Duration > 900 || v.LiveStatus == "is_live" || v.LiveStatus == "is_upcoming" {
			continue
		}
		if v.Availability != "" && v.Availability != "public" && v.Availability != "unlisted" {
			continue
		}
		title := strings.TrimSpace(words.ReplaceAllString(strings.ToLower(v.Title), " "))
		if !strings.Contains(" "+title+" ", " "+needle+" ") {
			continue
		}
		if containsAny(title, []string{"trailer", "compilation", "full album", "full soundtrack", "reaction", "tutorial", "karaoke", "nightcore", "slowed", "sped up"}) {
			continue
		}
		if !o.AllowCovers && containsAny(title, []string{"cover", "remix", "reimagined"}) {
			continue
		}
		if !o.AllowInstrumental && containsAny(title, []string{"instrumental", "piano", "orchestral version"}) {
			continue
		}
		score := 10
		reasons := []string{"title contains the catalog title; identity still needs review"}
		if o.Year > 0 && strings.Contains(title, fmt.Sprint(o.Year)) {
			score += 3
			reasons = append(reasons, "catalog year appears in title")
		}
		if !o.Soundtrack && containsAny(title, []string{"opening", "title theme", "main title", "intro"}) {
			score += 2
		}
		if o.Soundtrack && strings.Contains(title, "soundtrack") {
			score += 2
		}
		if v.Duration == 0 {
			reasons = append(reasons, "duration is missing")
		}
		if len(strings.Fields(needle)) < 2 {
			reasons = append(reasons, "generic title requires extra identity checking")
		}
		reasons = append(reasons, "check remake, year, and soundtrack identity before saving this URL")
		result = append(result, ReviewCandidate{v.ID, v.Title, "https://www.youtube.com/watch?v=" + v.ID, v.Channel, v.Duration, score, reasons})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Score != result[j].Score {
			return result[i].Score > result[j].Score
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}
func containsAny(s string, terms []string) bool {
	for _, v := range terms {
		if strings.Contains(" "+s+" ", " "+v+" ") {
			return true
		}
	}
	return false
}
