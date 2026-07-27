# Changelog

All notable changes to beamd are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- **Transport (Part B, default-off): QUIC tunnel path with tuned TCP fallback.**
  The edge can listen on `443/udp`, the agent supports `auto|quic|tcp`,
  diagnostics report the selected transport/fallback reason, and either
  `BEAMD_DISABLE_QUIC=true` or `BEAMD_TRANSPORT=tcp` restores the old path.
  QUIC remains opt-in until the B4 synthetic gates and production pilot pass.

### Changed

- **Tunnel liveness diagnostics:** application heartbeat expiry is now
  classified as `idle` instead of `protocol`, and suspend/resume recovery is
  covered over both TCP and QUIC.
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
