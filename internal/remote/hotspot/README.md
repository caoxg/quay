# quay-hotspot

A small service for a board run by NetworkManager (the Arduino UNO Q's Debian image): when no Wi‑Fi
it can join is in range, the board opens a hotspot of its own; when Wi‑Fi is back, it goes back to
it. Quay installs it with `quay hotspot board` or `quay hotspot both` (and the **Network** card in the
app), and removes it with `quay hotspot phone` or `off`. It is one Python 3 file with no
dependencies, so it also works without Quay.

## What it does

It owns one NetworkManager profile, `quay-hotspot` (a WPA2 access point with `ipv4.method shared`,
so the board is `10.42.0.1` on it and hands out addresses), and never touches the others:

- No Wi‑Fi 45 s after the Wi‑Fi device comes up, or 60 s after losing Wi‑Fi: it opens the hotspot.
- While nobody is on the hotspot for 3 minutes, every 5 minutes it closes it, rescans, and joins a
  saved Wi‑Fi in range (highest `autoconnect-priority` first; profiles with autoconnect off are
  skipped); with none in range it opens the hotspot again. It never closes the hotspot while a
  device is on it (wpa_supplicant's station list over D-Bus; with an older wpa_supplicant, devices
  that talked to the board recently).
- If another hotspot is up (one set up by Duck Setup, say), it does nothing.

`quay-hotspot open` asks the running service to open the hotspot now, `quay-hotspot close` to close
it and look for Wi‑Fi now, `quay-hotspot status` prints its state (also in
`/run/quay-hotspot/state.json`). The timings are at the top of the file.

Quay pairs it with phone hotspots: a saved network marked as a phone hotspot is joined
automatically only when the away plan is `phone` or `both`, with priority −10, after the everyday
Wi‑Fi (see `/etc/quay/network.json`).

## Installing by hand

```bash
sudo install -m 755 quay-hotspot /usr/local/lib/quay/quay-hotspot
sudo install -m 644 quay-hotspot.service /etc/systemd/system/
sudo nmcli connection add type wifi ifname wlan0 con-name quay-hotspot autoconnect no \
  ssid my-board 802-11-wireless.mode ap 802-11-wireless.band bg ipv4.method shared ipv6.method disabled \
  wifi-sec.key-mgmt wpa-psk wifi-sec.psk 'a password'
sudo systemctl daemon-reload
sudo systemctl enable --now quay-hotspot
```

To remove it: `sudo systemctl disable --now quay-hotspot`, delete the two files, and
`sudo nmcli connection delete quay-hotspot`.

## Tests

`python3 -m unittest test_quay_hotspot` runs its decisions against a fake NetworkManager (Quay's
`go test ./internal/remote` runs them too).
