package remote

import (
	_ "embed"
	"strings"
)

// The hotspot service (see hotspot/README.md). Away installs these two files when the board is to
// open its own hotspot.
var (
	//go:embed hotspot/quay-hotspot
	hotspotService string
	//go:embed hotspot/quay-hotspot.service
	hotspotUnit string
)

// NetConfig is where Quay keeps the away plan and which saved networks are phone hotspots:
// {"away": "off"|"board"|"phone"|"both", "phones": [profile uuids]}. Root writes it, anyone reads it.
// ("own" is read as "board": the name before 2026-10-25.)
const NetConfig = "/etc/quay/network.json"

// nmTerse is shared by the network scripts: nmcli -t output parsing.
const nmTerse = `
import json, os, subprocess, sys

def sh(*cmd, timeout=30):
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        return r.returncode, r.stdout
    except Exception:
        return 127, ""

def split(line, sep=":"):
    """Splits an nmcli -t line at unescaped separators; nmcli escapes ':' and '\\' in values."""
    fields, cur, esc = [], [], False
    for ch in line:
        if esc:
            cur.append(ch); esc = False
        elif ch == "\\":
            esc = True
        elif ch == sep:
            fields.append("".join(cur)); cur = []
        else:
            cur.append(ch)
    fields.append("".join(cur))
    return fields

def unesc(v):
    return split(v, None)[0]

def values(*args):
    rc, out = sh("nmcli", "-t", *args)
    res = {}
    for line in out.splitlines():
        k, _, v = line.partition(":")
        res[k] = unesc(v)
    return res

def netconfig():
    try:
        with open("` + NetConfig + `") as f:
            c = json.load(f)
    except Exception:
        c = {}
    away = {"own": "board"}.get(c.get("away"), c.get("away")) or "off"
    return {"away": away, "phones": [u for u in c.get("phones") or [] if isinstance(u, str)]}
`

// Network prints the board's Wi-Fi as JSON: the device, the saved networks (phone hotspots marked),
// the away plan and the board's own hotspot. Runs as the login user.
var Network = nmTerse + sysFiles + `
IFACE = "wlan0"
cfg = netconfig()
out = {"iface": IFACE, "nmcli": False, "ap": False, "state": 0, "active": "", "ip": "", "profiles": [], "away": cfg["away"],
       "hotspot": {"configured": False, "installed": os.path.exists("/etc/systemd/system/quay-hotspot.service"), "enabled": False,
                   "running": False, "ssid": "", "state": None}}
rc, _ = sh("nmcli", "--version")
if rc == 0:
    out["nmcli"] = True
    d = values("-f", "GENERAL.STATE,GENERAL.CON-UUID,WIFI-PROPERTIES.AP,IP4.ADDRESS", "device", "show", IFACE)
    st = d.get("GENERAL.STATE", "").split(" ")[0]
    out["state"] = int(st) if st.isdigit() else 0
    out["active"] = "" if d.get("GENERAL.CON-UUID", "--") == "--" else d.get("GENERAL.CON-UUID", "")
    out["ap"] = d.get("WIFI-PROPERTIES.AP") == "yes"
    out["ip"] = (d.get("IP4.ADDRESS[1]") or "").split("/")[0]
    rc, o = sh("nmcli", "-t", "-f", "UUID,TYPE", "connection", "show")
    for line in o.splitlines():
        f = split(line)
        if len(f) < 2 or f[1] not in ("802-11-wireless", "wifi"):
            continue
        v = values("-f", "connection.id,802-11-wireless.ssid,802-11-wireless.mode,802-11-wireless.hidden,"
                   "connection.autoconnect,connection.autoconnect-priority,802-11-wireless-security.key-mgmt",
                   "connection", "show", "uuid", f[0])
        prio = v.get("connection.autoconnect-priority", "0")
        p = {"uuid": f[0], "id": v.get("connection.id", ""), "ssid": v.get("802-11-wireless.ssid", ""),
             "mode": v.get("802-11-wireless.mode", "") or "infrastructure",
             "hidden": v.get("802-11-wireless.hidden") == "yes", "autoconnect": v.get("connection.autoconnect") == "yes",
             "priority": int(prio) if prio.lstrip("-").isdigit() else 0,
             "secure": bool(v.get("802-11-wireless-security.key-mgmt")), "active": f[0] == out["active"],
             "phone": f[0] in cfg["phones"]}
        if p["id"] == "quay-hotspot":
            out["hotspot"].update(configured=True, ssid=p["ssid"], uuid=p["uuid"], active=p["active"])
        else:
            out["profiles"].append(p)
h = out["hotspot"]
if h["installed"]:
    h["enabled"] = sh("systemctl", "is-enabled", "quay-hotspot.service")[1].strip() == "enabled"
    h["running"] = sh("systemctl", "is-active", "quay-hotspot.service")[1].strip() == "active"
    try:
        with open("/run/quay-hotspot/state.json") as f:
            h["state"] = json.load(f)
    except Exception:
        pass
out["sysFiles"] = sys_files()
print(json.dumps(out))
`

// WifiScan prints the Wi-Fi networks in range as JSON (strongest first, one row per SSID).
const WifiScan = nmTerse + `
rows = {}
rc, o = sh("nmcli", "-t", "-f", "SSID,SIGNAL,SECURITY", "device", "wifi", "list", "ifname", "wlan0", "--rescan", "yes", timeout=45)
if rc != 0:  # rescanning may need a local session; the cached list is still useful
    rc, o = sh("nmcli", "-t", "-f", "SSID,SIGNAL,SECURITY", "device", "wifi", "list", "ifname", "wlan0", "--rescan", "no")
if rc != 0:
    print(o.strip() or "cannot list Wi-Fi networks", file=sys.stderr); sys.exit(1)
for line in o.splitlines():
    f = split(line)
    if len(f) < 3 or not f[0]:
        continue
    sig = int(f[1]) if f[1].isdigit() else 0
    if f[0] not in rows or rows[f[0]]["signal"] < sig:
        rows[f[0]] = {"ssid": f[0], "signal": sig, "security": f[2] if f[2] != "--" else ""}
print(json.dumps(sorted(rows.values(), key=lambda r: -r["signal"])))
`

// netcfg is a shell function for the root scripts: it changes NetConfig and makes the phone
// hotspots' profiles follow the away plan (joined automatically only for "phone" and "both", and
// below every other saved network).
//
//	netcfg away board|phone|both|off    netcfg phone UUID yes|no    netcfg forget UUID
const netcfg = `netcfg() { python3 - "$@" <<'QUAY_NETCFG'
import json, os, subprocess, sys
path = "` + NetConfig + `"
try:
    with open(path) as f:
        cfg = json.load(f)
except Exception:
    cfg = {}
cfg = {"away": {"own": "board"}.get(cfg.get("away"), cfg.get("away")) or "off", "phones": [u for u in cfg.get("phones") or [] if isinstance(u, str)]}
op, args = sys.argv[1], sys.argv[2:]

def nm(*a):
    return subprocess.run(["nmcli", *a], capture_output=True, text=True).returncode == 0

if op == "away":
    cfg["away"] = args[0]
elif op == "phone" and args[1] == "yes":
    cfg["phones"] = [u for u in cfg["phones"] if u != args[0]] + [args[0]]
elif op in ("phone", "forget"):
    cfg["phones"] = [u for u in cfg["phones"] if u != args[0]]
    if op == "phone":  # an ordinary network again
        nm("connection", "modify", "uuid", args[0], "connection.autoconnect", "yes", "connection.autoconnect-priority", "0")
join = "yes" if cfg["away"] in ("phone", "both") else "no"
cfg["phones"] = [u for u in cfg["phones"]
                 if nm("connection", "modify", "uuid", u, "connection.autoconnect", join, "connection.autoconnect-priority", "-10")]
os.makedirs(os.path.dirname(path), exist_ok=True)
with open(path + ".tmp", "w") as f:
    json.dump(cfg, f)
os.chmod(path + ".tmp", 0o644)
os.replace(path + ".tmp", path)
QUAY_NETCFG
}
`

// keyfile writes a NetworkManager keyfile from the JSON request in $SECRETS (which it deletes):
// {"kind": "hotspot"|"wifi", "ssid", "psk", "hidden"}. The hotspot keeps its uuid, and its password
// when psk is empty. Prints the file's path and the profile's uuid.
const keyfile = `python3 - "$SECRETS" "$IFACE" <<'QUAY_PY'
import json, os, sys, uuid
req = json.load(open(sys.argv[1]))
os.remove(sys.argv[1])
iface = sys.argv[2]
d = "/etc/NetworkManager/system-connections"

def esc(v):  # GKeyFile string escaping
    v = v.replace("\\", "\\\\").replace("\n", "\\n").replace("\t", "\\t").replace("\r", "\\r")
    return "\\s" + v[1:] if v.startswith(" ") else v

def read(path):
    res, sec = {}, ""
    try:
        for line in open(path):
            line = line.strip()
            if line.startswith("["):
                sec = line.strip("[]")
            elif "=" in line:
                k, _, v = line.partition("=")
                res[sec + "." + k] = v
    except OSError:
        pass
    return res

if req["kind"] == "hotspot":
    path = os.path.join(d, "quay-hotspot.nmconnection")
    old = read(path)
    psk = esc(req["psk"]) if req["psk"] else old.get("wifi-security.psk", "")
    if not psk:
        sys.exit("the hotspot needs a password")
    u = old.get("connection.uuid") or str(uuid.uuid4())
    text = ("[connection]\nid=quay-hotspot\nuuid=%s\ntype=wifi\ninterface-name=%s\nautoconnect=false\n\n"
            "[wifi]\nband=bg\nmode=ap\nssid=%s\n\n"
            "[wifi-security]\ngroup=ccmp;\nkey-mgmt=wpa-psk\npairwise=ccmp;\nproto=rsn;\npsk=%s\n\n"
            "[ipv4]\nmethod=shared\n\n[ipv6]\naddr-gen-mode=default\nmethod=disabled\n") % (u, iface, esc(req["ssid"]), psk)
else:
    u = str(uuid.uuid4())
    path = os.path.join(d, "quay-wifi-%s.nmconnection" % u[:8])
    text = ("[connection]\nid=%s\nuuid=%s\ntype=wifi\nautoconnect=true\n\n[wifi]\nmode=infrastructure\nssid=%s\n%s\n"
            % (esc(req["ssid"]), u, esc(req["ssid"]), "hidden=true\n" if req.get("hidden") else ""))
    if req["psk"]:
        text += "[wifi-security]\nkey-mgmt=wpa-psk\npsk=%s\n\n" % esc(req["psk"])
    text += "[ipv4]\nmethod=auto\n\n[ipv6]\naddr-gen-mode=default\nmethod=auto\n"
os.makedirs(d, exist_ok=True)
fd = os.open(path + ".tmp", os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w") as f:
    f.write(text)
os.replace(path + ".tmp", path)
print(path, u)
QUAY_PY
`

// WifiAdd saves a Wi-Fi network ($SECRETS: name, password, hidden, phone). An ordinary network is
// joined whenever it is in range; a phone hotspot as the away plan says. The current connection
// stays.
const WifiAdd = `
IFACE=wlan0
trap 'rm -f "$SECRETS"' EXIT
` + netcfg + `
command -v nmcli >/dev/null || { echo "NetworkManager (nmcli) is not installed" >&2; exit 1; }
SSID=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["ssid"])' "$SECRETS")
PHONE=$(python3 -c 'import json,sys; print("yes" if json.load(open(sys.argv[1])).get("phone") else "no")' "$SECRETS")
while IFS= read -r uuid; do
  if [ "$(nmcli -g 802-11-wireless.ssid connection show uuid "$uuid" 2>/dev/null)" = "$SSID" ] &&
     [ "$(nmcli -g 802-11-wireless.mode connection show uuid "$uuid" 2>/dev/null)" != ap ]; then
    echo "\"$SSID\" is saved already; remove it first to change its password" >&2
    exit 1
  fi
done < <(nmcli -g UUID,TYPE connection show | awk -F: '$2 == "802-11-wireless" || $2 == "wifi" {print $1}')
out=$(` + keyfile + `)
read -r path uuid <<<"$out"
nmcli connection load "$path"
if [ "$PHONE" = yes ]; then
  netcfg phone "$uuid" yes
  echo "saved the phone hotspot \"$SSID\""
else
  echo "saved Wi-Fi \"$SSID\""
fi
`

// WifiPhone marks a saved network as a phone hotspot or as an ordinary one ($UUID, $PHONE yes|no).
const WifiPhone = netcfg + `
netcfg phone "$UUID" "$PHONE"
[ "$PHONE" = yes ] && echo "marked as a phone hotspot" || echo "marked as an ordinary Wi-Fi"
`

// WifiRemove deletes a saved Wi-Fi profile ($UUID), unless the board is using it right now.
const WifiRemove = netcfg + `
if [ "$(nmcli -g GENERAL.CON-UUID device show wlan0 2>/dev/null)" = "$UUID" ]; then
  echo "the board is on this Wi-Fi right now; it can be removed once the board uses another connection" >&2
  exit 1
fi
nmcli connection delete uuid "$UUID" >/dev/null
netcfg forget "$UUID"
echo "removed"
`

// HotspotSettings saves the board's own hotspot: the quay-hotspot NetworkManager profile ($SECRETS:
// name and password; an empty password keeps the old one). It never opens it.
const HotspotSettings = `
IFACE=wlan0
trap 'rm -f "$SECRETS"' EXIT
command -v nmcli >/dev/null || { echo "NetworkManager (nmcli) is not installed" >&2; exit 1; }
if [ "$(nmcli -g WIFI-PROPERTIES.AP device show "$IFACE" 2>/dev/null)" != yes ]; then
  echo "this board's Wi-Fi ($IFACE) cannot run a hotspot" >&2
  exit 1
fi
out=$(` + keyfile + `)
read -r path uuid <<<"$out"
nmcli connection load "$path"
echo "hotspot \"$(nmcli -g 802-11-wireless.ssid connection show uuid "$uuid")\" saved"
`

// Away sets the away plan ($MODE): "board" and "both" install and start the hotspot service,
// "phone" and "off" remove it (the hotspot's settings stay). If the board's own hotspot is open
// and no longer wanted, it closes a few seconds after this script returns (Quay may be connected
// through it).
var Away = `
IFACE=wlan0
` + netcfg + `
case "$MODE" in board|both|phone|off) ;; *) echo "unknown away plan $MODE" >&2; exit 1 ;; esac
ours=$(nmcli -g connection.uuid connection show id quay-hotspot 2>/dev/null || true)
if [ "$MODE" = board ] || [ "$MODE" = both ]; then
  [ -n "$ours" ] || { echo "set the hotspot's name and password first" >&2; exit 1; }
  netcfg away "$MODE"
  mkdir -p /usr/local/lib/quay
  cat > /usr/local/lib/quay/quay-hotspot <<'QUAY_HOTSPOT_EOF'
` + strings.TrimRight(hotspotService, "\n") + `
QUAY_HOTSPOT_EOF
  chmod 755 /usr/local/lib/quay/quay-hotspot
  cat > /etc/systemd/system/quay-hotspot.service <<'QUAY_UNIT_EOF'
` + strings.TrimRight(hotspotUnit, "\n") + `
QUAY_UNIT_EOF
  systemctl daemon-reload
  systemctl enable quay-hotspot.service >/dev/null 2>&1
  systemctl restart quay-hotspot.service
else
  netcfg away "$MODE"
  systemctl disable --now quay-hotspot.service >/dev/null 2>&1 || true
  rm -f /etc/systemd/system/quay-hotspot.service /usr/local/lib/quay/quay-hotspot
  rmdir /usr/local/lib/quay 2>/dev/null || true
  rm -rf /run/quay-hotspot
  systemctl daemon-reload
  if [ -n "$ours" ] && [ "$(nmcli -g GENERAL.CON-UUID device show "$IFACE" 2>/dev/null)" = "$ours" ]; then
    systemd-run --quiet --on-active=3 nmcli connection down uuid "$ours"
    echo "the board's hotspot closes in a few seconds; the board then joins a saved Wi-Fi in range"
  fi
fi
case "$MODE" in
  board) echo "away from saved Wi-Fi, the board opens its own hotspot" ;;
  phone) echo "away from saved Wi-Fi, the board joins a phone hotspot" ;;
  both) echo "away from saved Wi-Fi, the board joins a phone hotspot, else opens its own" ;;
  off) echo "away from saved Wi-Fi, the board stays offline" ;;
esac
`

// HotspotRequest asks the running service to open the hotspot now, or close it and look for
// Wi-Fi ($WHAT: open|close). The service acts within seconds, after this script has returned.
const HotspotRequest = `
systemctl is-active --quiet quay-hotspot.service || { echo "the board's own hotspot is not in the away plan" >&2; exit 1; }
/usr/local/lib/quay/quay-hotspot "$WHAT"
`

// NMAutoconnectOff stops saved NetworkManager profiles ($UUIDS) coming up by themselves; they stay
// saved, and the current connection stays as it is. (A system profile's nm_autoconnect_off.)
const NMAutoconnectOff = `
for u in $UUIDS; do
  nmcli connection modify uuid "$u" connection.autoconnect no
  echo "$(nmcli -g connection.id connection show uuid "$u"): no longer comes up by itself"
done
`

// NMDelete deletes saved NetworkManager profiles ($UUIDS), except the one in use.
// (A system profile's nm_delete.)
const NMDelete = `
active=$(nmcli -g GENERAL.CON-UUID device show wlan0 2>/dev/null || true)
for u in $UUIDS; do
  if [ "$u" = "$active" ]; then
    echo "$(nmcli -g connection.id connection show uuid "$u") is in use; left alone" >&2
    continue
  fi
  nmcli connection delete uuid "$u" >/dev/null && echo "deleted $u"
done
`
