# Theme Songs

An unofficial Silo plugin that downloads movie and series theme audio. It uses exact TMDB matches in ThemerrDB, an explicit item URL, or an operator-selected direct audio template. It preserves existing themes and checks which catalog item owns each destination.

## Install

Add the [Crowquillx plugins catalog](https://raw.githubusercontent.com/crowquillx/crowquillx-silo-plugins/main/repository.json) under Silo Administration, Plugins, Catalog, Repositories. Install **Theme Songs** and open its settings. Linux amd64 and arm64 binaries are available on the [releases page](https://github.com/crowquillx/silo-theme-songs/releases).

For a local installation, upload the matching `plugin-linux-amd64` or `plugin-linux-arm64` binary through Silo's plugin upload control. The `.tar.gz` download bundles the binary, manifest, checksums, license notices and configuration guide for manual deployment. Upload the raw binary, not the tarball.

The Silo process must have writable media mounts and persistent plugin state. Provide the executable paths for `ffprobe`, `yt-dlp` and FFmpeg. Silo's standard Docker image already includes FFmpeg and ffprobe; see [Update provider settings](#update-provider-settings) for their paths. Install yt-dlp separately for YouTube sources. YouTube extraction also needs Deno 2.3+ or Node 22+ and the matching `yt-dlp-ejs` scripts. Use a bundled yt-dlp release or install `yt-dlp[default]` in a virtual environment; see the [yt-dlp runtime setup guide](https://github.com/yt-dlp/yt-dlp/wiki/EJS). The plugin supports yt-dlp 2025.11.12 or later; validation used 2026.08.19. Direct MP3 URLs need only `ffprobe`. Other direct audio formats also need FFmpeg 4.4+ with the `libmp3lame` encoder; set `provider.ffmpeg` to its executable path if it is not on PATH. The plugin does not install or update tools, use browser cookies, or require the AnimeThemes plugin.

## Configure

Use [config.example.json](config.example.json) as the reference for the `settings` configuration entry. The Silo form includes JSON fields for arrays, destination policy and provider settings.

1. Enter the Silo HTTP origin, an unscoped API key from a dedicated administrator account, and that account's primary profile ID. The API key field is a declared secret. It is never copied into the plugin's state database.
2. Select library IDs, a persistent state directory, allowed local media roots and any server-to-local path mappings. Map path components, not arbitrary text. Keep each mapping reversible. If no translations are configured, identical paths need no entry. When translations are configured, add an explicit identity mapping for every unchanged root.
3. Assign anime libraries to `anime_library_ids`, or use `exclude_items` for specific series managed by another downloader. Existing owner markers enforce the selected provider once a destination is claimed.
4. Keep `preview_only` enabled. Bind the Preview task and run it. Open the plugin's administrator status page to inspect sources, destinations and refusal reasons.
5. Enable Autoscan and add this plugin's `themes` source. Leave connection credentials empty, use poll delivery, and leave rewrites empty or identical on both sides. The plugin emits server paths. Use a polling interval of at least 60 seconds and allow two polls for its initial marker handshake.
6. Set `preview_only` to false and run the Download task. Bind it to a daily interval if desired. Silo manages task schedules; this plugin does not install a timer. If Silo asks for a restart after adding task bindings, restart it before testing the binding.

If Autoscan is unavailable, set `manual_refresh` to true. Run a library scan yourself after downloading, then run Reconcile. A downloaded file remains unconfirmed until the native theme set contains its unique filename-derived title under the expected owner.

## Update provider settings

1. Open **Administration → Plugins → Installed**. On **Theme Songs**, select the **Plugin settings** gear or **Configure** button.
2. Expand **Global Configuration** and find the plugin's settings form.
3. For Silo's standard Docker image, set **ffprobe executable** to `/usr/lib/jellyfin-ffmpeg/ffprobe`.
4. In **Provider settings (JSON)**, add or update the `"ffmpeg"` entry to `"/usr/lib/jellyfin-ffmpeg/ffmpeg"`. Edit the existing JSON object and preserve your other entries, including `yt_dlp`, `js_runtime`, `url_overrides` and `allowed_audio_hosts`.
5. Select **Save config**, wait for it to succeed, then reopen the form to confirm the saved values.
6. Run the plugin's **Preview** task and check its administrator status page for prerequisite failures before running **Download**.

If **Provider settings (JSON)** is empty, this is a valid minimal value:

```json
{
  "ffmpeg": "/usr/lib/jellyfin-ffmpeg/ffmpeg"
}
```

That field contains the provider object itself. Use `"ffmpeg"` as the key inside it; do not wrap it in another `"provider"` object or use `"provider.ffmpeg"` as a literal key. `ffprobe` belongs in its separate **ffprobe executable** field. JSON requires double quotes and no trailing commas or comments.

When editing the full `settings` JSON or a CLI configuration file, merge the same values at these locations while retaining the rest of your configuration:

```json
{
  "ffprobe": "/usr/lib/jellyfin-ffmpeg/ffprobe",
  "provider": {
    "ffmpeg": "/usr/lib/jellyfin-ffmpeg/ffmpeg"
  }
}
```

This is a configuration fragment, not a complete replacement for [config.example.json](config.example.json). Paths must exist inside the Silo container or whichever environment runs the plugin. Silo's standard Docker image includes these Jellyfin-packaged tools, but their directory may be absent from `PATH`. For other installations, use the actual installed paths. Each plugin saves its own settings, so repeat these steps for both plugins when both are installed.

## Provider options

| Field | Meaning |
| --- | --- |
| `types` | `series`, `movie`, or both. |
| `url_overrides` | Map Silo item IDs to selected YouTube videos or direct supported audio URLs. Overrides take precedence. |
| `direct_template` | Explicit alternative to ThemerrDB. Supports `{tmdbId}`, `{tvdbId}` and `{imdbId}`. Missing IDs refuse the lookup. |
| `allowed_audio_hosts` | Exact HTTPS hosts permitted for direct audio. Redirects must also remain allowed. |
| `yt_dlp`, `ffmpeg` | Operator-installed executable paths. `ffprobe` is a common setting. |
| `js_runtime` | `deno[:path]` or `node[:path]`, for example `node:/usr/bin/node`. When omitted, discovers a supported Deno or Node executable. Used only for YouTube extraction. |
| `max_bytes`, `timeout_seconds` | Input and output size limit and total download/conversion deadline, default 80 MiB per file and five minutes. |
| `assisted_search` | Optional YouTube candidate search when an exact ID or entry is absent. Defaults to false. Every result requires review. |
| `search_soundtrack` | Prefer soundtrack themes instead of opening/title music in assisted search. |
| `allow_covers`, `allow_instrumental` | Explicit search opt-ins. Both default to false. |

ThemerrDB uses `/tv_shows/themoviedb/<id>.json` and `/movies/themoviedb/<id>.json` and reads `youtube_theme_url`. A missing TMDB ID, absent entry, malformed response, transient failure and unavailable video are separate results. Failures never switch silently to another song. Repeated failures across different videos open a temporary extractor circuit breaker. Update the installed tools if they stop working.

Assisted search displays candidate links and reasons on the administrator page. Check the work, year, remake and track identity, then save the chosen URL in `provider.url_overrides` and rerun Preview. A channel name or view count is not identity proof. The plugin never downloads an unreviewed search result, and a transient curated lookup failure does not trigger search.

## Request pacing

YouTube search and downloads share one queue per plugin process, with a ten-second gap between jobs. yt-dlp waits three seconds between metadata requests and five to ten seconds before downloading. It uses one fragment worker and does not retry failed requests internally. A throttling or authentication response pauses the queue for one hour. This follows [yt-dlp’s pacing guidance](https://github.com/yt-dlp/yt-dlp/wiki/Extractors#common-youtube-errors); YouTube does not publish a guaranteed scraping quota.

ThemerrDB and direct audio requests start at most once per second per origin, including redirects. Silo catalog requests start at most twice per second per plugin process. HTTP 429 and 503 responses update a shared cooldown from `Retry-After`, accepting both seconds and HTTP dates without shortening the delay. Exhausted rate-limit reset headers also defer requests. Without a retry header, throttling pauses an origin for at least one minute; temporary failures use bounded backoff. Each request has a deadline and at most three attempts. Long waits defer work instead of sending an early retry.

HTTP cooldowns and YouTube pacing survive configuration changes and new clients within the same process. Restarting the plugin resets this in-memory state. Metadata caches avoid repeated successful and absent lookups. These are conservative plugin limits, not claimed service quotas.

## Ownership and files

Supported automatic layouts are a dedicated movie folder with one or more versions of the same catalog movie, and a dedicated series folder with flat episodes or conventional `Season NN` directories. One episode subdirectory inside a season is accepted. Separate copies stay separate. Unknown videos, shared folders, missing media, unclassified extras, category nesting and symlinks refuse automatic placement.

Destination overrides use `library_id/item_id/season/copy_root` keys and absolute server-path values. Use season `-1` for a series or movie destination. Overrides still pass inventory and on-disk ownership checks. They cannot make a mixed-season folder own one season's themes.

New downloads are saved as MP3 directly under `OWNER/theme-music/`. Non-MP3 audio is converted locally to 192 kbps MP3 at 44.1 kHz, preserving mono or stereo and downmixing larger channel layouts to stereo. Existing MP3 files pass through byte-for-byte; MP3 audio in another supported container is repackaged without re-encoding. Conversion stays within the job deadline and applies the size limit separately to input and output. Only validated audio-only output is published. Previously downloaded files keep their original format; upgrades do not replace or duplicate them.

Both plugins use protocol 1 sidecars named `.silo-theme-download.lock` and `.silo-theme-download-owner.json`. Keep them in place. One downloader installation per plugin is supported. Independent clustered writers and filesystems without reliable advisory locks are unsupported.

Publication uses a temporary file on the destination filesystem, an audio-only probe, a durable intent and an atomic no-clobber link. Root `theme.*` files and unowned audio are preserved. A checksum mismatch protects an edited managed file. Changed selections do not delete old audio. A new owner receives at most one theme before exact discovery succeeds.

The state directory contains download records, checksums and the scan journal. Keep it across upgrades. Autoscan acknowledgement means the host consumed a batch; it does not mean indexing finished. Failed discovery is retried with bounds. After deleting/recreating a source or changing `source_generation`, allow the new baseline handshake and run Reconcile to re-emit existing managed files. Never erase state to force an overwrite.

## Build and test

Requires Go 1.26.0 or later. Install FFmpeg with libmp3lame, libvorbis and libopus plus ffprobe for the real codec tests. Set `SILO_REQUIRE_FFMPEG=1` to fail instead of skipping when tools are absent. The SDK is pinned to v0.17.0. The API contract pin is in [docs/compatibility.json](docs/compatibility.json).

```sh
go test -race ./...
go vet ./...
make build
bin/plugin manifest
bin/plugin preview /path/to/private-config.json
make build-all
```

`sync` and `reconcile` also accept a private configuration file. Keep real credentials outside the repository. Both plugins share the Go packages in this repository, compiled into each independent binary. No second running plugin or shared service is required.

Releases contain per-platform manifests, SHA-256 checksums, source and dependency license notices, and `repository.json`. The catalog consumes that release index. CI uses controlled HTTP and subprocess fixtures; live provider availability is not a deterministic CI dependency.

## Sources and license

MIT for this plugin. See [LICENSE](LICENSE) and [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). It is independent of the Silo Server project.

Metadata comes from [ThemerrDB](https://github.com/LizardByte/ThemerrDB). Extraction uses separately installed [yt-dlp](https://github.com/yt-dlp/yt-dlp) and [FFmpeg](https://ffmpeg.org/). Those tools and downloaded media are not bundled with the plugin. Use sources you are authorized to download.

See [validation results](docs/validation.md) for live checks and provider availability limits.
