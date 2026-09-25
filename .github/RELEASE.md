Theme Songs v0.1.5 for Silo, Linux amd64 and arm64.

New downloads are saved as MP3. Direct Ogg, Opus, FLAC, WAV, AAC and M4A/M4B sources are converted locally with FFmpeg before validation and publication. Existing MP3 files remain byte-identical; MP3 audio in another supported container is copied without re-encoding. Previously downloaded themes remain untouched and are not duplicated.

Install FFmpeg 4.4+ with libmp3lame and ffprobe in the plugin environment. Set `provider.ffmpeg` if FFmpeg is not on PATH. Preview reports a missing converter before downloading a source that needs it. Conversion uses the same job deadline, bounds output size, and cleans staging on failure or cancellation. Existing request pacing, cooldowns and file protections are retained.

Install through the Crowquillx plugin catalog or upload the matching raw binary. Tarballs include the manifest, configuration example and license notices. Verify downloads with `checksums.txt`.
