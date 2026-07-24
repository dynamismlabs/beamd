# Changelog

All notable changes to beamd are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Changed

- **Transport (A1): tuned yamux stream window.** The yamux per-stream receive
  window now defaults to 4 MiB (up from the 256 KiB library default), lifting the
  `256 KiB / RTT` ceiling on solo transfers. It is set process-wide via the
  `BEAMD_YAMUX_STREAM_WINDOW_BYTES` environment variable (base-10 bytes,
  `262144`–`16777216`) on both the edge and the agent — there is no YAML,
  profile, or account setting. A present-but-invalid value is a fatal startup
  error, and the effective value is logged at edge and agent startup. Deploy the
  edge first, then `beamd reload` the agent. See
  [`docs/transport-performance-spec.md`](docs/transport-performance-spec.md)
  Part A.
