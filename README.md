# Theme Songs

An unofficial Silo plugin that downloads movie and series theme audio. It uses exact TMDB matches in ThemerrDB, an explicit item URL, or an operator-selected direct audio template. It preserves existing themes and checks which catalog item owns each destination.

## Install

Add the [Crowquillx plugins catalog](https://raw.githubusercontent.com/crowquillx/crowquillx-silo-plugins/main/repository.json) under Silo Administration, Plugins, Catalog, Repositories. Install **Theme Songs** and open its settings. Linux amd64 and arm64 binaries are available on the [releases page](https://github.com/crowquillx/silo-theme-songs/releases).

For a local installation, upload the matching `plugin-linux-amd64` or `plugin-linux-arm64` binary through Silo's plugin upload control. The `.tar.gz` download bundles the binary, manifest, checksums, license notices and configuration guide for manual deployment. Upload the raw binary, not the tarball.

The Silo process must have writable media mounts and persistent plugin state. Install `ffprobe`, `yt-dlp`, and FFmpeg yourself and provide their executable paths. Direct audio URLs need only `ffprobe`. The plugin does not install or update tools, use browser cookies, or require the AnimeThemes plugin.

## Configure

Use [config.example.json](config.example.json) as the reference for the `settings` configuration entry. The Silo form includes JSON fields for arrays, destination policy and provider settings.

1. Enter the Silo HTTP origin, an unscoped API key from a dedicated administrator account, and that account's primary profile ID. The API key field is a declared secret. It is never copied into the plugin's state database.
2. Select library IDs, a persistent state directory, allowed local media roots and any server-to-local path mappings. Map path components, not arbitrary text. Keep each mapping reversible. If no translations are configured, identical paths need no entry. When translations are configured, add an explicit identity mapping for every unchanged root.
3. Assign anime libraries to `anime_library_ids`, or use `exclude_items` for specific series managed by another downloader. Existing owner markers enforce the selected provider once a destination is claimed.
4. Keep `preview_only` enabled. Bind the Preview task and run it. Open the plugin's administrator status page to inspect sources, destinations and refusal reasons.
5. Enable Autoscan and add this plugin's `themes` source. Leave connection credentials empty, use poll delivery, and leave rewrites empty or identical on both sides. The plugin emits server paths. Use a polling interval of at least 60 seconds and allow two polls for its initial marker handshake.
6. Set `preview_only` to false and run the Download task. Bind it to a daily interval if desired. Silo manages task schedules; this plugin does not install a timer. If Silo asks for a restart after adding task bindings, restart it before testing the binding.

If Autoscan is unavailable, set `manual_refresh` to true. Run a library scan yourself after downloading, then run Reconcile. A downloaded file remains unconfirmed until the native theme set contains its unique filename-derived title under the expected owner.

Provider settings:

| Field | Meaning |
| --- | --- |
| `types` | `series`, `movie`, or both. |
| `url_overrides` | Map Silo item IDs to selected YouTube videos or direct supported audio URLs. Overrides take precedence. |
| `direct_template` | Explicit alternative to ThemerrDB. Supports `{tmdbId}`, `{tvdbId}` and `{imdbId}`. Missing IDs refuse the lookup. |
| `allowed_audio_hosts` | Exact HTTPS hosts permitted for direct audio. Redirects must also remain allowed. |
| `yt_dlp`, `ffmpeg` | Operator-installed executable paths. `ffprobe` is a common setting. |
| `max_bytes`, `timeout_seconds` | Download limits, default 80 MiB and five minutes. |
| `assisted_search` | Optional YouTube candidate search when an exact ID or entry is absent. Defaults to false. Every result requires review. |
| `search_soundtrack` | Prefer soundtrack themes instead of opening/title music in assisted search. |
| `allow_covers`, `allow_instrumental` | Explicit search opt-ins. Both default to false. |

ThemerrDB uses `/tv_shows/themoviedb/<id>.json` and `/movies/themoviedb/<id>.json` and reads `youtube_theme_url`. A missing TMDB ID, absent entry, malformed response, transient failure and unavailable video are separate results. Failures never switch silently to another song. Repeated failures across different videos open a temporary extractor circuit breaker. Update the installed tools if they stop working.

Assisted search displays candidate links and reasons on the administrator page. Check the work, year, remake and track identity, then save the chosen URL in `provider.url_overrides` and rerun Preview. A channel name or view count is not identity proof. The plugin never downloads an unreviewed search result, and a transient curated lookup failure does not trigger search.

## Ownership and files

Supported automatic layouts are a dedicated movie folder with one or more versions of the same catalog movie, and a dedicated series folder with flat episodes or conventional `Season NN` directories. One episode subdirectory inside a season is accepted. Separate copies stay separate. Unknown videos, shared folders, missing media, unclassified extras, category nesting and symlinks refuse automatic placement.

Destination overrides use `library_id/item_id/season/copy_root` keys and absolute server-path values. Use season `-1` for a series or movie destination. Overrides still pass inventory and on-disk ownership checks. They cannot make a mixed-season folder own one season's themes.

Files live directly under `OWNER/theme-music/`. Both plugins use protocol 1 sidecars named `.silo-theme-download.lock` and `.silo-theme-download-owner.json`. Keep them in place. One downloader installation per plugin is supported. Independent clustered writers and filesystems without reliable advisory locks are unsupported.

Publication uses a temporary file on the destination filesystem, an audio-only probe, a durable intent and an atomic no-clobber link. Root `theme.*` files and unowned audio are preserved. A checksum mismatch protects an edited managed file. Changed selections do not delete old audio. A new owner receives at most one theme before exact discovery succeeds.

The state directory contains download records, checksums and the scan journal. Keep it across upgrades. Autoscan acknowledgement means the host consumed a batch; it does not mean indexing finished. Failed discovery is retried with bounds. After deleting/recreating a source or changing `source_generation`, allow the new baseline handshake and run Reconcile to re-emit existing managed files. Never erase state to force an overwrite.

## Build and test

Requires Go 1.26.0 or later. The SDK is pinned to v0.17.0. The API contract pin is in [docs/compatibility.json](docs/compatibility.json).

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
