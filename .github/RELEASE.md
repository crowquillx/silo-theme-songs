Theme Songs v0.3.3

Removed and private YouTube videos no longer pause downloads for the valid themes after them.

- Theme Songs paused YouTube downloads for 10 minutes after three videos in a row failed, to wait out a broken yt-dlp. It counted removed and private videos too, and ThemerrDB still links some of them, so on large libraries valid themes were skipped as `provider circuit open: extractor_broken`. Videos that YouTube reports as removed, private, blocked in your country or for channel members only are now reported as `YouTube video is unavailable` and don't count. Neither does a video reported unavailable without a reason, but it starts an early yt-dlp update check, since an outdated yt-dlp can say that about a video that plays. yt-dlp's usual "Video unavailable" message was not recognized before and showed up as `provider extractor: unavailable_media`. Thanks to @ArmoiraglatZ (#13).
- Downloads skipped during a pause are listed as `deferred` instead of failed, and the run still needs attention.

Nothing needs to be changed after upgrading. Failed and skipped downloads are not saved, so the next full run tries them again. For a run right away, choose **Download now** on the Theme Songs page.

Linux amd64 and arm64 binaries and checksums are attached.
