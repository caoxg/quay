<p align="center"><img src="build/icon/icon-256.png" width="96" alt=""></p>

<h1 align="center">Quay</h1>

<p align="center">Deploy Arduino Apps from any git repository to an Arduino® UNO Q — and keep them up to date without losing their data.</p>

<p align="center"><a href="README.zh-CN.md">中文说明</a></p>

<p align="center"><img src="docs/images/overview-en.png" alt="Quay showing a board, its system, the China mirrors and the relay, and two apps — one with an update from GitHub" width="860"></p>

> [!NOTE]
> **Quay is an independent project for any Arduino App.** It is not affiliated with or endorsed by Arduino, or by Luwu Dynamics (陆吾智能), the maker of XGO-Duck.
>
> - **XGO-Duck is only one custom profile.** It is the one Quay ships today, and the example app in these screenshots. The profile was written for Quay, not by Luwu Dynamics.
> - **XGO-Duck itself** is at [github.com/LuwuDynamics](https://github.com/LuwuDynamics). Its app is [xgoduck_runtime_arduino](https://github.com/LuwuDynamics/xgoduck_runtime_arduino); get support for the robot there.
> - Arduino® and UNO™ are trademarks of Arduino S.r.l.; XGO is a name of Luwu Dynamics.

## Highlights

### Fast downloads in mainland China

Boards in China often crawl through updates and Python installs. **China mirrors** tests eight mirrors from the board itself and points pip, uv and apt at the fastest. apt keeps the next fastest and the official servers as fallbacks, so one broken mirror does not stop an update. **Via this computer** sends what has no mirror (Arduino's own packages, App Lab's update) through your computer and its proxy while Quay is connected. Both switch off cleanly and never turn on by themselves.

<p align="center"><img src="docs/images/mirrors-en.png" alt="The China mirrors dialog: download speed and latency of each mirror, measured from the board" width="760"></p>

### Custom apps: what an app needs, done for it

Apps in the **Custom** tab come with a Quay profile (their own [`quay.yaml`](docs/quay-yaml.md), or one built into Quay). It tells Quay what the app needs beyond a plain install, and the install preview lists each step:
- one-time system setup;
- a step before every start;
- start and safety checks;
- the web console and shortcuts to its pages (for XGO-Duck: Calibration and Servo setup);
- policy slots;
- clean-up on erase.

Today Quay ships one such profile, for [XGO-Duck](https://github.com/LuwuDynamics/xgoduck_runtime_arduino), as a worked example. Any other app brings its own `quay.yaml`, and it works the same way.

For robots that run trained models, **Policies** installs models other people trained, checks each one against the app before it reaches the board, and keeps your choice across updates. XGO-Duck reads its models from the community collection [awesome-xgo-duck](https://github.com/caoxg/awesome-xgo-duck).

<p align="center"><img src="docs/images/custom-install-en.png" alt="Installing XGO-Duck from the Custom tab: the preview lists what its profile makes Quay do" width="760"></p>

### System updates, under control

See what the board's update would install and how much it downloads, before anything happens. Download first, even while an app is running, and install later. Two progress bars show the time left. Leave out the on-board desktop (App Lab, Chromium) when you use the board from a computer. The update runs on the board by itself, so a dropped Wi‑Fi link does not break it, and App Lab is not needed.

<p align="center"><img src="docs/images/system-update-en.png" alt="The system update dialog: packages by source, Arduino's packages, download only or install" width="49%"> <img src="docs/images/update-progress-en.png" alt="Download progress with the time left, in the Activity panel and on the System card" width="49%"></p>

### Apps that follow GitHub

Install from any repository, branch or tag. Quay remembers where each app came from. When the branch moves on, the card says how many commits are new and what the latest one is, with a red dot on Update. Updating keeps `data/` (calibration, settings) and backs it up first. Apps installed some other way show GitHub's latest commit too, and one click reinstalls them through Quay.

<p align="center"><img src="docs/images/app-update-en.png" alt="An app card: installed commit, three new commits on GitHub, and a red dot on Update" width="520"></p>

## Everything else

Quay is a small desktop app and command-line tool for the Linux side of the Arduino UNO Q. Point it at a git repository (or a folder on your computer) and it installs the app on the board, keeps the app's `data/` folder across updates, backs it up, and drives the board's own `arduino-app-cli` to start, stop, and choose the app that runs at boot.

It is what you would otherwise do by hand over SSH, and an alternative to re-flashing a board image just to get a new app version.

- **Any git repository, any ref.** Pick a branch, tag, or commit. The repository is fetched on your computer, so the board needs no GitHub access and no git credentials.
- **Updates that keep your data.** `data/` (calibration, settings, recordings) is preserved on every update. A backup is taken first by default.
- **Backup, restore, export.** Backups are `.tgz` files of `data/` with a small manifest that records the board they came from. Exports are App Lab–compatible zips.
- **Uses the board's tools.** Lifecycle goes through `arduino-app-cli`, so App Lab sees exactly the same apps and states.
- **Per-app hooks, only when needed.** An app can ship a [`quay.yaml`](docs/quay-yaml.md) for one-time system setup, steps that must run before start, health and safety checks, shortcuts, policy slots and uninstall cleanup. Apps without one deploy generically.
- **Policies.** Install trained models (ONNX) from policy indexes or a file. Each one is verified (SHA-256, input/output shapes, joint order) before it is uploaded, and rolled back if the app does not start with it. See [Policies](#policies).
- **The app's own console, one click away.** Quay opens the web UI an app serves (from `app.yaml` `ports`, or `web` in `quay.yaml`).
- **Finds your boards.** Boards on the local network show up by themselves (they announce `_arduino._tcp` over mDNS). A board's name in Quay is its own hostname — the name App Lab shows and `NAME.local` — and renaming it in Quay renames the board.
- **System updates without App Lab.** Quay lists what the board's update would download (on a fresh board, hundreds of MB) and runs the board's own `arduino-app-cli system update`, or leaves out the on-board desktop packages (App Lab, Chromium) a board used from a computer does not need. The update runs detached on the board, so a dropped Wi‑Fi link does not interrupt it.
- **Faster downloads where the board is slow.** `quay mirror cn` tests eight mirrors in mainland China from the board itself and points pip, uv and apt's Debian packages at the fastest; apt also gets the next fastest and the official servers to fall back to by itself if a mirror breaks. `quay relay on` lets the board download through your computer (and its proxy) while Quay is connected. Both are separate, reversible commands, never applied implicitly.
- **SSH or USB.** Wi‑Fi/Ethernet over SSH (agent, key files, or password), or USB through `adb`.
- **Native and small.** The app uses the system web view (WebKit on macOS and Linux, WebView2 on Windows) through [Wails v3](https://v3.wails.io); there is no bundled Chromium. One binary is both the app and the CLI.

## Install

Download the file for your system from the releases page.

| System | File | Notes |
|---|---|---|
| macOS 12+ (Apple silicon and Intel) | `Quay-…-macos-universal.dmg` | Drag Quay to Applications. The build is ad hoc–signed and not notarized: on first launch, open **System Settings → Privacy & Security** and click **Open Anyway**, or run `xattr -dr com.apple.quarantine /Applications/Quay.app`. |
| Windows 10/11 (x64, ARM64) | `Quay-…-windows-amd64.zip` / `-arm64.zip` | Unzip anywhere. `Quay.exe` is the app; `bin\quay.exe` is the CLI (add `bin` to `PATH`). Needs the Microsoft Edge WebView2 runtime, which ships with Windows 11 and current Windows 10. |
| Debian 12+, Ubuntu 22.04+ | `quay_…_amd64.deb` / `_arm64.deb` | `sudo apt install ./quay_…_amd64.deb`. Then start Quay from the menu, or run `quay`. |
| Other Linux | `quay-…-linux-amd64.tar.gz` / `-arm64.tar.gz` | Needs GTK 3 and WebKitGTK 4.1 (`libwebkit2gtk-4.1-0`). Run `./quay`, or `./install.sh` to add it to your menu. |
| Command line only | `quay-cli-…-<os>-<arch>` | A single static binary with no GUI dependencies, for scripts and servers. |

On macOS the CLI is inside the app: `/Applications/Quay.app/Contents/MacOS/quay`. A symlink makes it handy:

```bash
sudo ln -s /Applications/Quay.app/Contents/MacOS/quay /usr/local/bin/quay
```

For USB connections, install `adb` from Android SDK platform-tools (`brew install android-platform-tools`, `apt install adb`, or Google's download) and keep it on `PATH`, or point `QUAY_ADB` at it.

## The app

Run Quay without arguments, or open it from the dock, Start menu or application menu.

1. **Add a board.** Pick it from the boards Quay finds on your network, enter its address (for example `duck.local` or `192.168.1.42`), or choose it from the USB list. Quay connects, names the board after itself (its hostname), remembers its host key, and asks for the password only when no SSH key works.
2. **Install an app.** Choose from **Custom** (apps that come with a Quay profile), paste a git URL (`owner/repo` works for GitHub), or pick a local folder. Quay shows what it found — app name, ref, commit, profile — before anything touches the board.
3. **Run it.** Each app card shows its state, where it came from, its data files and whether it runs at boot. **Console** opens the app's own web UI. The ⋯ menu has update, logs, backup, restore, export, system setup and erase.
4. **Keep the board current.** The **System** card shows the pending system updates; click it to see what they are, choose everything or Arduino's packages only, and install. The board menu (⋯ at the top) also renames the board.

Long operations run in the **Activity** panel with their steps and logs. You can keep working while they run.

Apps whose profile declares policy slots (the duck does) get a **Policies** button: a library of models other people trained, read from the community index the profile names (for the duck, the unofficial [awesome-xgo-duck](https://github.com/caoxg/awesome-xgo-duck)), plus **From a file…** for your own exports. See [Policies](#policies).

If an app's safety check refuses to be stopped (the duck while its servos hold torque, for example), Quay shows the app's reason and lets you cancel or **Force anyway**.

## The command line

```text
quay [command] [flags]

Boards       board scan · board add ADDRESS [--key FILE] [--default] [--upload-key] · board ls · board rm NAME · board default NAME
             board usb · upload-key
Apps         status · deploy REPO|DIR · update APP · start|stop|restart APP · autostart APP|none · logs APP [-f]
             open APP · backup APP · restore APP FILE · export APP · erase APP
Board        upgrade [--check] [--arduino-only] [--skip-desktop] · rename NAME · prepare APP [--revert]
             mirror cn|off|test|list · relay on|off
Policies     policy list APP · policy search [PROFILE] · policy install APP ID|FILE.onnx [--slot S] · policy restore APP [SLOT…]
             policy inspect FILE.onnx [--app PROFILE] · policy index [add|rm URL]
Other        refs REPO · shell · profiles · licenses · gui
Global       -b/--board NAME · -H/--host ADDRESS · --json
```

`ADDRESS` is `host`, `user@host[:port]` (user `arduino` by default), `adb`, or `adb:SERIAL`. `APP` is the folder name under `~/ArduinoApps` (or the app's name).

```bash
# Find boards on the network and save one; it is named after its hostname
quay board scan
quay board add duck.local --default

# Install the latest default branch, a tag, or a local folder
quay deploy LuwuDynamics/xgoduck_runtime_arduino
quay deploy https://github.com/me/weather --ref v1.2.0
quay deploy ./my-app --folder weather_station

# What is on the board?
quay status

# Start it at boot, follow its logs, open its console
quay autostart xgoduck_runtime_arduino
quay logs xgoduck_runtime_arduino -f
quay open xgoduck_runtime_arduino

# Update from the repository it came from (data/ is kept and backed up first)
quay update xgoduck_runtime_arduino
quay update xgoduck_runtime_arduino --ref next-audio-video-tof

# Back up, restore, export for App Lab
quay backup xgoduck_runtime_arduino -o ~/duck-backups
quay restore xgoduck_runtime_arduino ~/duck-backups/xgoduck_runtime_arduino-9f3a21c7-20261023-130501.tgz
quay export xgoduck_runtime_arduino -o duck.zip

# Remove an app (asks you to type its folder name; backs up data/ first)
quay erase weather_station

# Update the board's system, without the on-board desktop packages
quay upgrade --check
quay upgrade --skip-desktop

# Rename the board (it becomes goose.local; App Lab shows the new name too)
quay rename goose
```

`--json` turns any command's progress into one JSON event per line, for scripts and other front ends.

### What `deploy` does

1. Fetches the repository on your computer (a cached bare clone) and checks `app.yaml` and `python/main.py`.
2. Connects to the board and checks the app's safety probe, if it has one.
3. Backs up `data/` to your computer, then stops the app if it is running.
4. Uploads the files and replaces everything in the app folder **except** `data/` and App CLI's `.cache/`.
5. Runs the app's `before_start` hook, if any.
6. Starts the app and waits until it is up.

`deploy` never changes which app runs at boot (that is `quay autostart`) and never changes system settings (that is `quay prepare`). Each command does one thing.

### Board-wide settings

- `quay prepare APP` applies the one-time system change an app declares, such as a serial link speed. It needs `sudo`: Quay tries passwordless sudo, then the SSH login password, then asks. `--revert` undoes the change.
- `quay mirror cn` first tests the built-in mirrors (Tsinghua TUNA, USTC, Alibaba Cloud, Tencent Cloud, Huawei Cloud, BFSU, Nanjing University, SJTU — `quay mirror list`) from the board, then points pip and uv at the fastest PyPI mirror and apt's Debian packages at the fastest Debian mirror. apt is set up with a mirror list (`mirror+file:`): when the chosen mirror fails it moves on by itself to the next fastest ones and finally to deb.debian.org; pip gets the second fastest as an extra index. `--use ID` picks a mirror without testing, `--index URL` / `--debian URL` use mirrors of your own, `--no-pypi` / `--no-debian` leave one alone. `quay mirror test` only measures (Debian download speed and latency from the board, PyPI latency, the official servers for comparison). `quay mirror off` restores the previous files exactly. In the app, the **China mirrors** card opens the same test as a table to pick from.
- `quay relay on` installs a small apt hook on the board (sudo). While Quay is connected — during `quay upgrade`, and whenever the app is open — the board's package downloads go through your computer, and through its proxy if it has one (the system proxy settings, or `--upstream http://127.0.0.1:7890`). When Quay is not connected the board downloads directly, as before. This covers Arduino's own packages, which have no mirror. `quay relay off` removes the hook.
- `quay upgrade` runs the board's system update. It refreshes the package lists, shows what would be installed and how much it downloads, asks, then runs `arduino-app-cli system update` — the update App Lab runs. `--arduino-only` limits it to Arduino's packages; `--skip-desktop` leaves out the on-board App Lab and Chromium (about 290 MB on a fresh board). The board image lets these commands run without a password. The job runs detached on the board: if the connection drops, run `quay upgrade` again to follow it.
- `quay upload-key` appends your SSH public key (`~/.ssh/id_ed25519.pub`, `id_ecdsa.pub` or `id_rsa.pub`) to the board's `~/.ssh/authorized_keys`, like `ssh-copy-id`, so Quay and `ssh` log in without a password. `board add --upload-key` (a checkbox in the app's **Add board**) does it right away; the board menu has it too.
- `quay rename NAME` renames the board the way App Lab does (`arduino-app-cli system set-name`), re-announces it as `NAME.local`, and updates the saved board.

### Policies

Apps that run trained models (ONNX policies) can declare their model files as *slots* in `quay.yaml` (`policies:`). Quay then installs models other people trained in their place:

```bash
quay policy search xgoduck                                  # what the indexes offer
quay policy install xgoduck_runtime_arduino walk-luwu-2026-09-22
quay policy install xgoduck_runtime_arduino ./model_5500.onnx --slot roller --name "Roller 5500"
quay policy list xgoduck_runtime_arduino                    # what each slot holds
quay policy restore xgoduck_runtime_arduino                 # back to the stock models
```

1. **Download and verify.** The model is downloaded to your computer, and its SHA-256 and size are checked against the index.
2. **Check against the slot.** Quay reads the ONNX file itself: input and output shapes, required metadata, and metadata that must match the stock model in that slot (for the duck: `joint_names` and `observation_names`, so a model trained for another joint order is refused). `--force` overrides a failed check.
3. **Swap.** The app is stopped (after its safety check), the model goes into place, and the app starts again. If the app does not come up with the new model, the previous one is put back and the app is started again.
4. **Keep it.** Installed models and the stock originals live in the app's `data/quay-policies/` on the board, so backups include them. After every deploy or update, Quay puts the installed models back before the app starts.

Indexes are YAML files anyone can host. Each profile names its community index, and `quay policy index add URL` adds your own (a team's or a class's). `quay policy inspect model.onnx --app xgoduck` shows a model's shapes and metadata and prints an index entry to publish it.

## App profiles (`quay.yaml`)

Most apps need nothing. An app that needs more puts a `quay.yaml` next to its `app.yaml`:

```yaml
system:                      # one-time root setup, applied with `quay prepare`
  description: Sets the router serial link to 2 Mbps.
  check: grep -qs 2000000 /etc/systemd/system/arduino-router.service.d/90-baud.conf
  apply: bash tools/router-2m.sh
  revert: rm -f /etc/systemd/system/arduino-router.service.d/90-baud.conf && systemctl daemon-reload && systemctl restart arduino-router
before_start:                # after every deploy, before the app starts
  run: python3 tools/prepare-bridge.py
health: { url: http://127.0.0.1:9527/api/status, timeout: 240 }
safety: { url: http://127.0.0.1:9527/api/status, field: enabled, message: Turn torque off first. }
web: { port: 9527 }
```

The full reference is in [docs/quay-yaml.md](docs/quay-yaml.md). Quay also carries built-in profiles for known apps that do not ship a `quay.yaml` yet (`quay profiles` lists them). Currently there is one, for [XGO-Duck](https://github.com/LuwuDynamics/xgoduck_runtime_arduino). It was written for Quay and only drives scripts that already live in that repository; it is not maintained by Luwu Dynamics. A `quay.yaml` in the repository always wins over a built-in profile.

## Files and settings

| What | Where |
|---|---|
| Saved boards, language, your policy indexes | `config.json` in the Quay config folder: `~/Library/Application Support/Quay` (macOS), `%AppData%\Quay` (Windows), `~/.config/Quay` (Linux) |
| Trusted board host keys | `known_hosts` in the same folder (your `~/.ssh/known_hosts` is read too, never written) |
| Backups | `backups/` in the same folder, unless you choose another |
| Repository cache | `Quay/repos` in the user cache folder |
| Policy downloads and index copies | `Quay/policies` in the user cache folder |
| Installed policies (on the board) | `~/ArduinoApps/APP/data/quay-policies/` (models, stock originals, `state.json`) |

| Environment variable | Effect |
|---|---|
| `QUAY_HOME` | Use another config folder |
| `QUAY_BOARD` | Default board for the CLI (overridden by `--board`) |
| `QUAY_GIT_TOKEN` | Token for private HTTPS repositories (SSH URLs use your ssh-agent) |
| `QUAY_ADB` | Path to `adb` |

## Building from source

Requires Go 1.26.

```bash
make            # GUI + CLI for this machine → dist/quay-<os>-<arch>
make cli        # CLI only, no cgo
make app        # macOS: dist/Quay.app for this Mac
make test       # vet, unit tests, frontend syntax check
make e2e        # end-to-end tests against a fake UNO Q in Docker (sshd, sudo, a stub arduino-app-cli)
make preview    # the UI with a mock backend: http://127.0.0.1:8765/?mock (add &os=windows or &os=linux)
make release    # everything below + CLI binaries for six platforms + SHA256SUMS
make licenses   # regenerate the third-party license list after changing dependencies
make mac        # universal .dmg and .zip (macOS with Xcode command-line tools)
make windows    # Windows zips, cross-compiled from any OS (no cgo needed)
make linux      # .deb and .tar.gz for amd64 and arm64, built in Debian 12 containers (Docker)
```

A native Linux GUI build needs `libgtk-3-dev` and `libwebkit2gtk-4.1-dev`. Without cgo, or with `-tags nogui`, Quay builds as a pure-Go CLI.

### Layout

```text
main.go                 GUI when started without arguments, CLI otherwise
internal/board          SSH and adb transports, host keys, prompts
internal/discover       finding boards on the network (mDNS, no extra dependencies)
internal/relay          the download relay's HTTP proxy
internal/mirror         the mirrors in mainland China and how a speed test ranks them
internal/source         git fetch (go-git) and local folders → an upload bundle
internal/remote         the small bash/python scripts run on the board
internal/core           deploy, update, lifecycle, backup/restore, erase, prepare, mirror
internal/profile        quay.yaml parsing and built-in profiles (profiles/*.yaml)
internal/cli            the command line (cobra)
internal/legal          copyright notice and the generated third-party license list (embedded)
internal/gui            the Wails service and the frontend (plain HTML/CSS/JS, no build step)
test/fakeboard, test/e2e  a Docker "UNO Q" and the end-to-end suite
build/                  icons, Info.plist, Windows resources, Linux packaging
```

## License

Quay is licensed under the [Apache License 2.0](LICENSE). Copyright 2026 Xiaogang Cao.

It includes third-party software under their own licenses (MIT, BSD, ISC and Apache-2.0). The full list ships with every package as `THIRD_PARTY_LICENSES.txt`, and `quay licenses` (or **About** in the app) shows it. After changing dependencies, run `make licenses`. A test fails if the list is out of date.

---

Arduino® and UNO™ are trademarks of Arduino S.r.l. Quay is an independent project and is not affiliated with Arduino.
