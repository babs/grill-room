# Lab check: a participant outside the LAN

`make e2e-lab` proves the TURN/TLS path from the dev machine through the lab's traefik, but that machine sits on the LAN. This check covers what it can't: a participant on a phone over 4G, with no route into the LAN, hearing and being heard (001-AC38, 001-AC6). It takes about five minutes.

You need the deployment up (`make k8s-apply`), a laptop on the LAN, and a phone with Wi-Fi off.

1. On the laptop, create a room and two invites with the admin token:
   ```sh
   curl -s -H "Authorization: Bearer $GRILL_ROOM_ADMIN_TOKEN" -H 'Content-Type: application/json' \
     -d '{"name":"4G check"}' https://grill-room.<domain>/api/rooms
   curl -s -X POST -H "Authorization: Bearer $GRILL_ROOM_ADMIN_TOKEN" https://grill-room.<domain>/api/rooms/<room id>/invites
   ```
   Run the second command twice, one invite per person. Each answer holds the invite link in `url`.
2. Turn Wi-Fi off on the phone, check it shows 4G or 5G, and open one invite link in its browser. The padlock must show a valid certificate.
3. Open the other link on the laptop. Join the room on both sides, then press Join audio on both.
4. Speak on the phone and listen on the laptop, then the other way round. Each side must hear the other within a second or two, and the speaking dot must light next to the speaker's name.
5. Say one sentence on the phone and wait. With parakeet (spec 007), a voice line with the phone's display name must appear on both screens within 5 s. Then speak at the same time on the phone and the laptop: both get their lines.
6. The phone can only reach LiveKit through `turns:grill-room-turn.<domain>:443`, so steps 4 and 5 passing prove that path. Firefox on the phone is worth a second try, because spike S4 tested Chrome only.
7. Delete the room:
   ```sh
   curl -s -X DELETE -H "Authorization: Bearer $GRILL_ROOM_ADMIN_TOKEN" https://grill-room.<domain>/api/rooms/<room id>
   ```

If step 4 fails while `make e2e-lab` passes, look at the phone's network first: some carriers block TLS to unusual SNI. Then check the traefik logs for a `grill-room-turn` TCP connection.

## STT engine rollback (007-AC17)

One step: switch the worker back to faster-whisper and restart it. No data changes either way.

```sh
KUBECONFIG=<lab kubeconfig> kubectl -n grill-room set env deploy/stt APP_ENGINE=whisper
```

`set env` restarts the Deployment by itself. Set `APP_ENGINE=parakeet` the same way to come back. The image carries both engines for one release. `make k8s-apply` puts back the manifest's value, so make the change in `deploy/k8s/stt.yaml` to keep it. The second way out is the previous image tag: set `GRILL_STT_IMAGE` in `.env` to it, then `make k8s-apply`.

## On-device transcription (010)

On a desktop Chrome with 4 cores or more, after a deploy. It takes about two minutes, plus the first download of about 430 MB.

1. Check the model volume: the `models` init container's log lists seven files, downloaded or skipped, and `curl -sI https://grill-room.<domain>/models/<revision>/vocab.txt` answers 200 with `Cache-Control: public, max-age=31536000, immutable` and `Cross-Origin-Resource-Policy: same-origin`. The revision is in the `device_stt_offered` log line of the room server.
2. Join a room, join the audio, open the audio menu and turn on "Transcribe on this device". The menu shows the download size, the progress, then "Testing this device…", then that this device can write your voice down.
3. Say a sentence. It shows as a voice line whose "(voice)" mark reads "Transcribed on <your name>'s device"; People shows the device badge on you, and your details card says "Transcribed on their device".
4. Turn the option off and say another sentence. Within about 5 s the next line reads "Transcribed by the server".
5. Delete the room as above.

## Notifications on an Android phone (005-AC24)

`make e2e-push` proves a push goes through Chrome's push service to a desktop Chrome. This check covers a phone, with the browser closed. It takes about five minutes.

1. Create a room and two invites as above, open one invite on the phone in Chrome, the other on the laptop, and join on both.
2. On the phone, open your name's menu in the header, then Notifications, press "Turn on notifications", then "Turn on notifications" in the sheet that explains them, and allow them.
3. Close Chrome on the phone, or switch to another app.
4. On the laptop, write a line that mentions the phone's participant, `@<their name>`. Within about 10 s the phone must show one notification named after the room, saying "You were mentioned" and nothing of the line.
5. Write two more mentions. The same notification must update, counting the events, rather than stack new ones.
6. Tap it: Chrome must open the room. Delete the room as above.
