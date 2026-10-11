<div align="center">

<img src="assets/banner.svg" alt="cadence" width="100%">

**A standalone audio node for Discord bots, built in Go, with frame-accurate timing and two-deck mixing.**

[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Discord Voice](https://img.shields.io/badge/voice%20gateway-v8-5865F2?logo=discord&logoColor=white)](https://docs.discord.com/developers/topics/voice-connections)
[![DAVE](https://img.shields.io/badge/E2EE-DAVE-2ea44f)](https://daveprotocol.com)
[![Encryption](https://img.shields.io/badge/transport-AES--256--GCM%20%7C%20XChaCha20-informational)](#discord-voice)
[![Sources](https://img.shields.io/badge/sources-Deezer%20%C2%B7%20SoundCloud%20%C2%B7%20Spotify%20%C2%B7%20Apple%20Music-ff69b4)](#sources)
[![Status](https://img.shields.io/badge/status-production-brightgreen)](#status)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

[Why](#why-cadence) · [Features](#features) · [Quick start](#quick-start) · [Configuration](#configuration) · [API](#api) · [Timing](#the-wire-clock) · [Mixing](#mixing) · [Architecture](#architecture)

</div>

---

Cadence is the audio node behind **greed**. It joins Discord voice channels, streams music, mixes tracks DJ-style, and tells your bot **exactly** when each sound reached Discord, so synced lyrics stay synced. If you've used [Lavalink](https://github.com/lavalink-devs/Lavalink), the model will feel familiar: your bot does the thinking, Cadence does the audio, and they talk over REST and a WebSocket.

## Why Cadence?

We ran Lavalink, and then our own fork of it, in production for a long time. Cadence exists because of what that taught us:

| Problem we kept hitting | What it cost | How Cadence handles it |
| --- | --- | --- |
| **Timing was measured in the wrong place.** Lavalink's position is where the *decoder* is, about 400 ms ahead of the network. `TrackStart` fires before the stream is even fetched. Updates arrive every 5 s, and position ignores speed filters. | Live lyrics drifted by an unpredictable amount on every song | A [wire clock](#the-wire-clock): every position is stamped when the packet leaves the socket, plus a dedicated `TrackAudibleEvent` |
| **One track per player.** Crossfading isn't possible ([lavalink#1093](https://github.com/lavalink-devs/Lavalink/issues/1093)), so we forked the JVM node | A fork patched into upstream seams, with bugs at every seam | [Two decks](#mixing) built in from the start: armed crossfades, bass swap, filter sweeps, tempo matching |
| **The voice connection disposed an encoder it didn't own.** The fork handed each player's long-lived mixer to Koe as the connection's sender, and Koe disposes its sender on re-assignment and on close, freeing the mixer's Opus encoder mid-playback | Silent audio, twice ("changing the region fixes it"), fleet-wide after a restart | Each voice connection **owns** its encoder, sender and DAVE session, and they die together |
| **An unbounded analysis queue** | Out-of-memory crash that broke YouTube playback for 16 days | Every queue, cache and backlog is bounded and observable |
| **Silent failures.** Koe drops frames inside a `catch` with no log | Hours of debugging with no trail | Every dropped frame, stall and failure becomes an event, a log line and a metric |
| **DAVE send stalls** of 100–400 ms about once a second ([lavalink#1222](https://github.com/lavalink-devs/Lavalink/issues/1222)) | Audible dropouts on a node that looked idle | A paced, OS-thread-locked send loop that reports any lateness as a `SendStallEvent` |
| **JVM footprint**: a 12 GB heap for a music node | A big box per region | A single static Go binary |
| **Fixed encoder bitrate.** Lavalink's binding has no bitrate setter | Boosted channels couldn't use their extra bandwidth | Per-connection bitrate (16–384 kbps) set from the channel |

## Features

- 🎧 **Discord voice done properly**: gateway v8, `seq_ack` resume, AES-256-GCM / XChaCha20 transport, and **DAVE end-to-end encryption** through Discord's own `libdave`
- ⏱️ **Wire clock**: frame-accurate positions measured at the socket, correct under every speed and tempo filter, with clock-sync and playout-delay support
- 🎚️ **Two-deck mixer**: pre-buffered deck B, beat-aligned armed crossfades, equal-power or linear curves, bass swap, high-pass sweeps, loudness matching, tempo ramps
- 🔍 **Smart sources**: Deezer playback with SoundCloud as a fallback, and Spotify and Apple Music links and search mirrored to the **exact recording** by ISRC
- 🧠 **Track analysis**: BPM, meter, downbeats, phrase grid, Camelot key, sections, vocals, intro and outro regions, and EBU R128 loudness, compatible with greed's automix fingerprints
- 🎛️ **Filters**: 15-band equalizer, timescale (speed, pitch, rate), karaoke, low-pass, tremolo, vibrato, rotation and **reverb**, all of which **glide** into a new setting instead of jumping
- 🔁 **Session resume**: players keep running while your bot reconnects, and missed events are replayed
- 📈 **Observability**: Prometheus `/metrics`, structured JSON logs, typed failures with severities
- 🛡️ **Safe defaults**: no default password, constant-time auth, per-source proxies with automatic direct fallback

## Quick start

### Docker (recommended)

```bash
cp .env.example .env
docker build -t cadence .
docker run --env-file .env -p 2333:2333 cadence
```

The image includes `libopus` and Discord's `libdave`, so voice works out of the box.

### From source

Production builds need **Linux, cgo, libopus and libdave**:

```bash
sh libdave_install.sh v1.1.0
export PKG_CONFIG_PATH=$HOME/.local/lib/pkgconfig
go build -tags native,nolibopusfile -o cadence ./cmd/cadence
CADENCE_PASSWORD=change-me ./cadence
```

Get `libdave_install.sh` from [godave](https://github.com/disgoorg/godave/tree/master/scripts).

A build without the `native` tag runs anywhere and passes every test, but it has no voice. Cadence refuses to start that way unless `CADENCE_ALLOW_NO_VOICE=true` is set, which is useful for developing search or analysis on Windows or macOS.

```bash
go test ./...
```

## Configuration

Everything is set through environment variables. See [`.env.example`](.env.example).

| Variable | Default | Description |
| --- | --- | --- |
| `CADENCE_PASSWORD` | **required** | Shared secret for every request. `youshallnotpass` is refused. |
| `CADENCE_ADDRESS` | `0.0.0.0:2333` | Listen address |
| `CADENCE_BUFFER_MS` | `10000` | Decoded audio buffered per deck (1 000–60 000) |
| `CADENCE_BITRATE` | `96000` | Default Opus bitrate when the bot doesn't send one |
| `CADENCE_PLAYOUT_DELAY_MS` | `0` | Measured delay from Discord receiving audio to listeners hearing it, added to `audibleAt` |
| `CADENCE_PAUSE_FADE_MS` | `0` | Fade out on pause and back in on resume (0–2000). `0` pauses instantly |
| `CADENCE_RESUME_TIMEOUT_SECONDS` | `300` | How long players survive a disconnected client (when resuming is enabled) |
| `CADENCE_UPDATE_INTERVAL_MS` | `1000` | `playerUpdate` cadence while playing (min 250) |
| `CADENCE_SEARCH_ORDER` | `applemusic,spotify,deezer` | What plain-text searches query, in order |
| `CADENCE_ANALYSIS_WORKERS` / `_QUEUE` | `2` / `32` | Offline analysis concurrency and queue bound |
| `CADENCE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `CADENCE_AUDIO_CACHE_DIR` | off | Directory for the disk audio cache. Unset disables it |
| `CADENCE_AUDIO_CACHE_GB` | `20` | Disk cache size cap (1–1000). Least recently played tracks are removed first |
| `CADENCE_NORMALIZE` | `true` | Level every track to the same loudness, from its analysis |
| `CADENCE_NORMALIZE_LUFS` | `-14` | Loudness target for levelling (−30 to −5) |
| `CADENCE_STATE_FILE` | off | Where to save sessions on shutdown so a restart resumes playback where it stopped |
| `DEEZER_ARL` | | Deezer account session for full-length streams. List several, comma-separated, and a rejected or refusing account is skipped for the next |
| `DEEZER_ARL_ROTATION` | `failover` | Which listed account each stream starts with: `failover` (first working one), `round-robin` or `random`. The others stay as fallbacks |
| `DEEZER_MASTER_KEY` | | 16-byte key for Deezer's `BF_CBC_STRIPE` stream cipher. Without it nothing plays |
| `DEEZER_HIGH_QUALITY` | `true` | Prefer 128 kbps MP3 over 64 kbps |
| `SPOTIFY_CLIENT_ID` / `_SECRET` | | Spotify Web API credentials (metadata only) |
| `APPLE_MUSIC_TOKEN` | scraped | Optional fixed developer token |
| `APPLE_MUSIC_STOREFRONT` | `us` | Catalog storefront |
| `SOUNDCLOUD_ENABLED` | `true` | Use SoundCloud when Deezer has no match |
| `SOUNDCLOUD_CLIENT_ID` | scraped | Optional fixed SoundCloud client id |
| `DEEZER_PROXY`, `SPOTIFY_PROXY`, `APPLE_MUSIC_PROXY`, `SOUNDCLOUD_PROXY` | | Per-source HTTP proxy; a failed proxy falls back to direct for 60 s |

## Sources

| Source | Search prefix | Links | Plays audio |
| --- | --- | --- | --- |
| **Deezer** | `dzsearch:` / `dzisrc:` | tracks, albums, playlists, artists, short links | ✅ |
| **Apple Music** | `amsearch:` | songs, albums, playlists, artists | Mirrored to Deezer |
| **Spotify** | `spsearch:` | tracks, albums, playlists, `spotify:` URIs, short links | Mirrored to Deezer |
| **SoundCloud** | `scsearch:` | tracks, sets, short links | ✅ fallback, used only when Deezer has no match |

**How mirroring works.** When a Spotify or Apple track plays, Cadence takes its **ISRC**, the unique ID of a recording, and looks up that exact recording on Deezer. If there's no ISRC match, it falls back to a scored text search. That search penalizes remixes, live versions, sped-up versions and anything else that differs from what was requested, and rejects candidates whose length is off by more than 5 seconds. Matches are cached for 14 days and concurrent lookups are de-duplicated, so a song is resolved once.

**Spotify playlists** load up to 1,000 tracks. Spotify only serves public, user-made playlists to third-party apps, so its own editorial and personalised playlists (Today's Top Hits, Discover Weekly, Daily Mix) and private playlists return "not found".

Encoded tracks are plain base64url JSON, so **any Cadence node can play any track**, no matter which node resolved it.

## API

All routes require `Authorization: <CADENCE_PASSWORD>`. The shape follows Lavalink v4 closely, under `/v1`.

**Drop-in Lavalink v4.** Cadence also speaks Lavalink's own v4 protocol (`/v4/websocket`, `/v4/loadtracks`, `/v4/sessions/…`, `/version`), so an existing bot using a Lavalink client such as `lavalink-client` can point at Cadence without code changes. On v4 connections only Lavalink's standard events are sent. To also receive Cadence's extras (`TrackAudibleEvent`, mix events, stalls), send `PATCH /v4/sessions/{id} {"cadenceEvents": true}` after checking that `/v4/info` reports `"wireClock": true`.

### WebSocket

`GET /v1/websocket` with headers `User-Id` (your bot's ID) and `Client-Name`. To resume a session, also send `Session-Id`.

```json
{ "op": "ready", "sessionId": "8e1f0c2a9b7d4e61", "resumed": false }
```

| Op | When |
| --- | --- |
| `ready` | Right after connecting |
| `playerUpdate` | Every `CADENCE_UPDATE_INTERVAL_MS` while playing, and immediately after every event |
| `event` | See [Events](#events) |

### REST

| Method | Route | Description |
| --- | --- | --- |
| `GET` | `/v1/info` | Version, sources, filters, and whether DAVE is available |
| `GET` | `/v1/stats` | Players, sessions, uptime, analysis queue |
| `GET` | `/v1/clock` | Node time, for clock sync |
| `GET` | `/metrics` | Prometheus exposition |
| `GET` | `/v1/loadtracks?identifier=` | Resolve a link, a prefixed search, an ISRC or plain text |
| `GET` | `/v1/decodetrack?encodedTrack=` | Decode one track |
| `POST` | `/v1/decodetracks` | Decode many tracks |
| `POST` | `/v1/prefetch` (also `/v4/prefetch`) | `{ "tracks": ["…"] }` (up to 10). Preload the start of upcoming tracks so they play instantly |
| `PATCH` | `/v1/sessions/{session}` | `{ "resuming": true, "timeout": 300 }` |
| `GET` | `/v1/sessions/{session}/players` | All players |
| `GET` / `PATCH` / `DELETE` | `/v1/sessions/{session}/players/{guild}` | Read, update or destroy a player |
| `POST` | `…/players/{guild}/mix/deckb` | Pre-load the incoming track |
| `POST` | `…/players/{guild}/mix/crossfade` | Start or arm a transition |
| `DELETE` | `…/players/{guild}/mix` | Cancel an armed transition |
| `GET` | `/v1/mix/analysis/{source}/{identifier}` | A finished fingerprint |
| `POST` | `/v1/mix/analyze` | Queue offline analysis of a track |

#### Updating a player

```json
PATCH /v1/sessions/{session}/players/{guild}?noReplace=false
{
  "track":   { "encoded": "eyJzb3VyY2UiOiJkZWV6ZXIi..." },
  "position": 0,
  "paused":   false,
  "volume":   100,
  "filters":  { "timescale": { "speed": 1.1, "pitch": 1, "rate": 1 } },
  "voice":    { "token": "…", "endpoint": "c-dfw09.discord.media:443", "sessionId": "…", "channelId": "…", "bitrate": 128000 },
  "analyze":  true
}
```

Send `"track": { "encoded": null }` to stop. Two more fields work on both `/v1` and `/v4`: `"bitrate": 128000` sets the Opus bitrate (16–384 kbps) and is remembered across voice updates, and `"nextTrack": { "encoded": "…" }` preloads the next track so it starts on the exact frame the current one ends (`{ "encoded": null }` clears it). The voice and track updates can arrive in **any order**. A track sent before voice simply waits, without its clock moving.

#### Filters

`filters` takes Lavalink's shapes (`equalizer`, `timescale`, `karaoke`, `lowPass`, `tremolo`, `vibrato`, `rotation`) plus one of Cadence's own:

```json
"filters": { "reverb": { "mix": 0.3, "room": 0.85, "damping": 0.4 } }
```

`reverb` is a Freeverb (8 combs and 4 all-passes per channel, tuned for 48 kHz); every value is 0–1. It is listed in `/v4/info` `filters`, so `lavalink-client` passes it through unchanged.

**Changes are smooth.** A new `filters` value never switches on the next frame. Equalizer gains, pitch and speed glide to their targets (about 0.5 s), and effects that turn on or off crossfade in and out, keeping their last settings while they fade. Nightcore sweeps up instead of snapping, and toggling reverb never clicks.

### Events

Every event carries `guildId`, the `track`, and an `instance` counter, so your bot can ignore events from a track it has already replaced.

| Event | Meaning |
| --- | --- |
| `TrackLoadingEvent` | Accepted, now resolving (mirror lookup, stream URL) |
| `TrackStartEvent` | Decoding has begun |
| `TrackAudibleEvent` | **The first packet left the socket.** Carries `positionMs`, `sentAt` and `audibleAt`. Anchor lyrics here. |
| `TrackEndEvent` | `reason`: `finished`, `loadFailed`, `stopped`, `replaced` or `cleanup` |
| `TrackExceptionEvent` | `exception{severity, message, cause, source}`, where severity is `common`, `suspicious` or `fault` |
| `TrackStuckEvent` | No audio for 10 s while not finished |
| `MixArmedEvent` / `MixStartedEvent` / `MixPromotedEvent` / `MixDroppedEvent` | The crossfade lifecycle |
| `SendStallEvent` | The send loop fell behind schedule by `stallMs` |
| `PositionEvent` | A seek moved the clock |
| `WebSocketClosedEvent` | The voice gateway closed (`code`, `reason`, `terminal`) |

## The wire clock

Cadence's main promise is that **"position" means what listeners hear**, not where the decoder happens to be.

```
decode ─► resample ─► time-stretch ─► effects ─► mix ─► opus ─► DAVE ─► AEAD ─► UDP
                                                                                 │
                                                       position + sentAt stamped here
```

- The sender pulls **one** 20 ms frame per tick and writes it straight to the socket. There's no queue after it to drift.
- Every frame carries its exact source position through resampling and time-stretching, so positions stay correct under nightcore, vaporwave or an automix tempo ramp.
- `playerUpdate.state.player` includes an anchor (`anchorMs`, `anchorSentAt`, `anchorAudibleAt`, `rate`, `sounding`). Compute the live position as:

  ```
  position = anchorMs + (clientNow - offset - anchorSentAt) * rate
  ```

  `offset` comes from an NTP-style exchange with `GET /v1/clock`.
- Set `CADENCE_PLAYOUT_DELAY_MS` to the measured gap between Discord receiving audio and listeners hearing it. This one constant replaces the variable error we used to live with.

## Instant starts

Like Spotify, Cadence keeps the first ~12 seconds of upcoming tracks ready, so pressing play doesn't wait on Deezer.

- `loadtracks` preloads its first result, so the usual "search, then play" flow starts from memory.
- Send the next tracks in your queue to `POST /v1/prefetch` and they'll start just as fast.
- On play, a preloaded track starts decoding immediately while the rest is fetched in the background. A track that wasn't preloaded plays the normal way.
- `cadence_prefetch_total{outcome}` on `/metrics` counts `queued`, `hit`, `miss`, `failed` and `dropped`.
- **Disk cache.** With `CADENCE_AUDIO_CACHE_DIR` set, a track played a second time within 7 days, in any server, is saved to disk as it plays. From then on it starts and seeks with no Deezer requests at all. Only complete downloads are kept, and the cache never grows past `CADENCE_AUDIO_CACHE_GB`. See `cadence_audio_cache_total{outcome}`, `cadence_audio_cache_bytes` and `cadence_audio_cache_files`.
- **Seeks** of 10 seconds or more fetch from the target with one range request, or read the disk cache with none, and stay frame-accurate. `cadence_seek_total{outcome}` counts `range`, `disk` and `fallback` (a full stream, for a track whose layout cannot be proven). `cadence_cdn_range_ignored_total` counts range requests the CDN answered with the whole file.
- **Fallback source.** `cadence_fallback_total{kind}` counts searches (`search`) and Spotify/Apple mirrors (`mirror`) that SoundCloud answered because Deezer had nothing.

## Mixing

```
deck A ─► bass / sweep DSP ─┐
                            ├─► equal-power crossfade ─► volume ─► out
deck B ─► bass / sweep DSP ─┘        (armed at a downbeat on deck A)
```

1. `POST …/mix/deckb` loads and pre-buffers the next track, starting from any position (for example, past its intro).
2. `POST …/mix/crossfade` with `atPositionMs` arms the transition. It starts on the exact frame where deck A crosses that cue.
3. A `profile` describes the transition: `bassSwapAt`, `bassSwapRamp`, and high-pass sweeps on either deck. `tempoRatio` beat-matches deck B, which then glides back to its natural speed over 11 s. `gainRatio` matches loudness.
4. When the fade completes, deck B is promoted and `MixPromotedEvent`, `TrackEndEvent{replaced}` and `TrackStartEvent` are emitted.

The node makes no musical decisions. The planner in your bot chooses what to mix and how, and Cadence executes it to the frame.

## Discord voice

- Voice gateway **v8** with `seq_ack` heartbeats and resume. Reconnects use exponential backoff and resume when they can, so audio keeps flowing through gateway blips.
- **DAVE** through `libdave`, including MLS commits, welcomes, proposals and protocol transitions. When a call negotiates E2EE, audio is *held*, never sent unencrypted, until the epoch is ready; a hold that lasts 15 s restarts the voice session. Calls Discord runs without E2EE (DAVE protocol 0) play straight away.
- Transport encryption is `aead_aes256_gcm_rtpsize`, falling back to `aead_xchacha20_poly1305_rtpsize`. The deprecated modes are never offered.
- One Opus encoder, sender, UDP socket and DAVE session per connection, torn down together. A voice update with the same session is ignored. A new one closes the old connection and waits for its sender to exit before dialing.

## Architecture

```
cmd/cadence            entry point, wiring, graceful shutdown
internal/api           REST + WebSocket server
internal/session       client sessions, resume, per-guild players and voice
internal/player        decks, crossfades, events, the wire clock
internal/voice         Discord voice gateway, DAVE, UDP, RTP, AEAD, send loop
internal/audio         PCM format, resampler, WSOLA stretcher, biquads, deck DSP
internal/codec         Opus encoder (libopus), MP3 / PCM decoders
internal/source        track model, resolver, mirroring, match scoring
internal/source/*      deezer, soundcloud, spotify, applemusic
internal/lavalink      Lavalink v4 protocol compatibility
internal/analysis      tempo, key, meter, phrases, structure, loudness, vocals
internal/httpx         per-source HTTP clients with proxy failover and dead-connection retry
internal/cache         bounded TTL LRU
internal/audiocache    bounded on-disk cache of repeat-played tracks
internal/config        environment configuration
internal/metrics       Prometheus exposition
```

## Status

Cadence is **in production**. It is greed's only audio node, running on two servers (Dallas, US and Germany, EU), with greed routing each guild to the node nearest its voice region and failing over to the other if one goes down. The native (`libopus` + `libdave`) build plays to Discord voice with DAVE end-to-end encryption, driven by greed over the Lavalink v4 protocol. Everything above is covered by unit tests: the audio maths, filters and their ramps, reverb, analysis, voice protocol framing, player lifecycle, resolver and API. Still to do:

- Measure `CADENCE_PLAYOUT_DELAY_MS` against a recorded client.
- Move greed's client from Lavalink v4 to the `/v1` protocol.

## License

Cadence is released under the [MIT License](LICENSE). © 2026 greed.

Cadence builds on [godave](https://github.com/disgoorg/godave) and Discord's [libdave](https://github.com/discord/libdave) (Apache-2.0 / MIT), [hraban/opus](https://github.com/hraban/opus) (MIT), [go-mp3](https://github.com/hajimehoshi/go-mp3) (Apache-2.0) and [coder/websocket](https://github.com/coder/websocket) (ISC). The voice protocol work was cross-checked against [disgo](https://github.com/disgoorg/disgo).
