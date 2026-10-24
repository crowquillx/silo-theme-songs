# Silo Theme Songs

Downloads movie and series themes from ThemerrDB or selected URLs and saves them as MP3 in `theme-music` folders. Existing themes are preserved.

Part of [Crowquillx Silo Plugins](https://github.com/crowquillx/crowquillx-silo-plugins#install). Supports Linux amd64 and arm64.

## Install

1. In **Administration → Plugins → Catalog**, add the [Crowquillx catalog](https://github.com/crowquillx/crowquillx-silo-plugins#install) and install **Theme Songs**.
2. Restart Silo. Silo adds plugin tasks to **Scheduled Tasks** only when it starts.
3. Create a key in **Administration → API Keys**.
4. In **Installed**, open the plugin's settings, then **Global Configuration**. Paste the key into **Silo administrator API key** and select **Save config**.
5. Run **Preview Theme Songs** in **Scheduled Tasks**, then open **Theme Songs** in the admin menu. The page lists your movie and series libraries with their IDs.
6. Enter the libraries to use in **Selected library IDs**, for example `["3", "4"]`, and save. Run Preview again and check the planned downloads.
7. Turn on **Manual library scans**, or enable the plugin's **themes** source in Autoscan with poll delivery and no path rewrites.
8. Turn off **Preview only** and run **Download Theme Songs**. With manual scans, run **Reconcile Theme Songs** after the next library scan.

Silo writes themes into your media folders, so they must be writable. Silo's Docker Compose file mounts media read-only. Remove `:ro` from the media volume and recreate the container.

The first Preview or Download that finds a YouTube source installs yt-dlp and Deno in the plugin's state directory. This uses about 190 MB and needs access to GitHub. The plugin checks the yt-dlp release signature and the Deno checksum, and it checks for a new yt-dlp release once a day.

### Optional settings

Leave these empty unless your setup differs from Silo's Docker image.

| Setting | Default |
| --- | --- |
| Silo base URL | Silo's internal API address |
| Primary profile ID | The API key owner's primary profile |
| Persistent state directory | `plugin-state/dev.crowquillx.theme-songs` in `SILO_PLUGIN_CACHE_DIR`, which is `/var/lib/silo/plugins` in Docker. Required when that variable is unset. |
| Writable roots and path mappings | The selected libraries' folders |
| ffprobe executable | `/usr/lib/jellyfin-ffmpeg/ffprobe`, then `ffprobe` on PATH |
| Provider `ffmpeg` | `/usr/lib/jellyfin-ffmpeg/ffmpeg`, then `ffmpeg` on PATH |
| Provider `yt_dlp` and `js_runtime` | `yt-dlp`, Deno or Node 22+ on PATH, then the managed copies |

To use only your own yt-dlp and JavaScript runtime, set `"manage_tools": false` in **Provider settings (JSON)**. Use separate movie or series folders. Media files may be symlinks, but the plugin refuses other symlinks inside a media folder. If AnimeThemes manages a library, add its ID to **Anime-managed library IDs**.

## Build

Requires Go 1.26 or newer. FFmpeg and ffprobe are needed for the audio conversion tests.

```sh
make test
make build
```

The binary is `bin/plugin`.

## Acknowledgments

- [ThemerrDB](https://github.com/LizardByte/ThemerrDB) for theme sources.
- [yt-dlp](https://github.com/yt-dlp/yt-dlp), [Deno](https://deno.com/) and [FFmpeg](https://ffmpeg.org/) for audio extraction and conversion.
- [Silo plugin SDK](https://github.com/Silo-Server/silo-plugin-sdk) for Silo integration.

[MIT license](LICENSE). Dependency licenses are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
