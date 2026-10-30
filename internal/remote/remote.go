// Package remote holds the small scripts Quay runs on the board. Go decides what to do; these
// only touch files and call arduino-app-cli, so they stay short and testable.
package remote

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/caoxg/quay/internal/board"
)

// Prelude is prepended to every bash script run as the login user.
const Prelude = `set -euo pipefail
export HOME="$(getent passwd "$(id -un)" | cut -d: -f6)"
APPS="$HOME/ArduinoApps"
fail() { echo "$*" >&2; exit 1; }
`

// Script renders prelude + variables + body. Values are shell-quoted.
func Script(body string, vars map[string]string) string {
	var b strings.Builder
	b.WriteString(Prelude)
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, board.Quote(vars[k]))
	}
	b.WriteString(body)
	return b.String()
}

// RootScript is a script for sudo: no HOME games, variables, body.
func RootScript(body string, vars map[string]string) string {
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, board.Quote(vars[k]))
	}
	b.WriteString(body)
	return b.String()
}

// WithChecks prepends to a status/network script the board files a system profile asks about:
// which must be checked for existence, and which JSON keys to read (see sysFiles).
func WithChecks(script string, exists []string, keys map[string][]string) string {
	b, _ := json.Marshal(map[string]any{"exists": exists, "keys": keys})
	return "QUAY_CHECKS = " + strconv.Quote(string(b)) + "\n" + script
}

// sysFiles answers WithChecks: {path: {"exists": bool, "json": {key: value}}}.
const sysFiles = `
def sys_files():
    import json, os
    try:
        c = json.loads(globals().get("QUAY_CHECKS") or "{}")
    except Exception:
        c = {}
    res = {}
    for p in c.get("exists") or []:
        res[p] = {"exists": os.path.exists(p)}
    for p, keys in (c.get("keys") or {}).items():
        r = res.setdefault(p, {"exists": os.path.exists(p)})
        try:
            if os.path.getsize(p) <= 65536:
                with open(p) as f:
                    d = json.load(f)
                if isinstance(d, dict):
                    r["json"] = {k: d[k] for k in keys if isinstance(d.get(k), (str, int, float, bool))}
        except Exception:
            pass
    return res
`

// Status prints one JSON document describing the board and its user apps (python3 on the board).
var Status = sysFiles + `
import base64, glob, json, os, pathlib, pwd, socket, subprocess

def sh(*cmd, timeout=20):
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        return r.returncode, r.stdout, r.stderr
    except Exception as e:
        return 127, "", str(e)

def read(p, strip="\x00\n "):
    try:
        return pathlib.Path(p).read_text(errors="replace").strip(strip)
    except OSError:
        return ""

home = pwd.getpwuid(os.getuid()).pw_dir
apps_dir = os.path.join(home, "ArduinoApps")
out = {
    "user": pwd.getpwuid(os.getuid()).pw_name,
    "home": home,
    "hostname": socket.gethostname(),
    "model": read("/proc/device-tree/model") or read("/sys/firmware/devicetree/base/model"),
    "compatible": read("/sys/firmware/devicetree/base/compatible").replace("\x00", " ").split(),
    "serial": read("/sys/devices/soc0/serial_number") or read("/sys/firmware/devicetree/base/serial-number"),
    "bootId": read("/proc/sys/kernel/random/boot_id"),
    "ips": [],
    "appcli": False, "appcliVersion": "", "appList": None, "appListError": "",
    "pipIndex": "", "uvIndex": "", "mirrorManaged": False,
    "folders": [],
}
osr = {}
for line in read("/etc/os-release", "\n ").splitlines():
    k, _, v = line.partition("=")
    osr[k] = v.strip().strip('"')
out["os"] = {"id": osr.get("ID", ""), "name": osr.get("NAME", ""), "pretty": osr.get("PRETTY_NAME", ""),
             "version": read("/etc/debian_version") or osr.get("VERSION_ID", ""), "codename": osr.get("VERSION_CODENAME", "")}
rc, o, _ = sh("ip", "-j", "-4", "address")
try:
    for row in json.loads(o or "[]"):
        if row.get("ifname") in ("lo",) or row.get("ifname", "").startswith(("docker", "br-", "veth")):
            continue
        for a in row.get("addr_info", []):
            out["ips"].append({"ifname": row.get("ifname"), "ip": a.get("local")})
except Exception:
    pass
rc, o, e = sh("sh", "-c", "command -v arduino-app-cli")
if rc == 0:
    out["appcli"] = True
    rc, o, e = sh("arduino-app-cli", "version")
    out["appcliVersion"] = (o or e).strip().splitlines()[0] if (o or e).strip() else ""
    rc, o, e = sh("arduino-app-cli", "app", "list", "--format", "json", timeout=60)
    if rc == 0:
        try:
            data = json.loads(o)
            for a in data.get("apps", []) or []:
                try:
                    raw = a.get("id", "")
                    a["decodedId"] = base64.urlsafe_b64decode(raw + "=" * (-len(raw) % 4)).decode()
                except Exception:
                    a["decodedId"] = ""
            out["appList"] = data
        except Exception as ex:
            out["appListError"] = "unparseable app list: %s" % ex
    else:
        out["appListError"] = (e or o).strip()[-500:]
for line in read("/etc/pip.conf", "\n ").splitlines():
    if line.strip().startswith("index-url"):
        out["pipIndex"] = line.split("=", 1)[1].strip()
    elif line.strip().startswith("extra-index-url"):
        out["pipExtra"] = line.split("=", 1)[1].strip()
for line in read("/etc/uv/uv.toml", "\n ").splitlines():
    if line.strip().startswith("url"):
        out["uvIndex"] = line.split("=", 1)[1].strip().strip('"')
out["mirrorManaged"] = any(os.path.exists(p) for p in ("/etc/pip.conf.quay-orig", "/etc/pip.conf.quay-none"))
deb, fallback = "", []
for p in ["/etc/apt/sources.list"] + sorted(glob.glob("/etc/apt/sources.list.d/*.list") + glob.glob("/etc/apt/sources.list.d/*.sources")):
    for line in read(p, "\n ").splitlines():
        if line.strip().startswith("#"):
            continue
        for tok in line.split():
            tok = tok.rstrip("/")
            if deb:
                break
            if tok.startswith("mirror+file:") and tok.endswith("/debian.list"):
                ms = [l.split()[0].rstrip("/") for l in read(tok[len("mirror+file:"):], "\n ").splitlines() if l.strip()]
                if ms:
                    deb, fallback = ms[0], ms[1:]
            elif tok.startswith(("http://", "https://")) and tok.endswith("/debian"):
                deb = tok
out["debianMirror"] = deb
out["debianFallback"] = fallback
keys = []
for line in read(os.path.join(home, ".ssh", "authorized_keys"), "\n ").splitlines():
    for tok in line.split():
        if tok.startswith("AAAA"):
            keys.append(tok)
            break
out["authorizedKeys"] = keys
out["debianManaged"] = os.path.isdir("/var/lib/quay/apt-orig")
out["relay"] = os.path.exists("/etc/apt/apt.conf.d/80quay-relay")
try:  # the hook asks the port whether it is Quay's relay (Quay newer than 2026-10-28)
    out["relayProbe"] = "quay-relay.invalid" in open("/usr/local/lib/quay/apt-proxy").read()
except OSError:
    out["relayProbe"] = False
# First setup (App Lab's, or Quay's over USB): SSH on, and a usable password for the user
# (passwd -S on one's own account: "arduino P …"; L locked, NP none).
_, o, _ = sh("systemctl", "is-active", "ssh")
_, o2, _ = sh("systemctl", "is-enabled", "ssh")
rc, o3, _ = sh("passwd", "-S")
out["setup"] = {"sshActive": o.strip() == "active", "sshEnabled": o2.strip() == "enabled",
                "password": o3.split()[1] if rc == 0 and len(o3.split()) > 1 else ""}

def nm_values(*args):
    rc, o, _ = sh("nmcli", "-t", *args, timeout=10)
    res = {}
    for line in (o if rc == 0 else "").splitlines():
        k, _, v = line.partition(":")
        res[k] = v.replace("\\:", ":").replace("\\\\", "\\")
    return res
nv = nm_values("-f", "GENERAL.STATE,GENERAL.CONNECTION,GENERAL.CON-UUID", "device", "show", "wlan0")
if nv:
    st = nv.get("GENERAL.STATE", "").split(" ")[0]
    net = {"state": int(st) if st.isdigit() else 0, "connection": nv.get("GENERAL.CONNECTION", ""), "ssid": "", "hotspot": False,
           "away": "off"}
    try:
        with open("/etc/quay/network.json") as f:
            a = json.load(f).get("away") or "off"
            net["away"] = "board" if a == "own" else a
    except Exception:
        pass
    if nv.get("GENERAL.CON-UUID", "--") not in ("", "--"):
        cv = nm_values("-f", "802-11-wireless.mode,802-11-wireless.ssid", "connection", "show", "uuid", nv["GENERAL.CON-UUID"])
        net["hotspot"] = cv.get("802-11-wireless.mode") == "ap"
        net["ssid"] = cv.get("802-11-wireless.ssid", "")
    net["ours"] = net["hotspot"] and net["connection"] == "quay-hotspot"
    # Saved Wi-Fi profiles, for system profiles (e.g. another tool's hotspot that comes up at boot).
    profs = []
    rc, o, _ = sh("nmcli", "-t", "-f", "UUID,TYPE,AUTOCONNECT,ACTIVE,NAME", "connection", "show", timeout=10)
    for line in (o if rc == 0 else "").splitlines():
        parts = line.split(":", 4)
        if len(parts) < 5 or parts[1] not in ("802-11-wireless", "wifi"):
            continue
        cv = nm_values("-f", "802-11-wireless.mode,802-11-wireless.ssid", "connection", "show", "uuid", parts[0])
        profs.append({"uuid": parts[0], "id": parts[4].replace("\\:", ":").replace("\\\\", "\\"), "ssid": cv.get("802-11-wireless.ssid", ""),
                      "mode": cv.get("802-11-wireless.mode", "") or "infrastructure", "autoconnect": parts[2] == "yes", "active": parts[3] == "yes"})
    out["nmProfiles"] = profs
    if net["ours"]:
        try:
            with open("/run/quay-hotspot/state.json") as f:
                net["stations"] = json.load(f).get("stations")
        except Exception:
            pass
    out["net"] = net
if os.path.isdir(apps_dir):
    for name in sorted(os.listdir(apps_dir)):
        d = os.path.join(apps_dir, name)
        if not os.path.isfile(os.path.join(d, "app.yaml")):
            continue
        f = {"folder": name, "path": d, "quay": None, "quayYaml": "", "appYaml": read(os.path.join(d, "app.yaml"), "\n"), "data": []}
        meta = read(os.path.join(d, ".quay.json"), "\n ")
        if meta:
            try:
                f["quay"] = json.loads(meta)
            except Exception:
                pass
        f["quayYaml"] = read(os.path.join(d, "quay.yaml"), "")
        dd = os.path.join(d, "data")
        if os.path.isdir(dd):
            f["data"] = sorted(os.listdir(dd))
        try:
            with open(os.path.join(dd, "quay-policies", "state.json")) as pf:
                f["policies"] = json.load(pf).get("slots") or {}
        except Exception:
            pass
        out["folders"].append(f)
out["sysFiles"] = sys_files()
print(json.dumps(out))
`

// Probe fetches a URL on the board and prints the body, or QUAY_DOWN.
const Probe = `
import sys, urllib.request
try:
    print(urllib.request.urlopen(sys.argv[1], timeout=3).read().decode("utf-8", "replace"))
except Exception:
    print("QUAY_DOWN")
`

// Replace swaps the app code for the uploaded tarball and keeps data/, .cache/ and restore points.
const Replace = `
dest="$APPS/$FOLDER"
stage=$(mktemp -d /tmp/quay-stage.XXXXXX)
trap 'rm -rf "$stage" "$TARBALL"' EXIT
tar --warning=no-unknown-keyword -xf "$TARBALL" -C "$stage"
[ -f "$stage/app.yaml" ] || fail "bundle has no app.yaml"
mkdir -p "$dest"
find "$dest" -mindepth 1 -maxdepth 1 ! -name data ! -name 'data.before-restore-*' ! -name .cache ! -name .quay.json -exec rm -rf {} +
cp -a "$stage/." "$dest/"
printf '%s\n' "$META" > "$dest/.quay.json"
echo "installed $(find "$dest" -type f -not -path "$dest/.cache/*" -not -path "$dest/data/*" | wc -l) files into $dest"
`

// Indexes updates the board's library and platform indexes when the sketch pins a version that is
// neither installed nor listed in them, or prints QUAY_NOSKETCH. The OS image ships the indexes of
// its build day, and App CLI refreshes them only while it updates the system (App Lab's update) or
// adds a library to a sketch. On a board updated through plain apt (Quay's update without the
// desktop packages), or never updated, it cannot download a newer pin, and the sketch compiles
// against the core's stub headers instead ("Please install the Arduino_RouterBridge library").
const Indexes = `
yaml="$APPS/$FOLDER/sketch/sketch.yaml"
[ -f "$yaml" ] || { echo QUAY_NOSKETCH; exit 0; }
unlisted() {
    python3 - "$yaml" <<'EOF'
import glob, json, os, re, sys
text = open(sys.argv[1]).read()
data = os.path.expanduser("~/.arduino15/")

def unlisted(pins, index, entries):
    pins = [p for p in pins if not glob.glob(data + "internal/" + glob.escape("_".join(p).replace(" ", "_")) + "_*")]
    if not pins:
        return []
    try:
        with open(data + index) as f:
            have = entries(json.load(f))
    except (OSError, ValueError, KeyError):
        have = set()
    return [p for p in pins if p not in have]

libs = unlisted(re.findall(r"^\s*-\s+([^:()\n]+?)\s+\(([^)\s]+)\)[ \t]*$", text, re.M), "library_index.json",
                lambda d: {(l["name"], l["version"]) for l in d["libraries"]})
# Platforms from their own index (platform_index_url) are App CLI's to fetch.
cores = unlisted([m[:3] for m in re.findall(r"^\s*-\s+platform:\s*([^:\s]+):(\S+)\s+\(([^)\s]+)\)[ \t]*(?:\n[ \t]+(platform_index_url):)?", text, re.M) if not m[3]],
                 "package_index.json", lambda d: {(p["name"], q["architecture"], q["version"]) for p in d["packages"] for q in p["platforms"]})
if libs:
    print("lib", ", ".join("%s %s" % p for p in libs))
if cores:
    print("core", ", ".join("%s:%s %s" % p for p in cores))
EOF
}
stale=$(unlisted)
[ -n "$stale" ] || { echo "the board's indexes list every library and platform the sketch pins"; exit 0; }
while read -r kind what; do
    echo "the board's $kind index does not list $what yet: updating it"
    arduino-cli "$kind" update-index || fail "could not update the board's $kind index, which the sketch needs for $what"
done <<< "$stale"
stale=$(unlisted)
[ -z "$stale" ] || fail "the sketch pins versions the Arduino indexes do not list: $(cut -d' ' -f2- <<< "$stale" | paste -sd, -)"
`

// RefreshIndexes updates the Arduino platform and library indexes, as App CLI does while it lists
// the system's updates.
const RefreshIndexes = `
arduino-cli core update-index
arduino-cli lib update-index
`

// BeforeStart runs the app's before_start hook and clears the sketch build cache when the files
// listed in GLOBS (relative to $HOME) change, or when the first build had to resolve libraries.
const BeforeStart = `
cd "$APPS/$FOLDER"
fingerprint() {
    local g f
    for g in $GLOBS; do for f in $HOME/$g; do if [ -f "$f" ]; then cat "$f"; fi; done; done | sha256sum | cut -d' ' -f1
}
before=$(fingerprint); rebuild=0
if ! out=$(bash -c "$RUN" 2>&1); then
    printf '%s\n' "$out"
    [ "$RESOLVE" = 1 ] || fail "before_start hook failed"
    echo "first build: letting App CLI resolve the sketch libraries, then retrying"
    arduino-app-cli app start "$PWD" || true
    arduino-app-cli app stop "$PWD" || true
    out=$(bash -c "$RUN" 2>&1) || { printf '%s\n' "$out"; fail "before_start hook failed"; }
    rebuild=1
fi
printf '%s\n' "$out"
[ "$before" = "$(fingerprint)" ] || rebuild=1
if [ "$rebuild" = 1 ]; then rm -rf .cache/sketch; echo "QUAY_REBUILD"; fi
`

// Backup packs data/ with a metadata file into OUT, or prints QUAY_EMPTY.
const Backup = `
cd "$APPS/$FOLDER" 2>/dev/null || fail "app $FOLDER is not installed"
if [ ! -d data ] || [ -z "$(ls -A data)" ]; then echo QUAY_EMPTY; exit 0; fi
work=$(mktemp -d /tmp/quay-backup.XXXXXX)
trap 'rm -rf "$work"' EXIT
cp -a data "$work/data"
printf '%s\n' "$META" > "$work/quay-backup.json"
tar -czf "$OUT" -C "$work" data quay-backup.json
echo QUAY_OK
`

// Restore puts data/ from TARBALL back, keeping the current data/ as a restore point.
const Restore = `
dest="$APPS/$FOLDER"
[ -d "$dest" ] || fail "app $FOLDER is not installed"
work=$(mktemp -d /tmp/quay-restore.XXXXXX)
trap 'rm -rf "$work" "$TARBALL"' EXIT
tar --warning=no-unknown-keyword -xzf "$TARBALL" -C "$work"
[ -d "$work/data" ] || fail "not a Quay backup (no data/ folder)"
python3 - "$work/data" <<'PY' || fail "the backup contains invalid JSON files"
import json, pathlib, sys
for p in pathlib.Path(sys.argv[1]).rglob("*.json"):
    json.loads(p.read_text())
PY
if [ -d "$dest/data" ]; then mv "$dest/data" "$dest/data.before-restore-$(date +%Y%m%d-%H%M%S)"; fi
mv "$work/data" "$dest/data"
echo "restored: $(ls -A "$dest/data" | tr '\n' ' ')"
`

// CheckAll runs each "folder<TAB>check" line's check and prints folder<TAB>0|1.
const CheckAll = `
while IFS=$'\t' read -r folder check; do
    [ -n "$folder" ] || continue
    if (cd "$APPS/$folder" 2>/dev/null && bash -c "$check") >/dev/null 2>&1; then echo "$folder	1"; else echo "$folder	0"; fi
done <<< "$CHECKS"
`

// Uninstall runs an app's uninstall hook from its folder.
const Uninstall = `
cd "$APPS/$FOLDER"
bash -c "$RUN"
`

// RemoveFolder deletes an app folder (guarded).
const RemoveFolder = `
case "$FOLDER" in ""|.|..|*/*) fail "refusing to remove '$FOLDER'";; esac
rm -rf -- "$APPS/$FOLDER"
`

// MirrorOn points pip and uv at INDEX, remembering the original files once (root).
// INDEX_FALLBACK becomes pip's extra index (pip carries on when one index fails).
//
// With DEBIAN set it points apt's Debian sources (deb.debian.org) at apt mirror lists
// (mirror+file:): DEBIAN first, then DEBIAN_FALLBACK (space-separated), then the official server —
// apt moves to the next one by itself when a mirror fails. The originals are kept under
// /var/lib/quay/apt-orig, outside sources.list.d where apt would complain about the extra files.
const MirrorOn = `
if [ -n "${INDEX:-}" ]; then
    mkdir -p /etc/uv
    for f in /etc/pip.conf /etc/uv/uv.toml; do
        if [ ! -e "$f.quay-orig" ] && [ ! -e "$f.quay-none" ]; then
            if [ -f "$f" ]; then cp -p "$f" "$f.quay-orig"; else : > "$f.quay-none"; fi
        fi
    done
    printf '[global]\nindex-url = %s\n' "$INDEX" > /etc/pip.conf
    if [ -n "${INDEX_FALLBACK:-}" ]; then printf 'extra-index-url = %s\n' "$INDEX_FALLBACK" >> /etc/pip.conf; fi
    printf '[[index]]\nurl = "%s"\ndefault = true\n' "$INDEX" > /etc/uv/uv.toml
    echo "pip and uv now use $INDEX${INDEX_FALLBACK:+ (pip falls back to $INDEX_FALLBACK)}"
fi
if [ -n "${DEBIAN:-}" ]; then
    B=/var/lib/quay/apt-orig
    L=/etc/apt/quay
    mkdir -p "$L"
    : > "$L/debian.list.new"
    : > "$L/debian-security.list.new"
    seen=" "
    i=1
    for m in $DEBIAN ${DEBIAN_FALLBACK:-} http://deb.debian.org/debian; do
        m=${m%/}
        case "$seen" in *" $m "*) continue ;; esac
        seen="$seen$m "
        printf '%s\tpriority:%d\n' "$m" "$i" >> "$L/debian.list.new"
        printf '%s\tpriority:%d\n' "$m-security" "$i" >> "$L/debian-security.list.new"
        i=$((i + 1))
    done
    mv -f "$L/debian.list.new" "$L/debian.list"
    mv -f "$L/debian-security.list.new" "$L/debian-security.list"
    chmod 644 "$L/debian.list" "$L/debian-security.list"
    for f in /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do
        [ -f "$f" ] || continue
        if [ ! -e "$B$f" ]; then
            grep -q 'deb\.debian\.org' "$f" || continue
            mkdir -p "$B$(dirname "$f")"
            cp -p "$f" "$B$f"
        fi
        sed -E -e "s#https?://deb\.debian\.org/debian-security/?#mirror+file:$L/debian-security.list#g" \
               -e "s#https?://deb\.debian\.org/debian/?#mirror+file:$L/debian.list#g" "$B$f" > "$f"
        echo "$f now uses $(echo $DEBIAN), then: $(cut -f1 "$L/debian.list" | tail -n +2 | tr '\n' ' ')"
    done
fi
`

// MirrorOff restores what MirrorOn replaced (root).
const MirrorOff = `
for f in /etc/pip.conf /etc/uv/uv.toml; do
    if [ -e "$f.quay-orig" ]; then mv -f "$f.quay-orig" "$f"; echo "restored $f"
    elif [ -e "$f.quay-none" ]; then rm -f "$f" "$f.quay-none"; echo "removed $f"; fi
done
B=/var/lib/quay/apt-orig
if [ -d "$B" ]; then
    find "$B" -type f | while read -r o; do
        f=${o#"$B"}
        cp -p "$o" "$f" && echo "restored $f"
    done
    rm -rf "$B"
fi
rm -rf /etc/apt/quay
`

// MirrorTest measures, from the board, how each mirror in argv[1] (base64 JSON) serves it: the
// Debian release files (with security updates) and download speed on the package index, and the
// PyPI simple index. One JSON line per mirror, as each finishes.
const MirrorTest = `
import base64, json, subprocess, sys, time, urllib.request

cfg = json.loads(base64.b64decode(sys.argv[1]))

def os_release():
    d = {}
    try:
        for line in open("/etc/os-release"):
            k, _, v = line.strip().partition("=")
            d[k] = v.strip('"')
    except OSError:
        pass
    return d

code = os_release().get("VERSION_CODENAME") or "trixie"
try:
    arch = subprocess.run(["dpkg", "--print-architecture"], capture_output=True, text=True).stdout.strip() or "arm64"
except Exception:
    arch = "arm64"
seconds = float(cfg.get("seconds", 2.5))
UA = {"User-Agent": "Quay-mirror-test/1"}

def fetch(url, budget=0):
    t0 = time.monotonic()
    with urllib.request.urlopen(urllib.request.Request(url, headers=UA), timeout=6) as r:
        ttfb = time.monotonic() - t0
        n, t1 = 0, time.monotonic()
        while True:
            if budget and (n >= budget or time.monotonic() - t1 >= seconds):
                break
            b = r.read(65536)
            if not b:
                break
            n += len(b)
            if not budget and n > (1 << 20):
                break
        return ttfb, n, max(time.monotonic() - t1, 1e-3)

def why(e):
    return str(getattr(e, "reason", "") or e)[:160]

for m in cfg["mirrors"]:
    out = {"id": m["id"]}
    if m.get("debian"):
        d = m["debian"].rstrip("/")
        try:
            ttfb, _, _ = fetch(d + "/dists/" + code + "/Release")
            try:
                fetch(d + "-security/dists/" + code + "-security/Release")
                sec = True
            except Exception:
                sec = False
            _, n, dt = fetch(d + "/dists/" + code + "/main/binary-" + arch + "/Packages.xz", budget=32 << 20)
            out["debian"] = {"ok": True, "ms": round(ttfb * 1000), "speed": round(n / dt), "security": sec}
        except Exception as e:
            out["debian"] = {"ok": False, "error": why(e)}
    if m.get("pypi"):
        p = m["pypi"].rstrip("/")
        try:
            ttfb, n, _ = fetch(p + "/pip/")
            out["pypi"] = {"ok": n > 0, "ms": round(ttfb * 1000)}
        except Exception as e:
            out["pypi"] = {"ok": False, "error": why(e)}
    print(json.dumps(out), flush=True)
`

// UploadKey appends the public key KEY (whose base64 part is BLOB) to the login user's
// authorized_keys, unless it is there already.
const UploadKey = `
umask 077
mkdir -p "$HOME/.ssh"
touch "$HOME/.ssh/authorized_keys"
if grep -qF "$BLOB" "$HOME/.ssh/authorized_keys"; then
    echo QUAY_PRESENT
else
    printf '%s\n' "$KEY" >> "$HOME/.ssh/authorized_keys"
    echo QUAY_ADDED
fi
chmod 700 "$HOME/.ssh"
chmod 600 "$HOME/.ssh/authorized_keys"
`

// RelayOn installs the apt hook for the relay: while Quay is connected it forwards 127.0.0.1:PORT
// on the board to a proxy on its computer, and apt downloads through it; otherwise apt connects
// directly as before. Proxy-Auto-Detect runs the script for every download host.
const RelayOn = `
mkdir -p /usr/local/lib/quay
cat > /usr/local/lib/quay/apt-proxy <<EOF
#!/bin/bash
# Installed by Quay ("quay relay off" removes it). While Quay is connected to this board it
# forwards 127.0.0.1:$PORT to its computer, and apt downloads through there; otherwise directly.
# Only when Quay's relay answers the probe ($PROBE): a port held by anything else (a connection an
# earlier Quay left behind, which sshd has not noticed yet) would fail every download.
if (exec 3<>/dev/tcp/127.0.0.1/$PORT && printf 'GET http://$PROBE/ HTTP/1.0\r\n\r\n' >&3 &&
    IFS= read -r -t 3 line <&3 && [[ \$line == "HTTP/1."?" 200"* ]]) 2>/dev/null; then
    echo "http://127.0.0.1:$PORT"
else
    echo DIRECT
fi
EOF
chmod 755 /usr/local/lib/quay/apt-proxy
cat > /etc/apt/apt.conf.d/80quay-relay <<'EOF'
// Installed by Quay ("quay relay off" removes it): download through the computer running Quay
// while it is connected to this board.
Acquire::http::Proxy-Auto-Detect "/usr/local/lib/quay/apt-proxy";
Acquire::https::Proxy-Auto-Detect "/usr/local/lib/quay/apt-proxy";
EOF
echo "apt downloads through Quay's computer whenever Quay is connected"
`

// RelayOff removes RelayOn's hook.
const RelayOff = `
rm -f /etc/apt/apt.conf.d/80quay-relay /usr/local/lib/quay/apt-proxy
rmdir /usr/local/lib/quay 2>/dev/null || true
echo "apt downloads directly again"
`

// SetName renames the board the way App Lab does (the image lets it call hostnamectl via sudo
// without a password).
const SetName = `
arduino-app-cli system set-name "$NAME"
`

// EnableSSH turns the SSH server on the way App Lab's first setup does, with the commands the image
// lets its user run without a password (/etc/sudoers.d/20-arduino-ssh): host keys first when the
// image has none, then ssh and avahi-daemon (NAME.local) enabled and started. It prints the host's
// public keys, for Quay to trust (they arrive over the connection already in use, USB say).
const EnableSSH = `
ls /etc/ssh/ssh_host_*_key >/dev/null 2>&1 || sudo -n /usr/sbin/dpkg-reconfigure openssh-server >/dev/null
systemctl is-enabled --quiet ssh || sudo -n /usr/bin/systemctl enable ssh
systemctl is-active --quiet ssh || sudo -n /usr/bin/systemctl start ssh
systemctl is-active --quiet ssh || { echo "the SSH server did not start" >&2; exit 1; }
systemctl is-enabled --quiet avahi-daemon || sudo -n /usr/bin/systemctl enable avahi-daemon || true
systemctl is-active --quiet avahi-daemon || sudo -n /usr/bin/systemctl start avahi-daemon || true
cat /etc/ssh/ssh_host_*_key.pub
`

// WaitOnline waits up to $SECS seconds for the board's Wi-Fi to be connected with an address, and
// prints the address.
const WaitOnline = `
end=$(( $(date +%s) + SECS ))
while :; do
  if [ "$(nmcli -g GENERAL.STATE device show wlan0 2>/dev/null | cut -d' ' -f1)" = 100 ]; then
    ip=$(nmcli -g IP4.ADDRESS device show wlan0 2>/dev/null | cut -d' ' -f1 | cut -d/ -f1)
    [ -n "$ip" ] && { echo "$ip"; exit 0; }
  fi
  [ "$(date +%s)" -ge "$end" ] && { echo "the board is not on Wi-Fi after $SECS s" >&2; exit 1; }
  sleep 2
done
`

// RestartAvahi re-announces the board under its current hostname, with the two commands the image
// allows without a password.
const RestartAvahi = `
systemctl is-active --quiet avahi-daemon || exit 0
sudo -n systemctl stop avahi-daemon
sudo -n systemctl start avahi-daemon
`

// aptCache gives the scripts below their own copy of apt's package cache, in $A. apt-get clean,
// which ends every update, deletes the board's, and apt run without root then rebuilds it in
// memory for every command: 5–10 s each on the UNO Q. apt checks the copy against the package
// lists and the installed packages, as it does its own.
const aptCache = `C="$HOME/.cache/quay/apt"
mkdir -p "$C"
A="-o Debug::NoLocking=1 -o Dir::Cache::pkgcache=$C/pkgcache.bin -o Dir::Cache::srcpkgcache=$C/srcpkgcache.bin"
apt-cache $A policy apt >/dev/null 2>&1 || true # builds the copy once, before the commands share it
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
`

// UpgradeList prints the simulated system upgrade ("Inst" lines), the packages' download URIs with
// sizes, and whether a reboot is pending. No root needed.
const UpgradeList = `
export LC_ALL=C
` + aptCache + `
# Sources on a mirror list (Quay's "mirror cn": mirror+file:/etc/apt/quay/NAME.list) download from
# the list's first mirror.
for f in /etc/apt/quay/*.list; do
    [ -e "$f" ] && echo "QUAY_MIRRORLIST $f $(sed -n '1s#^[a-z]*://\([^/[:space:]]*\).*#\1#p' "$f")"
done
# Each apt command takes seconds; those that do not need another's output run side by side.
# --allow-downgrades: -qq implies -y, which refuses the downgrades Arduino's pins ask for.
apt-get --print-uris -qq --allow-downgrades $A dist-upgrade >"$T/uris" 2>/dev/null &
apt-get update --print-uris $A >"$T/lists" 2>/dev/null &
inst=$(apt-get -s $A dist-upgrade 2>/dev/null | grep '^Inst ' || true)
[ -n "$inst" ] && printf '%s\n' "$inst"
# Each package's candidate, the version apt picks for it by priority alone: one that dist-upgrade
# takes to another version (from backports, say) is only pulled in by a package that needs it.
if [ -n "$inst" ] && command -v apt-cache >/dev/null; then
    printf '%s\n' "$inst" | awk '{print $2}' | xargs apt-cache $A policy 2>/dev/null |
        awk '/^[^ ]/ {p = $1; sub(/:$/, "", p)} /^  Candidate:/ {print "QUAY_CAND", p, $2}'
fi
wait
echo QUAY_URIS
cat "$T/uris"
echo QUAY_END
if [ -e /var/run/reboot-required ]; then echo QUAY_REBOOT; fi
# The package lists of the configured sources: how many there are, how many are missing (a source
# just switched to a mirror has none until apt-get update), and the newest repository date among
# them (each InRelease's Date:; apt keeps the server's times, not when it fetched them).
# apt-get update --print-uris names each source's InRelease file, also for a source with no lists.
targets=0 missing=0 newest=0
for f in $(awk '$2 ~ /_InRelease$/ {print $2}' "$T/lists"); do
    targets=$((targets + 1))
    r=/var/lib/apt/lists/$f
    if [ ! -e "$r" ]; then missing=$((missing + 1)); continue; fi
    d=$(sed -n 's/^Date: //p' "$r" | head -1)
    s=$(date -d "$d" +%s 2>/dev/null || echo 0)
    if [ -n "$d" ] && [ "$s" -gt "$newest" ]; then newest=$s; fi
done
echo "QUAY_LISTS $targets $missing $newest"
`

// UpgradePlan works out, for each line "KEY NAME…" of PLANS, what installing those package
// upgrades installs (` + "`apt-get install --only-upgrade`" + `, as an update runs it), all side by side,
// and prints "QUAY_PLAN KEY EXIT-STATUS" with the simulation's "Inst" lines. No root needed.
const UpgradePlan = `
export LC_ALL=C
` + aptCache + `
while read -r key names; do
    [ -n "$key" ] || continue
    { apt-get -s $A install --only-upgrade --allow-downgrades $names >"$T/$key" 2>&1 && echo 0 >"$T/$key.rc" || echo $? >"$T/$key.rc"; } &
done <<<"$PLANS"
wait
while read -r key names; do
    [ -n "$key" ] || continue
    echo "QUAY_PLAN $key $(cat "$T/$key.rc")"
    grep '^Inst ' "$T/$key" || true
done <<<"$PLANS"
`

// UpgradeJob runs CMD detached from the SSH session — a system upgrade must survive a dropped
// Wi-Fi link — and streams its output until it ends. A job that is already running is joined
// instead of starting a second one.
const UpgradeJob = `
D=/tmp/quay-upgrade
mkdir -p "$D"
if [ -f "$D/pid" ] && kill -0 "$(cat "$D/pid")" 2>/dev/null; then
    echo QUAY_ATTACH
else
    rm -f "$D/rc" "$D/pid"
    : > "$D/log"
    printf '%s\n' "${ROUND:-}" > "$D/round" # which round of an update this is, for FollowUpgrade
    touch "$D/started" # PrefsChanged looks for apt preferences newer than this
    # The job is a file of its own: systemd would expand the $ of a command line. CMD may be a list
    # (a && b && c): the group sends all of its output to the log, not the last command's only.
    printf '%s\n' "$CMD" > "$D/cmd"
    cat > "$D/job.sh" <<'JOB'
D=$(dirname "$0")
echo $$ > "$D/pid"
{ eval "$(cat "$D/cmd")"; } > "$D/log" 2>&1
echo $? > "$D/rc"
JOB
    # Under the user's systemd when it runs, so the job belongs to neither connection: adbd, which a
    # USB connection runs under, takes everything it started down with it when it restarts (it does,
    # in the middle of an update). Otherwise in a session of its own.
    export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
    if ! { [ -S "$XDG_RUNTIME_DIR/bus" ] && command -v systemd-run >/dev/null &&
        DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus" systemd-run --user --quiet --collect \
            --unit="quay-upgrade-$$" bash "$D/job.sh" </dev/null >/dev/null 2>&1; }; then
        setsid bash "$D/job.sh" </dev/null >/dev/null 2>&1 &
    fi
    for _ in $(seq 50); do [ -s "$D/pid" ] && break; sleep 0.1; done
    [ -s "$D/pid" ] || fail "the update job did not start"
fi
tail -n +1 -f --pid="$(cat "$D/pid")" "$D/log"
rc=$(cat "$D/rc" 2>/dev/null || echo 1)
exit "$rc"
`

// MarkUpgrade marks the start of an install that does not run as an UpgradeJob (an image that wants
// a password for apt), for PrefsChanged.
const MarkUpgrade = `mkdir -p /tmp/quay-upgrade && touch /tmp/quay-upgrade/started`

// PrefsChanged lists the apt preference files changed since the last install started, each with the
// package that owns it: "path package". A file's ctime is when it was written; dpkg keeps the
// packaged mtime. find exits 1 for a path that does not exist (the UNO Q has no
// /etc/apt/preferences), which pipefail would turn into a failed script that hides every change.
const PrefsChanged = `
D=/tmp/quay-upgrade
[ -f "$D/started" ] || exit 0
{ find /etc/apt/preferences /etc/apt/preferences.d -maxdepth 1 -type f -cnewer "$D/started" 2>/dev/null || true; } | sort | while read -r f; do
    echo "$f $(dpkg -S "$f" 2>/dev/null | head -1 | cut -d: -f1)"
done
`

// UpgradeFollow picks up UpgradeJob's job after the connection dropped (the update restarts the
// board's services, sshd among them): it follows the rest of the job if it still runs, and exits
// with its exit status, showing the end of its output when it failed.
const UpgradeFollow = `
D=/tmp/quay-upgrade
if [ -f "$D/pid" ] && kill -0 "$(cat "$D/pid")" 2>/dev/null; then
    tail -n 0 -f --pid="$(cat "$D/pid")" "$D/log"
fi
rc=$(cat "$D/rc" 2>/dev/null || echo 1)
[ "$rc" = 0 ] || tail -n 5 "$D/log"
exit "$rc"
`

// SystemHook runs a profile's system apply/revert as root from the app folder.
const SystemHook = `
cd "$APPDIR"
bash -c "$RUN"
`

// Policies manages the models an app loads (python3 on the board; argv[1] is base64 JSON with op,
// app, dir, overlay, slots, …). Installed models live in the app's data/quay-policies/ (models/ by
// SHA-256, stock/ for the replaced stock files, state.json), so deploys and backups keep them.
const Policies = `
import base64, hashlib, json, os, shutil, sys, time
req = json.loads(base64.b64decode(sys.argv[1]))
app = req["app"]
pdir = os.path.join(app, req["dir"])
overlay = req.get("overlay", False)
store = os.path.join(app, "data", "quay-policies")
models = os.path.join(store, "models")
stock = os.path.join(store, "stock")
state_file = os.path.join(store, "state.json")
slots = {s["id"]: s["file"] for s in req.get("slots") or []}

def sha(p):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for b in iter(lambda: f.read(1 << 20), b""):
            h.update(b)
    return h.hexdigest()

def load():
    try:
        with open(state_file) as f:
            st = json.load(f)
        if isinstance(st.get("slots"), dict):
            return st
    except (OSError, ValueError):
        pass
    return {"version": 1, "slots": {}}

def save(st):
    os.makedirs(store, exist_ok=True)
    tmp = state_file + ".tmp"
    with open(tmp, "w") as f:
        json.dump(st, f, indent=1, sort_keys=True)
    os.replace(tmp, state_file)

def place(src, dst):
    os.makedirs(os.path.dirname(dst), exist_ok=True)
    tmp = dst + ".quay-tmp"
    shutil.copyfile(src, tmp)
    os.replace(tmp, dst)

def target(sid):
    return os.path.join(pdir, slots[sid])

def put_back_stock(st, sid):
    t = target(sid)
    if overlay:
        if os.path.lexists(t):
            os.unlink(t)
    else:
        sp = os.path.join(stock, slots[sid])
        if not os.path.isfile(sp):
            sys.exit("no copy of the stock %s was kept; update or reinstall the app to get it back" % slots[sid])
        place(sp, t)
        os.unlink(sp)
    st["slots"].pop(sid, None)

op = req["op"]
st = load()
if op == "status":
    out = {}
    for sid, fn in slots.items():
        p = os.path.join(pdir, fn)
        out[sid] = {"sha256": sha(p) if os.path.isfile(p) else "", "active": st["slots"].get(sid)}
    print(json.dumps({"slots": out, "orphans": sorted(k for k in st["slots"] if k not in slots)}))
elif op == "stockpath":
    sid = req["slot"]
    sp = os.path.join(stock, slots[sid])
    if overlay:
        sp = os.path.join(app, req.get("stockDir") or "", slots[sid]) if req.get("stockDir") else ""
    elif sid not in st["slots"]:
        sp = target(sid)
    print(sp if sp and os.path.isfile(sp) else "")
elif op == "apply":
    sid, meta, up = req["slot"], req["meta"], req["upload"]
    try:
        got = sha(up)
        if got != meta["sha256"]:
            sys.exit("the uploaded model is damaged (checksum mismatch); try again")
        os.makedirs(models, exist_ok=True)
        m = os.path.join(models, got + ".onnx")
        shutil.move(up, m + ".quay-tmp")
        os.replace(m + ".quay-tmp", m)
    finally:
        if os.path.exists(up):
            os.unlink(up)
    prev = st["slots"].get(sid)
    t = target(sid)
    if not overlay and prev is None and os.path.isfile(t):
        place(t, os.path.join(stock, slots[sid]))
    place(m, t)
    meta["installedAt"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    meta["file"] = slots[sid]
    st["slots"][sid] = meta
    save(st)
    print(json.dumps({"previous": prev}))
elif op == "rollback":
    sid, prev = req["slot"], req.get("previous")
    if prev is None:
        put_back_stock(st, sid)
    else:
        place(os.path.join(models, prev["sha256"] + ".onnx"), target(sid))
        st["slots"][sid] = prev
    save(st)
elif op == "restore":
    done = []
    for sid in req["restore"]:
        if sid in st["slots"] and sid in slots:
            put_back_stock(st, sid)
            done.append(sid)
    save(st)
    print(json.dumps({"restored": done}))
elif op == "reapply":
    if not st["slots"]:
        print("QUAY_NONE")
        sys.exit(0)
    for sid, meta in list(st["slots"].items()):
        m = os.path.join(models, meta.get("sha256", "") + ".onnx")
        if sid not in slots:
            print("%s: this version of the app has no such slot; dropped %s" % (sid, meta.get("name")))
            del st["slots"][sid]
            continue
        if not os.path.isfile(m):
            print("%s: the installed model is missing; keeping the stock one" % slots[sid])
            del st["slots"][sid]
            continue
        t = target(sid)
        if not overlay and os.path.isfile(t) and sha(t) != meta["sha256"]:
            place(t, os.path.join(stock, slots[sid]))
        place(m, t)
        print("%s: %s by %s" % (slots[sid], meta.get("name"), meta.get("author")))
    save(st)
elif op == "gc":
    keep = {m.get("sha256") for m in st["slots"].values()}
    if os.path.isdir(models):
        for f in os.listdir(models):
            if f.endswith(".onnx") and f[:-5] not in keep:
                os.unlink(os.path.join(models, f))
else:
    sys.exit("unknown op " + op)
`
