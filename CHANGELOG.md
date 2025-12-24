# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [0.1.0] - 2024-12-24

### Added

- **Tabbed detail panel**: The job detail panel now has "Details" and "Logs" tabs
  - Press `Tab` to switch between Details and Logs views
  - Press `l` to jump directly to Logs tab
  - Active tab is shown in bold in the header
- **Environment variables display**: Job details now show environment variables
  extracted from `export VAR=value && ` command prefixes
- **Mouse support**: Click on jobs in the list to select them
- **`remote-jobs status` command**: Re-enabled as a top-level command (synonym for
  `job status`)

### Changed

- **Cleaner command display**: Commands are now displayed without `export VAR=... && `
  prefixes for cleaner output (environment variables shown separately in details)
- **Consistent truncation**: Job list now uses consistent `…` character for truncation
  instead of mixed `...` styles
- **.gitignore**: Added `.gocache/` to ignore Go build cache

### Fixed

- Job list truncation now only adds ellipsis at the end, not both ends
