# HDHR Stream

A small web app to watch your HDHomeRun's over-the-air channels remotely. The
Go backend pulls each channel's transcoded MPEG-TS from the tuner and repackages
it to HLS with ffmpeg: the HDHomeRun transcodes the video to H.264 so it's copied
as-is, but its AC-3 audio (which browsers can't decode over HLS) is re-encoded to
stereo AAC — cheap, audio-only work. The browser SPA plays it via native HLS on
Safari/iPad and hls.js elsewhere.

Designed to run on an always-on machine on the same LAN as the tuner, reached
from elsewhere over a WireGuard VPN — so it serves plain HTTP with no built-in
auth (the VPN is the access control).

## Requirements

- Go 1.22+ (to build)
- ffmpeg on `PATH` (or set `-ffmpeg`)
- An HDHomeRun reachable on the LAN

## Install (Linux + systemd)

Clone, run the installer, done — it builds the binary, installs it to
`/opt/hdhrstream`, writes config to `/etc/hdhrstream.env`, and enables a systemd
service:

```sh
git clone https://github.com/jancona/HDHRStream.git
cd HDHRStream
./install.sh            # prompts for your HDHomeRun URL
```

Then browse to `http://<this-host>:8080`. Useful follow-ups:

```sh
systemctl status hdhrstream      # is it running?
journalctl -u hdhrstream -f      # follow logs
sudo nano /etc/hdhrstream.env    # change settings, then restart:
sudo systemctl restart hdhrstream
./uninstall.sh                   # remove (add --purge to also delete config)
```

The service runs as a locked-down `DynamicUser` with its HLS scratch on tmpfs
(`/run/hdhrstream`). Re-running `./install.sh` upgrades in place and keeps your config.

## Run manually (without systemd)

```sh
go build -o hdhrstream .         # or: make build
HDHR_URL=http://192.168.1.10 ./hdhrstream
# open http://<this-host>:8080
```

### Configuration

All flags have env-var equivalents:

| Flag        | Env             | Default                  | Notes                                   |
|-------------|-----------------|--------------------------|-----------------------------------------|
| `-hdhr`     | `HDHR_URL`      | (required)               | HDHomeRun base URL, e.g. `http://192.168.1.10` |
| `-listen`   | `HDHR_LISTEN`   | `:8080`                  | Listen address                          |
| `-profile`  | `HDHR_PROFILE`  | `heavy`                  | Default transcode profile (lower = less bandwidth) |
| `-workdir`  | `HDHR_WORKDIR`  | `$TMPDIR/hdhrstream`     | HLS scratch dir (a tmpfs/ramdisk is ideal) |
| `-ffmpeg`   | `HDHR_FFMPEG`   | `ffmpeg`                 | Path to ffmpeg                          |
| `-ffmpeg-loglevel` | `HDHR_FFMPEG_LOGLEVEL` | `warning`     | ffmpeg `-loglevel` (`warning`, `info`, `verbose`) |
| `-debug`    | `HDHR_DEBUG`    | `false`                  | Verbose debugging: per-request server logs, ffmpeg `verbose`, and the browser `[hdhr]` console trace |

By default the server logs only startup, stream sessions, and failed requests.
Run with `-debug` (or `HDHR_DEBUG=1`) to trace everything while diagnosing
playback: every HTTP request (with client + user-agent), full ffmpeg output, and
the in-browser stall/recovery trace in the DevTools console.

Transcode profiles (per the [HDHomeRun HTTP API](https://info.hdhomerun.com/info/http_api)):
`heavy`, `mobile`, `internet720`, `internet480`, `internet360`, `internet240`.
Lower = less upload bandwidth.

## How it works

- `GET /api/channels` — channel list from the tuner's `lineup.json` (encrypted/DRM channels are filtered out).
- `GET /stream/{ch}/index.m3u8` — starts (or reuses) an ffmpeg session for that channel and serves the HLS playlist.
- `GET /stream/{ch}/seg*.ts` — serves HLS segments.

One ffmpeg process runs per active channel and is reaped ~30s after the last
segment request, so tuners free themselves when you stop watching. Concurrent
streams are capped at the device's tuner count (returns 503 when all are busy).

## Smoke test without the tuner

`cmd/mockhdhr` emulates the HDHomeRun API with an ffmpeg-generated test pattern:

```sh
go run ./cmd/mockhdhr &              # serves on :9000
HDHR_URL=http://localhost:9000 ./hdhrstream
```

## Cross-compiling

The web assets are embedded, so the build is a single static binary. To build a
Linux binary from another machine (e.g. a Mac):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o hdhrstream .
```

The systemd unit and config templates live in [deploy/](deploy/); `./install.sh`
applies them.
