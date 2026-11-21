Theme Songs v0.3.0

Themes now play at the same volume, and existing themes can be brought in line with the current settings.

- **Theme volume** (in **Downloads**) levels every new download to one loudness, so a quiet film score and a loud anime opening play at about the same volume. The default is **Balanced (−16 LUFS)**, the level Apple Music and most podcasts use. The other choices run from **Very quiet (−23 LUFS)**, about the level of European TV broadcasts, to **Very loud (−11 LUFS)**, Spotify's Loud setting. **Original** keeps each source as released. Each theme gets one fixed gain, so its dynamics stay intact. A theme that can't get that loud without clipping stays a little quieter.
- YouTube audio is now encoded to MP3 once instead of twice.
- **Re-download themes that don't match these settings** (off by default) replaces themes Theme Songs downloaded when their volume or source no longer matches the settings. It also removes anime themes the settings no longer select, such as endings after **Anime endings** is turned off. It works like the Jellyfin AnimeThemes plugin's **Enforce conformity** option. The old theme stays until its replacement is saved. Themes you added or edited are never touched. Turn on **Dry run** first to see each change listed as `would_download` and `would_remove`.

After upgrading, new downloads are leveled to −16 LUFS, and themes you already have are left as they are. To level those too, turn on **Re-download themes that don't match these settings**. Every theme from an earlier version is then downloaded again, so consider a **Downloads per run** limit for a large library. To keep downloads as they were, set **Theme volume** to **Original**.

Linux amd64 and arm64 binaries and checksums are attached.
