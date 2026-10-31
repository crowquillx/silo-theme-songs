package pluginapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/crowquillx/silo-theme-songs/pkg/siloapi"
	"google.golang.org/protobuf/types/known/structpb"
)

// pageLibrary is one row of the status page's library list.
type pageLibrary struct {
	ID, Name, Type string
	Selected       bool
	Enabled        bool
}

type pageData struct {
	PluginID   string
	Setup      string
	DryRun     bool
	Progress   string
	Libraries  []pageLibrary
	LibraryErr string
	Report     Report
	NewReport  *Report
	Saved      string
}

var page = template.Must(template.New("status").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Theme Songs</title><style>body{font:16px system-ui,sans-serif;line-height:1.5;max-width:1200px;margin:32px auto;padding:0 24px;color:#20242b;background:#fafafa}h1{font-size:28px}h2{font-size:20px;margin-top:32px}table{border-collapse:collapse;width:100%;background:white}th,td{text-align:left;vertical-align:top;padding:12px;border-bottom:1px solid #ddd}th{font-weight:600}code{overflow-wrap:anywhere}p,ul{max-width:75ch}small{color:#555}.box{background:white;border:1px solid #ddd;border-left:4px solid #b54708;padding:12px 16px;margin:16px 0}.ok{border-left-color:#067647}form{display:inline}button{font:inherit;padding:6px 14px;margin:4px 8px 4px 0;border:1px solid #888;border-radius:6px;background:white;cursor:pointer}button.primary{background:#20242b;color:white;border-color:#20242b}label{display:block;padding:4px 0}</style>
<h1>Theme Songs</h1>
<p><a href="/admin/plugins/{{.PluginID}}">Settings</a> · <a href="/admin/tasks">Scheduled Tasks</a> · <a href="/">Back to Silo</a></p>
{{if .Setup}}<div class="box"><strong>Setup needed.</strong> {{.Setup}}{{if eq .Setup "Create an API key in Administration → API Keys, paste it into Silo API key in this plugin's settings and save."}} <a href="/admin/api-keys">Open API Keys</a> · <a href="/admin/plugins/{{.PluginID}}">Open settings</a>{{end}}</div>{{end}}
{{if .Saved}}<div class="box ok">{{.Saved}}</div>{{end}}
{{if not .Setup}}<p>Theme Songs runs on its own: once a day, after Silo scans a library with new items, and when Silo starts.{{if .DryRun}} <strong>Dry run is on</strong>, so runs only list what they would download; turn it off in Settings to download.{{end}}</p>
<form method="post" action="run"><input type="hidden" name="mode" value="sync"><button class="primary" type="submit">{{if .DryRun}}Run dry run now{{else}}Download now{{end}}</button></form>{{if not .DryRun}}<form method="post" action="run"><input type="hidden" name="mode" value="preview"><button type="submit">Preview</button></form>{{end}}{{end}}
{{if .Progress}}<p><strong>Running.</strong> {{.Progress}}. Reload for updates.</p>{{end}}
{{if .Libraries}}<h2>Libraries</h2><form method="post" action="libraries"><p>Theme Songs uses the checked libraries. New libraries are checked automatically.</p>{{range .Libraries}}<label><input type="checkbox" name="library" value="{{.ID}}"{{if .Selected}} checked{{end}}> {{.Name}} <small>{{.Type}}, ID {{.ID}}{{if not .Enabled}}, disabled{{end}}</small></label>{{end}}<button type="submit">Save libraries</button></form>{{else if .LibraryErr}}<h2>Libraries</h2><p>{{.LibraryErr}}</p>{{end}}
{{with .Report}}{{if .Mode}}<h2>Last run</h2>
<p>State: {{.State}}{{if .Mode}} ({{.Mode}}, {{.At.Format "2006-01-02 15:04 MST"}}){{end}}. {{.Downloaded}} themes downloaded.</p>
{{range .Errors}}<div class="box">{{.}}</div>{{end}}{{range .Notices}}<p>{{.}}</p>{{end}}
{{if .Summary}}<h2>Summary</h2><table><thead><tr><th>Count</th><th>Action</th><th>Reason</th></tr></thead><tbody>{{range .Summary}}<tr><td>{{.Count}}</td><td>{{.Action}}</td><td><small>{{.Reason}}</small></td></tr>{{end}}</tbody></table>{{end}}{{end}}{{end}}
{{with .NewReport}}<h2>New items, {{.At.Format "2006-01-02 15:04 MST"}}</h2><p>State: {{.State}}. {{.Downloaded}} themes downloaded.</p>{{range .Errors}}<div class="box">{{.}}</div>{{end}}{{if .Rows}}<table><thead><tr><th>Item / season</th><th>Theme and source</th><th>Destination</th><th>Action</th></tr></thead><tbody>{{range .Rows}}<tr><td>{{.Item}}{{if ge .Season 0}}<br><small>Season {{.Season}}</small>{{end}}</td><td>{{.Theme}}<br><small>{{.Source}}</small></td><td><code>{{.Destination}}</code></td><td>{{.Action}}<br><small>{{.Reason}}</small></td></tr>{{end}}</tbody></table>{{end}}{{end}}
{{with .Report}}{{if .Rows}}<h2>Items</h2><table><thead><tr><th>Item / season</th><th>Theme and source</th><th>Destination</th><th>Action</th></tr></thead><tbody>{{range .Rows}}<tr><td>{{.Item}}{{if ge .Season 0}}<br><small>Season {{.Season}}</small>{{end}}</td><td>{{.Theme}}<br><small>{{.Source}}</small></td><td><code>{{.Destination}}</code></td><td>{{.Action}}<br><small>{{.Reason}}</small>{{range .Candidates}}<br><a href="{{.URL}}" target="_blank" rel="noopener noreferrer">{{.Title}}</a> <small>{{.Channel}} (score {{.Score}})</small>{{end}}</td></tr>{{end}}</tbody></table>{{if .Truncated}}<p>This page shows 1,000 rows, problems and planned downloads first. The summary counts every item.</p>{{end}}{{end}}{{end}}
</html>`))

// statusJSON is the /status response.
type statusJSON struct {
	Report
	Progress  string  `json:"progress,omitempty"`
	Setup     string  `json:"setup,omitempty"`
	NewReport *Report `json:"new_items_report,omitempty"`
}

func (s *Server) Handle(ctx context.Context, req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	switch {
	case req.GetMethod() == "GET" && req.GetPath() == "/status":
		s.mu.RLock()
		out := statusJSON{Report: s.report, Progress: s.progress, Setup: errorText(s.configErr)}
		if !s.newReport.At.IsZero() {
			r := s.newReport
			out.NewReport = &r
		}
		s.mu.RUnlock()
		b, err := json.Marshal(out)
		if err != nil {
			return nil, err
		}
		return response(200, "application/json", b), nil
	case req.GetMethod() == "GET" && req.GetPath() == "/":
		return s.renderPage(ctx, query(req.GetQuery(), "saved"))
	case req.GetMethod() == "POST" && req.GetPath() == "/run":
		form, _ := url.ParseQuery(string(req.GetBody()))
		mode := form.Get("mode")
		if mode != "sync" && mode != "preview" {
			return response(400, "text/plain; charset=utf-8", []byte("unknown run mode")), nil
		}
		s.mu.RLock()
		configured := s.general != nil
		s.mu.RUnlock()
		if configured {
			s.start(mode, nil)
		}
		return redirect("./"), nil
	case req.GetMethod() == "POST" && req.GetPath() == "/libraries":
		form, _ := url.ParseQuery(string(req.GetBody()))
		if err := s.saveLibraries(ctx, form["library"]); err != nil {
			return response(409, "text/plain; charset=utf-8", []byte(err.Error())), nil
		}
		return redirect("./?saved=libraries"), nil
	case req.GetPath() == "/" || req.GetPath() == "/status" || req.GetPath() == "/run" || req.GetPath() == "/libraries":
		return &pluginv1.HandleHTTPResponse{StatusCode: 405}, nil
	}
	return &pluginv1.HandleHTTPResponse{StatusCode: 404}, nil
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func query(q *structpb.Struct, key string) string {
	switch v := q.GetFields()[key].GetKind().(type) {
	case *structpb.Value_StringValue:
		return v.StringValue
	case *structpb.Value_ListValue:
		if values := v.ListValue.GetValues(); len(values) > 0 {
			return values[0].GetStringValue()
		}
	}
	return ""
}

func response(status int32, contentType string, body []byte) *pluginv1.HandleHTTPResponse {
	return &pluginv1.HandleHTTPResponse{StatusCode: status, Headers: map[string]string{"Content-Type": contentType, "Cache-Control": "no-store", "Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'self'", "X-Content-Type-Options": "nosniff"}, Body: body}
}

// redirect answers a form post. The location is relative to the posted
// route, so it stays on the plugin's page whatever prefix Silo serves it at.
func redirect(location string) *pluginv1.HandleHTTPResponse {
	return &pluginv1.HandleHTTPResponse{StatusCode: 303, Headers: map[string]string{"Location": location, "Cache-Control": "no-store"}}
}

func (s *Server) renderPage(ctx context.Context, saved string) (*pluginv1.HandleHTTPResponse, error) {
	s.mu.RLock()
	data := pageData{PluginID: s.manifest.GetPluginId(), Setup: errorText(s.configErr), DryRun: s.cfg.dryRun(), Progress: s.progress, Report: s.report}
	if !s.newReport.At.IsZero() && s.newReport.At.After(s.report.At) {
		r := s.newReport
		data.NewReport = &r
	}
	cfg, api := s.cfg, s.client
	s.mu.RUnlock()
	if saved == "libraries" {
		data.Saved = "Libraries saved. The next run uses them."
	}
	if api != nil {
		libs, err := api.Libraries(ctx)
		if err != nil {
			data.LibraryErr = "Cannot list libraries: " + err.Error()
		}
		for _, l := range libs {
			if siloapi.Eligible(l) {
				data.Libraries = append(data.Libraries, pageLibrary{ID: l.ID, Name: l.Name, Type: l.Type, Selected: cfg.selected(l), Enabled: l.Enabled})
			}
		}
		if err == nil && len(data.Libraries) == 0 {
			data.LibraryErr = "Silo has no movie or series libraries yet."
		}
	}
	var buf bytes.Buffer
	if err := page.Execute(&buf, data); err != nil {
		return nil, err
	}
	return response(200, "text/html; charset=utf-8", buf.Bytes()), nil
}

// saveLibraries stores the checked libraries. It saves the unchecked ones as
// skipped and clears the only-these list, so libraries added later are used.
func (s *Server) saveLibraries(ctx context.Context, checked []string) error {
	host := sdkruntime.Host()
	if host == nil {
		return errors.New("Silo is not connected to the plugin; reload and try again")
	}
	s.mu.RLock()
	api, raw := s.client, maps.Clone(s.raw)
	s.mu.RUnlock()
	if api == nil {
		return errors.New("save an API key in the plugin's settings first")
	}
	libs, err := api.Libraries(ctx)
	if err != nil {
		return err
	}
	var skipped []string
	for _, l := range libs {
		if siloapi.Eligible(l) && !slices.Contains(checked, l.ID) {
			skipped = append(skipped, l.ID)
		}
	}
	if raw == nil {
		raw = map[string]any{}
	}
	delete(raw, "library_ids")
	raw["skip_library_ids"] = strings.Join(skipped, ", ")
	cfg, err := parseSettings(raw)
	if err != nil {
		return err
	}
	if err := host.SetGlobalConfigEntry(ctx, "settings", raw); err != nil {
		return err
	}
	// Silo saves the entry without restarting the plugin, so apply it here.
	setupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.ConfigureLocal(setupCtx, cfg); err != nil {
		return err
	}
	s.mu.Lock()
	s.raw = raw
	s.mu.Unlock()
	return nil
}
