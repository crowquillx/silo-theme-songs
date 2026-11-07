Theme Songs v0.2.1

Anime libraries built by the [ShokoAnime VFS](https://github.com/crowquillx/silo-shoko-plugin) plugin now get season themes from AnimeThemes.

- Each file in a Shoko VFS library names its AniDB anime as `[AniDB Anime=<id>]`. Theme Songs reads it to find each season's anime exactly, without TVDB or TMDB IDs and without the AniBridge or Anime-Lists mappings, so those seasons are themed even when the mapping download is unavailable.
- Season 00 is left out, because specials can come from several anime.
- Other libraries are unchanged.

Linux amd64 and arm64 binaries and checksums are attached.
