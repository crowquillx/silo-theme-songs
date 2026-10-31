# Silo Theme Songs

Downloads theme songs for movies and series and saves them as MP3 in `theme-music` folders. Movies and most series get their theme from ThemerrDB. Anime series get their openings and endings from AnimeThemes, one set per season. Existing themes are never replaced.

Part of [Crowquillx Silo Plugins](https://github.com/crowquillx/crowquillx-silo-plugins#install). Supports Linux amd64 and arm64.

## Install

1. In **Administration → Plugins → Catalog**, add the [Crowquillx catalog](https://github.com/crowquillx/crowquillx-silo-plugins#install) and install **Theme Songs**.
2. Create a key in **Administration → API Keys**, paste it into the plugin's **Silo API key** setting and save.
3. Open **Theme Songs** in the admin menu. The first run starts shortly after, and the page lets you choose libraries.

That's all. Theme Songs uses every movie and series library and runs on its own: once a day, after Silo scans a library with new items, and when Silo starts. After it saves a theme, it asks Silo to scan that folder, so the theme plays right away.

Silo writes themes into your media folders, so they must be writable. Silo's Docker Compose file mounts media read-only. Remove `:ro` from the media volume and recreate the container. The Theme Songs page and every run say which library is read-only. Like Jellyfin's theme plugins, Theme Songs leaves the permissions of new themes and `theme-music` folders to Silo's umask, so with the usual umask of `022` other programs that read your media can read them.

The first theme that comes from YouTube installs yt-dlp and Deno in the plugin's state directory. This uses about 190 MB and needs access to GitHub. The plugin checks the yt-dlp release signature and the Deno checksum, and it checks for a new yt-dlp release once a day.

### Upgrading from 0.1 or the AnimeThemes plugin

AnimeThemes was a separate plugin before 0.2.0. Install Theme Songs 0.2.0 or later, then uninstall the AnimeThemes plugin. Until you do, Theme Songs leaves anime libraries to it. The first run afterwards copies the AnimeThemes plugin's state from its default state directory, so its themes stay managed. Settings from the AnimeThemes plugin are not copied: re-enter any manual mappings or overrides under **AnimeThemes settings (JSON)**.

The Preview, Reconcile and Autoscan steps are gone. Every run now checks the themes it downloaded, and Silo scans each new theme when the plugin asks. You can delete the plugin's Autoscan source. Settings saved by 0.1 keep working: **Preview only** still applies until you save the settings form, which then shows **Dry run** instead, and **Anime-managed library IDs** became **AnimeThemes libraries**.

### Settings

Only the API key is required. The settings form groups the rest:

- **Libraries.** **Skip these libraries** leaves libraries out; every other movie and series library is used, including new ones. **Only these libraries** limits runs to a list instead. The Theme Songs page has checkboxes that fill in **Skip these libraries** for you.
- **Downloads.** **Dry run** lists what would be downloaded without saving files. **Use AnimeThemes for anime** turns anime season themes on or off, with switches for openings, endings, spoiler themes and NSFW themes. **Suggest YouTube matches** searches YouTube when ThemerrDB has no theme and lists candidates on the Theme Songs page; nothing downloads until you save a URL for the item.
- **Advanced.** Leave these empty unless your setup differs from Silo's Docker image.

| Advanced setting | Default |
| --- | --- |
| AnimeThemes libraries | Every series library is checked for anime |
| Skip these items | None |
| Downloads per run | No limit |
| Install yt-dlp and Deno | On. Off uses `yt-dlp` and Deno or Node 22+ from PATH |
| Silo URL | Silo's internal API address |
| Profile ID | The API key owner's primary profile |
| Persistent state directory | `plugin-state/dev.crowquillx.theme-songs` in Silo's plugin cache directory, which is `/var/lib/silo/plugins` in Docker |
| ffprobe path | `/usr/lib/jellyfin-ffmpeg/ffprobe`, then `ffprobe` on PATH |
| Writable roots and path mappings (JSON) | The selected libraries' folders |
| ThemerrDB and YouTube settings (JSON) | See below |
| AnimeThemes settings (JSON) | See below |

**ThemerrDB and YouTube settings** accept `url_overrides` (catalog item ID to a YouTube or direct audio URL), `direct_template`, `allowed_audio_hosts`, `yt_dlp`, `ffmpeg`, `js_runtime`, `max_bytes`, `timeout_seconds`, `types` (`movie`, `series`), `search_soundtrack`, `allow_covers` and `allow_instrumental`.

**AnimeThemes settings** accept `manual_mappings` (`"tvdb_show:<id>:s<season>"` to AniDB IDs; an empty list turns a season off), `series_fallback` (catalog series ID to AniDB IDs, for a theme in the series folder), `selection_overrides`, `all_distinct`, `allow_overlap`, `mapping_update_hours`, `anibridge_url`, `anime_lists_url`, `audio_hosts` and `ffmpeg`.

**Writable roots and path mappings** accept `allowed_roots`, `path_mappings` (`[{"server": "/media", "local": "/mnt/media"}]`), `single_season_flat_fallback` and `destination_overrides`.

Use separate movie or series folders, with `Season NN` or `Specials` folders inside series. Anime season themes go into those season folders. Media files may be symlinks, but the plugin refuses other symlinks inside a media folder.

### How Silo runs the plugin

- Silo adds plugin tasks to **Scheduled Tasks** only when it starts, so **Download Theme Songs** appears after the next restart. Theme Songs does not need it: it starts its own runs. Silo's task finishes after 10 seconds while the run continues, so the task's run time is wrong; the Theme Songs page shows progress and the result.
- Saving the plugin's settings restarts it and stops a run in progress. The next run starts shortly after.
- **Check Connection** on the settings page tests the API key, the libraries and whether Silo can write to them, on Silo versions that support plugin settings checks.
- Silo writes plugin logs to its container output, such as `docker logs silo`, not to its Logs page. Each run logs its start, its result, each download and one line per kind of problem. Set `SILO_THEME_LOG_LEVEL=warn` on the Silo container to log only problems.

## Build

Requires Go 1.26 or newer. FFmpeg and ffprobe are needed for the audio conversion tests.

```sh
make test
make build
```

The binary is `bin/plugin`. `bin/plugin preview CONFIG.json` and `bin/plugin sync CONFIG.json` run once from the command line with settings like [config.example.json](config.example.json).

## Acknowledgments

- [ThemerrDB](https://github.com/LizardByte/ThemerrDB) for theme sources.
- [AnimeThemes](https://animethemes.moe/) for anime theme metadata and audio.
- [AniBridge](https://github.com/anibridge/anibridge-mappings) and [Anime-Lists](https://github.com/Anime-Lists/anime-lists) for anime mappings.
- [yt-dlp](https://github.com/yt-dlp/yt-dlp), [Deno](https://deno.com/) and [FFmpeg](https://ffmpeg.org/) for audio extraction and conversion.
- [Silo plugin SDK](https://github.com/Silo-Server/silo-plugin-sdk) for Silo integration.

[MIT license](LICENSE). Dependency licenses are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
