runlite-gui 0.1.0 (windows/amd64)

This is a client of the same daemon as the CLI. Double-clicking runlite-gui.exe
starts the daemon for you if it is not already running (same path as
unlite daemon start), then connects - so no terminal is needed first.
That needs runlite.exe sitting next to this file (shipped in this zip), or
runlite on PATH.

"daemon not running" and "project won't start" stay two different things: the
former is a red screen with a Retry button, the latter is a project row in
failed state. The GUI only auto-starts the daemon; it never starts your
projects.

Needs the Visual C++ runtime that Go links against for OpenGL; on a machine
without it the exe exits immediately. That is why it is a separate zip.