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
5. Say one sentence on the phone and wait. A voice line with the phone's display name must appear on both screens within about 10 s.
6. The phone can only reach LiveKit through `turns:grill-room-turn.<domain>:443`, so steps 4 and 5 passing prove that path. Firefox on the phone is worth a second try, because spike S4 tested Chrome only.
7. Delete the room:
   ```sh
   curl -s -X DELETE -H "Authorization: Bearer $GRILL_ROOM_ADMIN_TOKEN" https://grill-room.<domain>/api/rooms/<room id>
   ```

If step 4 fails while `make e2e-lab` passes, look at the phone's network first: some carriers block TLS to unusual SNI. Then check the traefik logs for a `grill-room-turn` TCP connection.

## Notifications on an Android phone (005-AC24)

`make e2e-push` proves a push goes through Chrome's push service to a desktop Chrome. This check covers a phone, with the browser closed. It takes about five minutes.

1. Create a room and two invites as above, open one invite on the phone in Chrome, the other on the laptop, and join on both.
2. On the phone, open Notifications in the side panel (People on a narrow screen), press "Turn on notifications" and allow them.
3. Close Chrome on the phone, or switch to another app.
4. On the laptop, write a line that mentions the phone's participant, `@<their name>`. Within about 10 s the phone must show one notification named after the room, saying "You were mentioned" and nothing of the line.
5. Write two more mentions. The same notification must update, counting the events, rather than stack new ones.
6. Tap it: Chrome must open the room. Delete the room as above.
