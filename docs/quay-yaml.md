# `quay.yaml` reference

`quay.yaml` is an optional file at the root of an Arduino App, next to `app.yaml`. It tells Quay about the few things a generic deploy cannot know: a one-time system change, a step that must run before the app starts, how to tell the app is up, when it is unsafe to stop it, where its web console lives, and what to clean up when it is erased.

Apps without a `quay.yaml` are deployed generically: upload, keep `data/`, start with `arduino-app-cli`.

Every section is optional. Unknown keys are an error, so typos do not pass silently.

```yaml
id: weather                     # short identifier (used by built-in profiles)
name: Weather Station           # display name of the profile
description: BME280 charts in the browser.
icon: 🌦️                        # an emoji, or inline <svg> markup (see below)
folder: weather_station         # default install folder under ~/ArduinoApps

system:
  description: Enables I²C bus 2 for the Qwiic connector. Needs a reboot to take effect.
  description_zh: 打开 Qwiic 接口用的 I²C 总线 2，重启后生效。
  check: test -e /dev/i2c-2
  apply: bash tools/enable-i2c.sh
  revert: bash tools/enable-i2c.sh --undo
  stop_app: true

before_start:
  run: python3 tools/patch-libs.py
  description: Patches the sensor library's buffer size.
  resolve_on_failure: true
  rebuild_when_changed:
    - .arduino15/internal/SomeLib_*/SomeLib/src/config.h

health:
  url: http://127.0.0.1:7000/healthz
  timeout: 180

safety:
  url: http://127.0.0.1:7000/api/state
  field: busy
  message: A measurement is being recorded. Stop it in the web UI first.

  also:
    - field: mode
      equals: [calibrate]
      message: Calibration is running. Finish it in the web UI first.

web:
  port: 7000
  path: /

actions:
  - label: Calibration
    label_zh: 标定
    url: /calibrate

uninstall: |
  rm -f "$HOME/.config/weather/cache.db"
```

## Where Quay looks

1. `quay.yaml` in the app being deployed (or already on the board).
2. A built-in profile whose `match.repos` contains the app's repository URL.
3. A built-in profile whose `match.app_names` contains the `name:` from `app.yaml`.

The first hit wins. `quay profiles` lists the built-in profiles. They are published in [quay-profiles](https://github.com/caoxg/quay-profiles) (`apps/`), separately from Quay, and exist only for apps that do not ship their own file yet.

## How hooks run

| Hook | Runs as | Working directory | When |
|---|---|---|---|
| `system.check` | login user | home | on `status`, after deploy, before and after `prepare` |
| `system.apply` | root (`sudo`) | app folder | `quay prepare APP` |
| `system.revert` | root (`sudo`) | app folder | `quay prepare APP --revert`, `quay erase APP --revert-system` |
| `before_start.run` | login user | app folder | after every deploy or update, before the app starts |
| `uninstall` | login user | app folder | `quay erase APP`, before the folder is deleted |

Each hook is a bash script (`bash -c`). It can span several lines with YAML's `|` block. `HOME` is the login user's home (usually `/home/arduino`). A non-zero exit fails the step and is shown with the hook's output.

For `sudo`, Quay tries passwordless sudo first, then the SSH login password, then asks the user. The script is uploaded to a temporary file and runs with `sudo bash FILE`, so it never appears in the process list with its contents.

## Sections

### `icon`

Shown on the app's card, in the catalog and in the install preview, in place of the `icon` from `app.yaml`. Either an emoji, or a small SVG written inline:

```yaml
icon: |
  <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">
    <circle cx="32" cy="32" r="24" fill="#f6c700"/>
  </svg>
```

The SVG is drawn as an image (scripts and external references in it are ignored) and may be up to 16 KB. Use a square `viewBox`; Quay draws it at about 36 px, so keep it simple.

### `system`

A change outside the app folder that needs root and is made once per board, such as a systemd drop-in, a device-tree overlay or a udev rule. Quay never applies it on its own. It shows **System setup needed** on the app card and in `quay status`, and the user applies it with `quay prepare APP`.

| Key | Required | Meaning |
|---|---|---|
| `description` | recommended | One or two sentences shown before the user confirms. Say what changes and what else it affects. |
| `check` | recommended | Exits 0 when the change is already in place. Runs as the user, so keep it read-only. Without it, Quay cannot show whether setup is needed. |
| `apply` | yes | Makes the change. Must be idempotent. |
| `revert` | recommended | Undoes it. Without it, `--revert` is not offered. |
| `stop_app` | no | Stop the app before `apply`/`revert` and start it again afterwards (for example when the app holds the serial port being reconfigured). |

### `before_start`

Something the app needs after its files are in place and before it starts. A typical case is patching a library that App CLI downloads for the sketch.

| Key | Meaning |
|---|---|
| `run` | The script. Must be idempotent: it runs on every deploy. |
| `resolve_on_failure` | If `run` fails, start and stop the app once, then retry. App CLI downloads the libraries named in `sketch.yaml` the first time an app is built, so a hook that patches them can only succeed after that. |
| `rebuild_when_changed` | Globs relative to `$HOME`. Quay fingerprints these files before and after `run`. If they changed, it clears the app's sketch build cache so the MCU firmware is rebuilt against the new sources on the next start. |
| `description`, `description_zh` | One sentence saying what the hook does. Quay shows it in the install preview and on the app's profile chip. `system` takes the same `description_zh`. |

### `health`

How Quay knows the app is really up after starting it, beyond App CLI reporting `running`.

| Key | Meaning |
|---|---|
| `url` | An HTTP URL fetched **on the board** (so `127.0.0.1` means the board). Any 2xx answer means healthy. |
| `timeout` | Seconds to wait. Default 120. Include the time a first build takes. |

Without `health`, Quay waits for App CLI to report `running`, and fails early if it reports `failed`.

### `safety`

Protects hardware from being stopped at a bad moment. Before Quay stops, updates, restores or erases the app, it fetches `url` on the board. If the JSON field `field` is truthy (`true`, a non-zero number, or a non-empty string other than `"0"`/`"false"`), it refuses and shows `message`. `--force` (CLI) or **Force** (app) overrides it.

If the URL does not answer, the app is assumed to be stopped and the check passes.

| Key | Required | Meaning |
|---|---|---|
| `url` | yes | JSON endpoint on the board |
| `field` | yes | Top-level field to test |
| `message` | no | What the user should do instead |
| `also` | no | More reasons to refuse, each `{field, equals, message}`: the field is truthy, or (with `equals`) one of those values. For example, a mode that holds the motors without reporting them as enabled. |

### `web`

Where the app's own web console is, for the **Console** button and `quay open`. The URL is `http://<board address>:<port><path>`.

| Key | Meaning |
|---|---|
| `port` | TCP port |
| `path` | Path, default `/` |

Without `web`, Quay uses the first entry of `ports:` in `app.yaml`, which is usually right.

### `uninstall`

Cleans up anything the app put outside its own folder (for example restoring files a `before_start` hook patched). It runs before Quay deletes the folder. App CLI's containers and caches are removed separately (`arduino-app-cli app destroy`).

Give it as a plain script, or as `{run, description, description_zh}` to say in the install preview what it does.

### `actions`

Shortcuts on the app card and in its ⋯ menu, shown while the app runs: pages of its web console, or other URLs.

| Key | Meaning |
|---|---|
| `label`, `label_zh` | Button text |
| `url` | A path such as `/calibrate.html`, resolved against the app's web console (`web`, or the first `ports` entry), or a full `http(s)://` URL where `{host}` stands for the board's address |
| `description` | Tooltip |

### `policies`

Model files (ONNX policies) the app loads, so users can install models other people trained in their place (`quay policy`, the **Policies** button).

```yaml
policies:
  dir: python                  # where the app loads its models from, relative to the app folder
  input: [1, 61]               # shape every model must have (-1 matches any size)
  output: [1, 14]
  match_metadata: [joint_names, observation_names]   # must equal the stock model's values
  index:                       # the community index for this app (optional)
    url: https://example.com/policies/index.yaml
    mirrors: [https://cdn.example.com/policies/index.yaml]
  slots:
    - id: walk
      file: walk.onnx
      name: Walk
      name_zh: 行走
      description: Default walking model.
      require_metadata: [default_joint_pos]          # keys a model for this slot must carry
```

| Key | Meaning |
|---|---|
| `dir` | Folder the app loads models from. A folder **outside** `data/` holds the stock models: Quay copies the stock file to `data/quay-policies/stock/` before replacing it, and puts installed models back after every deploy, before the app starts. A folder **under** `data/` is an overlay the app reads before its stock models: Quay only writes and deletes files there, and `stock_dir` names where the stock models are (for the metadata comparison). |
| `input`, `output` | Required shapes of the first graph input and output. |
| `match_metadata` | ONNX metadata keys whose values must equal the stock model's in the same slot. |
| `index` | Policy index for this app. Quay reads it in addition to the user's own indexes. |
| `slots` | `id` (a-z, 0-9, `-`, `_`), `file` (a file name in `dir`), display `name`/`name_zh`, `description`, `require_metadata`. |

Quay restarts the app to load a new model and waits for `health`. If the app does not come up, the previous model is put back. The index format is described in [awesome-xgo-duck/policies](https://github.com/caoxg/awesome-xgo-duck/blob/main/policies/README.md).

### Catalog keys

Built-in profiles can also offer an app in the **Custom** tab of the app's **Install** dialog:

```yaml
source:
  repo: https://github.com/owner/repo
  ref: main            # optional
  subdir: apps/thing   # optional, when the app is not at the repository root
match:
  repos: [github.com/owner/repo]
  app_names: [Thing]
```

`source` and `match` are only meaningful in built-in profiles.

## A complete example

The built-in XGO-Duck profile, [`apps/xgoduck.yaml`](https://github.com/caoxg/quay-profiles/blob/main/apps/xgoduck.yaml) in quay-profiles, uses every section:

- `system` moves the Linux↔MCU router link to 2 Mbps.
- `before_start` patches the pinned RouterBridge and RPClite libraries and forces a firmware rebuild when they change.
- `policies` declares the five models in `python/` (walk, roller, get up, pick, roulade) and the community index in [awesome-xgo-duck](https://github.com/caoxg/awesome-xgo-duck).
- `health` and `safety` use the runtime's `/api/status`: Quay refuses to stop the duck while its servos hold torque.
- `web` points at the duck's console on port 9527.
- `uninstall` restores the original library files.
