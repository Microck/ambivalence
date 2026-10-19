# ambivalence protocol

JSON text frames on WebSocket path `/v1`.

The hub is an untrusted relay. The first message from each socket is `hello`; after that the hub forwards everything between a phone (host) and its agent. Hubs never see a secret and authorise nothing: any hub on the Wi-Fi can be rogue, so phone and agent authenticate each other end to end and seal all camera traffic. The reference implementation is `internal/link` (with pinned test vectors) and `internal/host/fake.go` (phone side); the Android, iOS and web hosts implement the same construction.

Notation: `H(label, f1, f2, …)` is SHA-256 over `"ambivalence-v1/" + label`, then each field as a 4-byte big-endian length followed by its bytes. `MAC(key, label, …)` is HMAC-SHA256(key, H(label, …)). Byte fields travel as lowercase hex. Nonces are 16 random bytes.

## hello

Phone:

```json
{"v":1,"type":"hello","role":"host","device":"<32 hex>","host":{"name":"android","platform":"android"}}
```

`device` is the phone's permanent public id, made on first launch. The hub assigns a fresh 6-character code (alphabet `23456789ABCDEFGHJKLMNPQRSTUVWXYZ`) on every connection. A phone may connect to several hubs at once (saved and discovered); its agent can reach it on any of them.

Agent, first time: `{"v":1,"type":"hello","role":"agent","code":"A2B3C4"}`

Agent, linked: `{"v":1,"type":"hello","role":"agent","device":"<32 hex>"}`

If the phone is not connected the hub answers `linked phone is offline: open the ambivalence app`; if another agent holds the phone's slot, `phone is busy with another agent connection`. Agents retry both. The hub never evicts a connected agent.

## welcome / status

```json
{"v":1,"type":"welcome","role":"host","code":"A2B3C4","connected":false,"hubId":"<32 hex>"}
```

`hubId` is random per hub process. Two URLs can reach one hub (tunnel and LAN), and each hello would evict the other socket, so a phone keeps one connection per `hubId`. On a duplicate it keeps the saved URL, otherwise the older connection, and parks the other URL until the kept one drops. `hubId` is not a trust signal, so the saved URL is never parked.

`status` carries `connected` whenever the other side joins or leaves. The hub may repeat it; phones act only on a change.

## pairing (once)

Numeric comparison with a commitment:

| step | message | content |
|---|---|---|
| agent → phone | `pair1` | `commit = H("commit", agentPub, nA)` |
| phone → agent | `pair2` | `pub = phonePub`, `nonce = nB`, `device` |
| agent → phone | `pair3` | `pub = agentPub`, `nonce = nA`; phone checks the commit |
| both | | show `SAS`: the first 30 bits of `H("sas", agentPub, phonePub, nA, nB)` as 6 code-alphabet chars |
| phone → agent | `pair4` | `mac = MAC(key, "confirm", device)`, sent after the user taps Confirm |

Public keys are uncompressed P-256 points (65 bytes). `key = H("key", ECDH, agentPub, phonePub)` is the link key, stored by both sides and never sent. Because of the commitment, a man in the middle must guess the SAS in one live attempt (about 1 in 10⁹). `pair1`/`pair3`/`pair4` are request/reply by `id`. The pairing session continues sealed with `nP = nB`.

## auth (every later session)

When an agent arrives, a linked phone sends `{"type":"auth","nonce":nP}`. The agent replies with `{"type":"auth","nonce":nP,"agentNonce":nA,"mac":MAC(key,"agent",device,nP,nA)}`. The phone answers `{"type":"authok","nonce":nP,"mac":MAC(key,"phone",device,nP,nA)}`. Each side uses a handshake at most once. A phone sends `{"type":"reject"}` to have the hub drop an agent that fails, or that neither authenticates nor starts pairing within 15 s.

## sealed traffic

After auth or pairing, every camera request, reply and frame is sealed:

```json
{"v":1,"type":"sealed","id":"…","box":"<base64>"}
```

`sk = H("session", key, device, nP, nA)`. Agent→phone uses AES-256-GCM key `H("seal-agent", sk)` and phone→agent uses `H("seal-phone", sk)`. A box is `seq (8 bytes BE) || ciphertext+tag`, with nonce `4 zero bytes || seq` and `seq` as additional data. Each direction counts from 1, and a receiver accepts only exactly the next `seq`. This blocks replay, dropping, reordering and splicing. A sealed `frame` is followed by one binary WebSocket message holding the sealed JPEG box, the next `seq`. The outer `id` only routes; the inner message must carry the same `id`.

Unsealed camera requests are always refused with `agent not authenticated`. The phone also sends that error, unsealed, for a sealed request it cannot open yet (for example right after reconnecting). The agent then waits for a newer auth and retries once. Hub-originated messages (`welcome`, `status`, errors) are unsealed; forging them can only deny service.

## browser host trust

The browser host is JavaScript served by the hub it connects to, so unlike the apps it must trust that hub: a hub that serves modified code could read its link key (kept in that origin's `localStorage`) or its frames. Use the browser host only through a hub you run. The apps have no such dependency.

## reconnects

When a phone reconnects (app restart, network change) the hub closes its old socket and moves a linked agent into the new room. The phone challenges again, so the agent re-authenticates on the same socket. While the phone is away, agent requests get the offline error.

## capture / frame (inner messages)

```json
{"v":1,"id":"…","type":"capture","quality":80,"maxWidth":1280}
{"v":1,"id":"…","type":"frame","mime":"image/jpeg","bytes":12345,"width":1280,"height":960,"facing":"back","torch":false,"zoom":1,"cameraId":"back"}
```

## command (inner)

`torchOn`, `zoomLevel`, `camera` (`front`/`back` aliases or an exact ID from `cameras`).
An optional `controls` object carries manual camera settings; see [camera controls](camera-controls.md).
`cameras`/`status` may include `capabilities` (including `controlsVersion: 1`) and
`settings`; individual camera entries may include `label` and `capabilities`.
`frame` includes `cameraId` and `settings` when available. The phone replies `status` with the same `id` after the hardware change, or `error`.

## local discovery

Hubs listening beyond loopback advertise mDNS service `_ambivalence._tcp` with TXT `path=/v1`, `v=1` and `addrs=<ipv4,...>` (private addresses of real interfaces, not container or VPN bridges). Phones connect to `ws://<addr>:<port>/v1` directly rather than resolving the SRV hostname. Discovery only locates a hub; it confers no trust and never replaces the saved hub.
