Theme Songs v0.1.15

Fixes the plugin failing at startup with `set state_dir to a persistent directory` on recent Silo builds. Silo now starts plugins without `SILO_PLUGIN_CACHE_DIR`, which the plugin used to find its default state directory. When that variable is unset, the plugin now reads Silo's plugin cache directory from the path Silo installed it at, so the default stays `plugin-state/dev.crowquillx.theme-songs` there. A state directory set in the settings still takes precedence.

Linux amd64 and arm64 binaries and checksums are attached.
