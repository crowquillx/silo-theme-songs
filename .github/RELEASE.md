Theme Songs v0.2.3

- The Theme Songs page follows Silo's theme, and your browser's light or dark setting when Silo sends none. Before, the page was always light, and **Download now** and **Save libraries** dropped the theme.
- The plugins page shows **Needs setup** for Theme Songs until you save an API key. Before, the settings were marked optional, so the plugin looked ready without one.
- The README, the setup notes and the Theme Songs page now say that the daily run needs the plugin to stay running. Silo stops the plugin when you save its settings, until the next library scan, page visit or task run. For a run every day, add a **Daily** trigger to **Download Theme Songs** in **Scheduled Tasks**. Before, they said it runs once a day on its own.
- Automatic runs start with the first task, library scan or page request, not when the plugin receives its settings, so a temporary plugin process that Silo starts only to check unsaved settings cannot begin a download run.
- The setup notes explain that "Connection checks are not supported for this plugin yet." after **Check Connection** is not an error on Silo versions without plugin settings checks.

Linux amd64 and arm64 binaries and checksums are attached.
