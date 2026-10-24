# Silo Theme Songs

Downloads movie and series themes from ThemerrDB or selected URLs and saves them as MP3 in `theme-music` folders. Existing themes are preserved.

Part of [Crowquillx Silo Plugins](https://github.com/crowquillx/crowquillx-silo-plugins#install). Supports Linux amd64 and arm64.

## Install

1. Add the [Crowquillx catalog](https://github.com/crowquillx/crowquillx-silo-plugins#install) in **Administration → Plugins → Catalog** and install **Theme Songs**.
2. In **Installed**, open the plugin's **Configure** button or settings gear, then **Global Configuration**. Set the Silo URL, an administrator API key, primary profile ID, selected libraries, persistent state directory and writable media roots. Use [config.example.json](config.example.json) as a reference.
3. FFmpeg and ffprobe default to Silo's Docker paths, `/usr/lib/jellyfin-ffmpeg/ffmpeg` and `/usr/lib/jellyfin-ffmpeg/ffprobe`. When those files are missing, the plugin uses `ffmpeg` and `ffprobe` from PATH. To use other copies, set **ffprobe executable** and the `ffmpeg` entry in **Provider settings (JSON)**.
4. Select **Save config**, then run **Preview**. Tasks that take longer than Silo's 10-second task call continue in the background; check the plugin's status page for the result.
5. Enable the plugin's **themes** Autoscan source with poll delivery and no path rewrites. Allow two polls, then disable **Preview only** and run **Download**. For manual scans, enable **Manual library scans** instead and run **Reconcile** after scanning.

**Provider settings (JSON)** contains the provider object only; `ffprobe` has its own field. Conversion requires FFmpeg 4.4+ with `libmp3lame` and ffprobe. YouTube sources also require yt-dlp 2025.11.12+, its EJS scripts, and Deno 2.3+ or Node 22+; see [yt-dlp setup](https://github.com/yt-dlp/yt-dlp/wiki/EJS). Their paths use `yt_dlp` and `js_runtime` in Provider settings.

Use dedicated movie or series folders. Media files may be symlinks, as in debrid or rclone libraries, but other symlinks inside a media folder are refused. Keep the state directory across upgrades. If AnimeThemes manages a library, add its ID to this plugin's `anime_library_ids` setting.

## Build

Requires Go 1.26 or newer. FFmpeg and ffprobe are needed for the audio conversion tests.

```sh
make test
make build
```

The binary is `bin/plugin`.

## Acknowledgments

- [ThemerrDB](https://github.com/LizardByte/ThemerrDB) for theme sources.
- [yt-dlp](https://github.com/yt-dlp/yt-dlp) and [FFmpeg](https://ffmpeg.org/) for audio extraction and conversion.
- [Silo plugin SDK](https://github.com/Silo-Server/silo-plugin-sdk) for Silo integration.

[MIT license](LICENSE). Dependency licenses are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
