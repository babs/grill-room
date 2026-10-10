# S1: Chrome on-device STT

## Verdict: depends

The API works on Linux desktop Chrome and offers fr-FR and en-US packs. Mixed FR/EN is the open risk: one recognizer takes one `lang`, and Chrome drops the model's language-ID events on this path. Go only if the fr-FR model writes English loanwords acceptably. Firefox, Safari and Chrome Android lack the API, so the CPU Whisper worker (D3) is needed for those participants whatever the result.

## Facts

| Fact | Source |
|---|---|
| Shipped in Chrome 139 desktop (Aug 2025), `is_released: true` | [chromestatus 6090916291674112](https://chromestatus.com/feature/6090916291674112), [New in Chrome 139](https://developer.chrome.com/blog/new-in-chrome-139) |
| Platforms: "Windows, Mac, and Linux with ChromeOS support to follow". Android `Available()` hard-returns `unavailable` | chromestatus `all_platforms_descr`; [on_device_speech_recognition_impl.cc](https://chromium.googlesource.com/chromium/src/+/main/chrome/browser/speech/on_device_speech_recognition_impl.cc) |
| `processLocally`, `available()`, `install()` since Chrome 139, unprefixed. Firefox preview only, Safari none | [MDN BCD](https://github.com/mdn/browser-compat-data/blob/main/api/SpeechRecognition.json), [MDN processLocally](https://developer.mozilla.org/docs/Web/API/SpeechRecognition/processLocally) |
| `processLocally = true`: recognition "MUST be performed locally" | [spec](https://webaudio.github.io/web-speech-api/#dom-speechrecognition-processlocally) |
| Engine is SODA (Live Caption). Default languages: en-US, fr-FR, it-IT, de-DE, es-ES, ja-JP, hi-IN, pt-BR, ko-KR, pl-PL, th-TH, tr-TR, id-ID, cmn-Hans-CN, cmn-Hant-TW, vi-VN, ru-RU | [soda_installer.cc](https://chromium.googlesource.com/chromium/src/+/main/components/soda/soda_installer.cc), [constants.h](https://chromium.googlesource.com/chromium/src/+/main/components/soda/constants.h) |
| About 60 MB per pack | [Intent to Ship](https://groups.google.com/a/chromium.org/g/blink-dev/c/VNOok2dbmHM/m/TQpe9shjCgAJ) |
| Mixed languages: Web Speech passes a single `options->language` to SODA, and `OnLanguageIdentificationEvent` is an empty function | [soda_speech_recognition_engine_impl.cc](https://chromium.googlesource.com/chromium/src/+/main/content/browser/speech/soda_speech_recognition_engine_impl.cc) |
| `install()` needs a user click while status is `downloadable` (`NotAllowedError` otherwise). `start()` with a missing pack fires `language-not-supported` | [Blink speech_recognition.cc](https://chromium.googlesource.com/chromium/src/+/main/third_party/blink/renderer/modules/speech/speech_recognition.cc) |
| Anti-fingerprinting: an origin that never called `install()` sees `downloadable` even when the pack is on disk | `ApplyOriginMasking` in on_device_speech_recognition_impl.cc |

## Verified here

- Chrome 155.0.8059.39 on Linux (claude-in-chrome, `http://localhost:8741`): secure context yes, `SpeechRecognition` unprefixed, `processLocally`, `available()` and `install()` all present.
- `available({processLocally:true})` returned `downloadable` for `["fr-FR"]`, `["en-US"]` and both. No `SODA*` dir under `~/.config/google-chrome`, so the packs really are absent, not just masked.
- Not done: `install()` (downloads about 60 MB per pack) and any voice test.

## User test protocol

1. Open the page, click "Install language pack(s)" with fr-FR, then with en-US. Expect `install(...) -> true`, then "Check availability" shows `available`.
2. fr-FR, Start, say "Demain on valide le plan avec l'équipe produit." Note transcript accuracy and punctuation.
3. en-US, Start, say "We need to ship the spec before Friday." Same notes.
4. fr-FR, Start, say "on doit merger la feature flag avant la release". Then tick Mixed mode and repeat. Note which words break, and whether the second parallel recognizer starts or errors.
5. Read latency from each final line ("ms after last interim", "ms after speechend"). Over 1 s hurts a live room. To confirm no audio leaves, unplug the network and redo step 2.

Record per step: transcript, errors from the log, latency, and a 1-5 quality score versus faster-whisper.

## Serving

- `python3 -m http.server 8741 --bind 0.0.0.0 --directory spikes/s1`, run with nohup. PID in `.scratchpad/s1/http.pid`, log in `.scratchpad/s1/http.log`.
- URL: <http://localhost:8741/>. From another machine, `ssh -L 8741:localhost:8741 <this host>` keeps it on localhost. Or start Chrome with `--unsafely-treat-insecure-origin-as-secure=http://<lan-ip>:8741` and open `http://<lan-ip>:8741/`.
- Stop: `kill "$(cat .scratchpad/s1/http.pid)"`.
- Packs land in `~/.config/google-chrome/SODALanguagePacks`. Chrome removes unused packs on its own schedule.
