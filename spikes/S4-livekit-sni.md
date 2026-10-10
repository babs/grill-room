# S4: LiveKit audio with no UDP, through traefik on 443

**Verdict: works.** Audio flows with zero UDP between browser and server. TURN/TLS on 443 through traefik's SNI router carries it when forced to relay. With UDP blocked and default ICE, Chrome picks ICE over TCP on 7881 and keeps TURN/TLS as a working fallback. This answers spec open question 1: TURN/TLS on 443 is enough, no UDP port-forward needed.

Code: `s4-livekit-sni/` (`compose.yaml`, `traefik-rule.yaml`, `s4.spec.js`, `run.sh`). LiveKit v1.13.9, livekit-client 2.22.3 (2.22.4 was under the pnpm min-age gate), Playwright 1.64, installed Chrome 155 with a fake mic.

## TLS mode: terminated at traefik

`turn.external_tls: true` makes LiveKit listen in plain TURN on `tls_port` and still advertise `turns:<domain>:443?transport=tcp` (port 443 is hardcoded in `roommanager.go`). Traefik terminates TLS with its wildcard cert, so LiveKit never needs the key.
```yaml
tcp:  # traefik file provider
  routers:
    s4-turn: { rule: "HostSNI(`turn.<dev-domain>`)", entryPoints: [websecure], service: s4-turn, tls: {} }
  services:
    s4-turn: { loadBalancer: { servers: [{ address: "host.docker.internal:5349" }] } }
```
```yaml
rtc: { tcp_port: 7881, udp_port: 7882, use_external_ip: false }
turn: { enabled: true, domain: turn.<dev-domain>, tls_port: 5349, external_tls: true,
        allow_restricted_peer_cidrs: [<SFU private IP range>] }
```
Signalling is a plain HTTP router `livekit.<dev-domain>` to port 7880. Traefik logged `Handling TCP connection address=host.docker.internal:5349` for `s4-turn`.

## Results (75 s per case, two Chrome contexts, receiver stats)

| Case | Selected pair (both peers) | bytesReceived | lost | jitter avg/max | jitter buffer | ICE RTT |
|---|---|---|---|---|---|---|
| A, `iceTransportPolicy: relay` | relay, relayProtocol `tls`, url `turns:turn.<dev-domain>:443` → SFU udp 7882 inside the container | +695 kB, steady 9.3 kB/s | 0 | 0.5 / 1 ms | 40 ms | ~1 ms |
| B, default policy, UDP blocked | prflx/tcp → host/tcp 7881. Relay/tls pairs also succeeded. All 9 UDP pairs stuck in-progress | +714 kB | 0 | 0.2 / 1 ms | 39 ms | ~0.5 ms |

Both cases also pass at 10 s with the same rates. No stall after 60 s, so traefik's default entrypoint timeouts don't cut TCP/TLS routes or the signalling WebSocket. Everything runs on one host, so the RTTs only show that the traefik TLS hop adds under 1 ms. Mouth-to-ear latency was not measured.

## Gotchas found

- Not publishing UDP doesn't block it on one host. The SFU container reaches the browser's LAN IP and connected over UDP (prflx). A sidecar sharing LiveKit's netns (NET_ADMIN) drops non-loopback UDP both ways. Loopback stays open because the TURN relay sends to the SFU on its own IP.
- `node_ip` on loopback plus `enable_loopback_candidate` breaks ICE/TCP. Pion's TCP mux keys connections by ufrag and local IP, so the docker-forwarded connection on eth0 never matched the loopback candidate.
- Chrome's `--force-webrtc-ip-handling-policy=disable_non_proxied_udp` still checked UDP host pairs in Chrome 155, so it's useless as a client-side UDP block.
- TURN relays to the SFU's private IP. LiveKit denies private peers by default, so `allow_restricted_peer_cidrs` must list it.

## Needed in prod (kube)

- `IngressRouteTCP` on `websecure`: `match: HostSNI(\`turn.<domain>\`)`, `tls: {secretName: <wildcard or turn cert>}`, service `livekit:5349`. Plus an `IngressRoute` for `livekit.<domain>` on 7880 (WebSocket).
- LiveKit: `external_tls: true`, `allow_restricted_peer_cidrs: [<pod CIDR>]`, no UDP Service.
- Optional: expose 7881/tcp (hostPort or LB) for ICE/TCP. TURN/TLS covers clients without it.
- Untested: Firefox rejects a TURN XOR-MAPPED-ADDRESS that is loopback or wildcard. If it breaks, enable `turn.proxy_protocol` with PROXY v2 from traefik's TCP serversTransport.

## Cleanup

`run.sh` writes `zz-s4-livekit-spike.yaml` into the traefik rules dir and removes it on exit, along with `compose down`. Removed: the rule file, the containers and their network, Chrome processes. Kept: images `livekit/livekit-server:v1.13.9` and `alpine:3.22`, untracked `s4-livekit-sni/{.env,node_modules,test-results}`, and logs in the scratchpad. The other routes stayed up (`grill-room` `/healthz` 200 after the runs).
