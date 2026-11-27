package provider

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A process-wide gate prevents search and download jobs, including newly
// configured Downloader instances, from issuing concurrent YouTube requests.
var youtubeRequests = newYouTubeLimiter(10 * time.Second)

type youtubeLimiter struct {
	token    chan struct{}
	interval time.Duration
	next     time.Time // accessed only while holding token
}

func newYouTubeLimiter(interval time.Duration) *youtubeLimiter {
	return &youtubeLimiter{token: make(chan struct{}, 1), interval: interval}
}

func (d *Downloader) youtubeLimit() *youtubeLimiter {
	if d.youtube != nil {
		return d.youtube
	}
	return youtubeRequests
}

func (l *youtubeLimiter) acquire(ctx context.Context) (func(error), error) {
	select {
	case l.token <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-l.token
		return nil, err
	}
	delay := time.Until(l.next)
	if deadline, ok := ctx.Deadline(); ok && delay > time.Until(deadline) {
		<-l.token
		return nil, &Error{Transient, "YouTube cooldown; retry later"}
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			<-l.token
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		<-l.token
		return nil, err
	}
	return func(err error) {
		delay := l.interval
		// yt-dlp does not expose HTTP retry headers through its CLI. Stop its
		// automatic retries and defer all jobs conservatively on throttling.
		if IsCode(err, RateLimited) || IsCode(err, AuthenticationRequired) {
			delay = max(delay, time.Hour)
		} else if IsCode(err, Transient) {
			delay = max(delay, time.Minute)
		}
		l.next = time.Now().Add(delay)
		<-l.token
	}, nil
}

func youtubeArgs() []string {
	return []string{
		"--ignore-config", "--no-config-locations", "--no-plugin-dirs", "--no-cache-dir", "--no-remote-components",
		"--sleep-requests", "3", "--sleep-interval", "5", "--max-sleep-interval", "10",
		"--concurrent-fragments", "1", "--retries", "0", "--fragment-retries", "0", "--extractor-retries", "0",
	}
}

func resolveJSRuntime(ctx context.Context, spec string) (argument, version string, err error) {
	choices := []string{spec}
	if spec == "" {
		choices = []string{"deno", "node"}
	}
	for _, choice := range choices {
		kind, path, hasPath := strings.Cut(choice, ":")
		if kind != "node" && kind != "deno" {
			return "", "", &Error{MissingTool, "js_runtime must be deno[:path] or node[:path]"}
		}
		if !hasPath {
			path = kind
		}
		executable, e := exec.LookPath(path)
		if e != nil {
			continue
		}
		executable, e = filepath.Abs(executable)
		if e != nil {
			continue
		}
		v, e := checkTool(ctx, executable, kind, "--version")
		if e != nil {
			if ctx.Err() != nil {
				return "", "", ctx.Err()
			}
			continue
		}
		return kind + ":" + executable, kind + " " + v, nil
	}
	return "", "", &Error{MissingTool, "YouTube requires Deno 2.3+ or Node 22+; set js_runtime"}
}

// Classify known failures without returning raw tool output, URLs or tokens.
func extractorError(output []byte) error {
	text := strings.ToLower(string(output))
	switch {
	case strings.Contains(text, "http error 429"), strings.Contains(text, "too many requests"), strings.Contains(text, "this content isn't available, try again later"):
		return &Error{RateLimited, "YouTube throttled; requests paused for one hour"}
	case strings.Contains(text, "sign in to confirm"), strings.Contains(text, "login required"), strings.Contains(text, "authentication required"):
		return &Error{AuthenticationRequired, "YouTube requires authentication; use an accessible selected source"}
	case strings.Contains(text, "no supported javascript runtime"), strings.Contains(text, "challenge solving failed"), strings.Contains(text, "challenge solver"), strings.Contains(text, "yt-dlp-ejs"):
		return &Error{MissingTool, "update yt-dlp with its EJS package and configure a supported js_runtime"}
	case mentionsAny(text, unavailableReasons):
		return &Error{UnavailableMedia, "YouTube video is unavailable"}
	case mentionsAny(text, unavailableWithoutReason):
		return &Error{UnavailableMedia, unconfirmedUnavailable}
	case strings.Contains(text, "http error 503"), strings.Contains(text, "http error 502"), strings.Contains(text, "http error 500"), strings.Contains(text, "timed out"), strings.Contains(text, "unable to download"), strings.Contains(text, "temporary failure"):
		return &Error{Transient, "YouTube transport; retry later"}
	default:
		return &Error{UnavailableMedia, unexplainedFailure}
	}
}

// unexplainedFailure is the Op of a yt-dlp failure no known message explains.
// Only these failures count toward the extractor circuit breaker: a video that
// YouTube reports as unavailable shows that yt-dlp itself still works.
const unexplainedFailure = "extractor"

// unconfirmedUnavailable is the Op of a video reported unavailable without a
// reason. Usually the video is gone, but an outdated yt-dlp whose YouTube
// clients fail can say the same about a playable video, so it asks for an
// early yt-dlp update check without opening the circuit breaker.
const unconfirmedUnavailable = "YouTube reports the video unavailable"

// unavailableReasons are YouTube's reasons for a video that no client can
// play: deleted, private, blocked or limited to channel members. yt-dlp prints
// most of them after "Video unavailable.".
var unavailableReasons = []string{
	"video is no longer available",
	"private video",
	"video is private",
	"has been removed",
	"account associated with this video has been terminated",
	"not made this video available in your country",
	"not available in your country",
	"not available from your location",
	"blocked it in your country",
	"members-only",
	"available to this channel's members",
	"join this channel to get access",
}

var unavailableWithoutReason = []string{
	"video unavailable",
	"video is unavailable",
	"video is not available",
}

func mentionsAny(text string, substrings []string) bool {
	for _, m := range substrings {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}
