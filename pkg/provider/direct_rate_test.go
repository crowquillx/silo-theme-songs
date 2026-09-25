package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testOriginSequence atomic.Uint64

func TestDirectCooldownAppliesToOtherFilesAndNewDownloader(t *testing.T) {
	var calls atomic.Int32
	host := fmt.Sprintf("direct-cooldown-%d.example.org", testOriginSequence.Add(1))
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"3600"}}, Body: io.NopCloser(strings.NewReader("busy")), Request: req}, nil
	})}
	paths := tools(t, "exit 1", "exit 1")
	for _, file := range []string{"first.mp3", "second.mp3"} {
		d := &Downloader{Tools: paths, StageParent: t.TempDir(), Client: client, AllowedDirectHosts: []string{host}}
		start := time.Now()
		_, err := d.Fetch(context.Background(), Source{URL: "https://" + host + "/" + file, Format: "mp3"})
		if !IsCode(err, Transient) {
			t.Fatalf("cooldown classified as permanent failure: %v", err)
		}
		if calls.Load() != 1 || time.Since(start) > time.Second {
			t.Fatalf("request escaped cooldown: calls=%d elapsed=%s", calls.Load(), time.Since(start))
		}
	}
}

func TestAllowedRedirectHonorsTargetServiceCooldown(t *testing.T) {
	var targetCalls atomic.Int32
	var redirect bool
	id := testOriginSequence.Add(1)
	startHost := fmt.Sprintf("redirect-start-%d.example.org", id)
	targetHost := fmt.Sprintf("redirect-target-%d.example.org", id)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status := 429
		headers := http.Header{"Retry-After": []string{"3600"}}
		if req.URL.Hostname() == startHost {
			redirect = true
			status = 302
			headers = http.Header{"Location": []string{"https://" + targetHost + "/theme.mp3"}}
		} else {
			targetCalls.Add(1)
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})}
	d := &Downloader{Tools: tools(t, "exit 1", "exit 1"), StageParent: t.TempDir(), Client: client, AllowedDirectHosts: []string{startHost, targetHost}}
	for _, host := range []string{targetHost, startHost} {
		_, err := d.Fetch(context.Background(), Source{URL: "https://" + host + "/theme.mp3", Format: "mp3"})
		if !IsCode(err, Transient) {
			t.Fatal(err)
		}
	}
	if !redirect || targetCalls.Load() != 1 {
		t.Fatalf("redirect bypassed cooldown: redirect=%v targetCalls=%d", redirect, targetCalls.Load())
	}
}

func TestRedirectResponseCooldownDefersWithoutRevisitingSource(t *testing.T) {
	id := testOriginSequence.Add(1)
	startHost := fmt.Sprintf("redirect-response-start-%d.example.org", id)
	targetHost := fmt.Sprintf("redirect-response-target-%d.example.org", id)
	var sourceCalls, targetCalls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status := 429
		headers := http.Header{"Retry-After": {"3600"}}
		if req.URL.Hostname() == startHost {
			sourceCalls.Add(1)
			status = 302
			headers = http.Header{"Location": {"https://" + targetHost + "/theme.mp3"}}
		} else {
			targetCalls.Add(1)
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})}
	d := &Downloader{Tools: tools(t, "exit 1", "exit 1"), StageParent: t.TempDir(), Client: client, AllowedDirectHosts: []string{startHost, targetHost}}
	_, err := d.Fetch(context.Background(), Source{URL: "https://" + startHost + "/theme.mp3", Format: "mp3"})
	if !IsCode(err, Transient) || sourceCalls.Load() != 1 || targetCalls.Load() != 1 {
		t.Fatalf("revisited source during redirected cooldown: source=%d target=%d err=%v", sourceCalls.Load(), targetCalls.Load(), err)
	}
}
