# Production stream-capacity finding

Status: 128-per-session / 256-global rollout selected; bounded waiting remains
a separate follow-up from the 2026-09-01 hosted production incident.

## What happened

On `flow-trey.beamd.run`, the hosted edge returned 3,566 HTTP 503 responses in
the 24 hours ending around 2026-09-01 19:12 UTC. Every response had Beamd's
fixed 36-byte `{"error":"tunnel capacity reached"}` body; 3,564 were emitted
in 0-1 ms, before the request could have reached the tunneled application.
Hourly rejection rates reached 15-18%.

The application generated synchronized fan-out bursts while other requests
were still open. The largest observed one-second interval contained 116 new
arrivals, including 81 immediate capacity rejections. At two sampled rejection
times, completed-event telemetry showed 59 accepted requests still in flight;
ongoing long-lived streams are not fully visible until their next heartbeat or
final event.

This was a Beamd admission-limit failure triggered by a real application
traffic shape, not a QUIC transport failure and not an application-generated
503. TCP and QUIC use the same stream admission limits.

## Capacity was logical, not physical

The deployed defaults were:

- 64 concurrent data streams per authenticated agent session;
- 128 concurrent data streams across the edge process;
- 8 authenticated agent sessions.

An agent session may own more than one tunnel name, so 64 is per agent session,
not necessarily per application hostname. A WebSocket, SSE response, or other
long-running request occupies a stream until it closes.

During the incident the 2-vCPU, 4-GiB edge showed no host exhaustion:

- CPU averaged 99.25% idle; the busiest ten-minute sample was 98.14% idle;
- approximately 3.5 GiB of memory remained available;
- Beamd's process RSS high-water mark was about 35 MiB and its systemd unit's
  recorded memory peak was about 146 MiB;
- there were no Beamd restarts, OOM kills, kernel stalls, or meaningful system
  load.

The current admission path is non-blocking: if either stream semaphore is full,
Beamd immediately returns 503. It does not wait for a request already in flight
to finish.

## Primary capacity change

The selected hosted target is 128 concurrent streams per current agent session
and 256 across the edge. That covers the largest observed 116-arrival burst
without making a single session unbounded. At the default 4-MiB yamux receive
window, the global flow-control exposure rises from 512 MiB to 1 GiB. The
production 4-GiB host had approximately 3.5 GiB available during the incident,
so this is a measured increase with room, not a claim that capacity is free.

The rollout is backward compatible. Current agents advertise a 128-stream
handler ceiling in the optional `hello.max_streams` field. The edge uses the
smaller of its configured ceiling and the advertised value. Older agents omit
the field and remain limited to 64, preventing an upgraded edge from sending
them more concurrent streams than they can accept. An edge log records the
negotiated `stream_capacity` when each session opens.

Qualification must still cover the `flow-trey` burst shape, large request and
response bodies, mixed-session fairness, capacity rejections, active streams,
RSS, and request latency. A future per-session or per-tier reservation may be
needed so one tenant cannot monopolize the 256 global slots.

## Bounded waiting is secondary resilience, not the capacity fix

Add a small, bounded wait for a stream slot so synchronized bursts do not fail
at the first instant the ceiling is occupied. This is standard backpressure,
but it must not be used to disguise sustained under-capacity.

Required properties:

- wait only for a short configured deadline and honor visitor cancellation,
  session closure, and edge shutdown;
- bound the number of queued requests per session and globally;
- preserve cross-session fairness rather than allowing one tenant to fill the
  queue;
- avoid unbounded request-body buffering while waiting;
- emit queue-depth, wait-duration, timeout, and capacity-rejection metrics;
- return a clear 503 with `Retry-After` when the queue is full or the deadline
  expires.

The expected sequence is: raise the justified per-session capacity, add bounded
waiting to absorb brief fan-out bursts, then use telemetry and stress tests to
decide whether the global ceiling or application request fan-out also needs to
change.
