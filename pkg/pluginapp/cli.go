package pluginapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/hashicorp/go-hclog"
	"google.golang.org/protobuf/encoding/protojson"
)

func Main(data []byte, version, name string, anime bool, f Factory) {
	m, e := manifest.LoadWithChecksum(data, version)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if len(os.Args) == 2 && os.Args[1] == "manifest" {
		b, e := (protojson.MarshalOptions{UseProtoNames: true, Indent: "  "}).Marshal(m)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		fmt.Println(string(b))
		return
	}
	s := New(m, name, anime, f)
	// Silo copies plugin stderr into its log and reads hclog JSON lines at
	// their level; other lines become debug messages.
	level := hclog.LevelFromString(os.Getenv("SILO_THEME_LOG_LEVEL"))
	if level == hclog.NoLevel {
		level = hclog.Info
	}
	s.log = hclog.New(&hclog.LoggerOptions{Name: m.GetPluginId(), Level: level, Output: os.Stderr, JSONFormat: len(os.Args) == 1})
	if len(os.Args) > 1 {
		if len(os.Args) != 3 || (os.Args[1] != "preview" && os.Args[1] != "sync" && os.Args[1] != "reconcile") {
			fmt.Fprintln(os.Stderr, "usage: plugin [manifest | preview CONFIG.json | sync CONFIG.json | reconcile CONFIG.json]")
			os.Exit(2)
		}
		data, e := os.ReadFile(os.Args[2])
		if e != nil {
			fmt.Fprintln(os.Stderr, "cannot read configuration file")
			os.Exit(1)
		}
		c := Defaults()
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if e = d.Decode(&c); e != nil {
			fmt.Fprintln(os.Stderr, "invalid configuration JSON")
			os.Exit(1)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if e = s.ConfigureLocal(ctx, c); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		r := s.Execute(ctx, os.Args[1])
		if e := s.Close(); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		if e := json.NewEncoder(os.Stdout).Encode(r); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		if r.State == "prerequisite_failed" || r.State == "cancelled" || r.State == "needs_attention" || r.State == "reconcile_required" || r.State == "needs_review" {
			os.Exit(1)
		}
		return
	}
	sdkruntime.Serve(sdkruntime.ServeConfig{Logger: s.log, Servers: sdkruntime.CapabilityServers{Runtime: s, ScheduledTask: s, ScanSource: s, HttpRoutes: s}})
}
