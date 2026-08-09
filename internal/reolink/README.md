# Reolink

Experimental native Baichuan source for Reolink cameras on a trusted local network.

- H.264/H.265 video from `main`, `sub`, and `extern` profiles
- AAC audio or camera ADPCM converted to PCMA
- PCMA/PCMU/PCM/PCML two-way audio converted to camera ADPCM
- Direct LAN TCP or UID-selected LAN UDP
- Explicit NVR and multi-lens channels

No FFmpeg, RTSP, or HTTP-FLV input bridge is required.

## Compatibility

The adapter discovers the selected profile's codecs from live media and probes talkback directly. Model identity, observed channels, and capability-query status are diagnostic; they do not select a model-specific media path.

Hardware testing covers E1 Zoom/E340 main/sub over TCP and dual-lens TrackFlex main/sub on both channels over TCP and UID, including H.264, H.265, AAC, and talkback. Current Reolink specifications place [E1 Outdoor Pro](https://reolink.com/us/product/e1-outdoor-pro/), [Duo 3 PoE](https://support.reolink.com/c/duo-3-poe/), and [TrackMix PoE](https://reolink.com/us/product/reolink-trackmix-poe/) inside the adapter's runtime codec/profile/talk envelope. Independent Baichuan field reports also cover Duo 2 WiFi, Video Doorbell, TrackMix WiFi 6, and RLC-1212A in [go2rtc PR #2263](https://github.com/AlexxIT/go2rtc/pull/2263). Neither source validates this adapter on those cameras, so they are compatibility candidates rather than support claims. The base [E1](https://reolink.com/product/e1/) publishes H.264 and a lower concurrent-stream limit. Battery cameras need a different wake/cloud lifecycle and are not covered.

## Configuration

```yaml
streams:
  camera_main: reolink://${REOLINK_USER}:${REOLINK_PASSWORD}@camera.local/main
  camera_sub: reolink://${REOLINK_USER}:${REOLINK_PASSWORD}@camera.local/sub?audio=0&backchannel=0
  camera_channel: reolink://${REOLINK_USER}:${REOLINK_PASSWORD}@camera.local/main?channel=1
  camera_uid: reolink://${REOLINK_USER}:${REOLINK_PASSWORD}@${REOLINK_UID}/main?transport=uid
```

Defaults: port `9000`, channel `0`, TCP transport, and enabled backchannel.

| Option | Values | Purpose |
|---|---|---|
| path | `main`, `sub`, `extern` | Camera profile |
| `channel` | `0`-`255` | NVR or multi-lens channel |
| `transport` | `tcp`, `uid` | Direct TCP or local UID discovery and UDP |
| `local` | IPv4 address | UID discovery interface |
| `broadcast` | IPv4 address | Discovery destination for a routed VLAN |
| `video`, `audio` | `0`/`1`, `false`/`true` | Received tracks |
| `backchannel` | `0`/`1` | Two-way audio; enabled by default |

Set both `video=0` and `audio=0` for a backchannel-only source. The source still drains its selected preview. Use a low-bandwidth profile. Dial fails if the camera does not support talkback.

## Behavior

- One source serves all go2rtc consumers. Duplicate sources open separate camera sessions.
- Multiple configured sources retain go2rtc's ordered codec matching. A later H.264 source can serve a consumer that does not negotiate HEVC; `video=0` prevents a source from satisfying video.
- Connections are demand-driven. When the last consumer detaches, go2rtc stops the source and the adapter synchronously releases its preview, talk, transport, receivers, and session budget.
- The local guard permits two `main` and ten combined `sub`/`extern` sessions per endpoint and credential, matching the largest limits in the surveyed current specifications. A camera or a pool shared with other clients may reject earlier; camera errors remain authoritative.
- Enabled audio must start within 10 seconds. A profile reconnects after 15 seconds without a valid audio frame. It also reconnects after a 60-second window below 75% of the advertised audio clock. Set `audio=0` if audio is disabled or unwanted.
- Best-effort setup queries run concurrently per configured source and add compact model, firmware, channel, and discovery status. Failed queries do not block streaming.
- Repeated talkback negotiation, writes, and teardown are tested. Audible quality, echo, and client UI behavior remain experimental.

## Security and limits

- Use only on a trusted network. Baichuan does not authenticate the camera before login negotiation. It may select an unencrypted session.
- A hostile broadcast-domain peer can impersonate a camera during UID discovery and attempt offline password guessing.
- UID mode is local only. It has no cloud P2P, relay, NAT traversal, or UPnP. The default requires one IPv4 broadcast domain. Routed discovery requires directed-broadcast and return routes.
- Keep credentials in environment substitutions. There is no automatic RTSP, RTMP, HTTP-FLV, or FFmpeg fallback.
