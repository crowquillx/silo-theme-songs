Theme Songs v0.2.4

- AnimeThemes lookups now use the AnimeThemes GraphQL API at `graphql.animethemes.moe`. The JSON:API at `api.animethemes.moe` has answered every request with HTTP 503 since 2026-11-10, so 0.2.3 and earlier downloaded no anime themes. They reported each anime season as unavailable with "AnimeThemes rate limit exceeds request budget". The JSON:API is no longer used, even as a fallback.
- Nothing needs to be cleared after upgrading. The next full run looks up the seasons that failed. For a run right away, choose **Download now** on the Theme Songs page.
- Audio still comes from `a.animethemes.moe`, so the default **audio_hosts** setting is unchanged.
- When AnimeThemes answers with a GraphQL error, the season is reported as unavailable and looked up again on the next run. The error is never cached as "no themes".

Linux amd64 and arm64 binaries and checksums are attached.
