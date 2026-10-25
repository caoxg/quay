"""Tests quay-hotspot's decisions against a fake NetworkManager: python3 -m unittest test_quay_hotspot"""
import importlib.machinery
import importlib.util
import os
import tempfile
import unittest

_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "quay-hotspot")
_loader = importlib.machinery.SourceFileLoader("quay_hotspot", _path)
_spec = importlib.util.spec_from_loader("quay_hotspot", _loader)
qh = importlib.util.module_from_spec(_spec)
_loader.exec_module(qh)
qh.log = lambda msg: None

HOT = "uuid-hotspot"


class FakeNM:
    def __init__(self, saved=(), in_range=(), active="", stations=0):
        self.profiles_ = {HOT: dict(uuid=HOT, id=qh.CON, ssid="duck", mode="ap", hidden=False, autoconnect=False, priority=0)}
        for i, (ssid, prio) in enumerate(saved):
            u = "uuid-%d" % i
            self.profiles_[u] = dict(uuid=u, id=ssid, ssid=ssid, mode="infrastructure", hidden=False, autoconnect=True, priority=prio)
        self.in_range = set(in_range)
        self.active = active
        self.stations_ = stations
        self.available = True
        self.ap_works = True
        self.actions = []

    def uuid(self, ssid):
        return next(u for u, p in self.profiles_.items() if p["ssid"] == ssid and u != HOT)

    def values(self, *args):
        if args[-2:] == ("id", qh.CON):
            return {"connection.uuid": HOT}
        if args[-2] == "uuid":
            return {"802-11-wireless.mode": self.profiles_[args[-1]]["mode"]}
        return {}

    def device(self):
        if not self.available:
            return 20, ""
        return (100, self.active) if self.active else (30, "")

    def profiles(self):
        return [dict(p) for p in self.profiles_.values()]

    def visible(self):
        self.actions.append("scan")
        return set(self.in_range)

    def up(self, uuid, wait=30):
        self.actions.append("up " + self.profiles_[uuid]["ssid"])
        p = self.profiles_[uuid]
        if (p["mode"] == "ap" and self.ap_works) or p["ssid"] in self.in_range:
            self.active = uuid
            return True
        return False

    def down(self, uuid):
        self.actions.append("down " + self.profiles_[uuid]["ssid"])
        if self.active == uuid:
            self.active = ""

    def stations(self):
        return self.stations_


class Run:
    """A Service on a fake clock."""

    def __init__(self, nm):
        self.t = 0.0
        self.dir = tempfile.mkdtemp()
        self.nm = nm
        self.s = qh.Service(nm, clock=lambda: self.t, sleep=self.sleep, run_dir=self.dir)

    def sleep(self, s):
        self.t += s

    def until(self, seconds):
        end = self.t + seconds
        while self.t < end:
            self.s.tick()
            self.t += qh.TICK

    def ask(self, what):
        with open(os.path.join(self.dir, "request"), "w") as f:
            f.write(what)

    def on_hotspot(self):
        return self.nm.active == HOT


class Boot(unittest.TestCase):
    def test_no_wifi_opens_the_hotspot_after_the_boot_wait(self):
        r = Run(FakeNM(saved=[("home", 0)]))
        r.until(qh.BOOT_WAIT - qh.TICK)
        self.assertFalse(r.on_hotspot())
        r.until(2 * qh.TICK)
        self.assertTrue(r.on_hotspot())

    def test_wifi_in_time_keeps_the_hotspot_closed(self):
        nm = FakeNM(saved=[("home", 0)])
        r = Run(nm)
        r.until(20)
        nm.active = nm.uuid("home")
        r.until(300)
        self.assertEqual(nm.actions, [])

    def test_the_wait_starts_when_the_device_comes_up(self):
        nm = FakeNM()
        nm.available = False
        r = Run(nm)
        r.until(120)
        self.assertFalse(r.on_hotspot())
        nm.available = True
        r.until(qh.BOOT_WAIT - qh.TICK)
        self.assertFalse(r.on_hotspot())
        r.until(2 * qh.TICK)
        self.assertTrue(r.on_hotspot())

    def test_a_missing_profile_is_not_retried_every_tick(self):
        nm = FakeNM()
        nm.ap_works = False
        r = Run(nm)
        r.until(2 * qh.BOOT_WAIT - qh.TICK)
        self.assertEqual(nm.actions.count("up duck"), 1)


class LosingWifi(unittest.TestCase):
    def test_hotspot_after_the_lost_wait(self):
        nm = FakeNM(saved=[("home", 0)], in_range=["home"])
        nm.active = nm.uuid("home")
        r = Run(nm)
        r.until(60)
        nm.active, nm.in_range = "", set()
        r.until(qh.LOST_WAIT - qh.TICK)
        self.assertFalse(r.on_hotspot())
        r.until(2 * qh.TICK)
        self.assertTrue(r.on_hotspot())


class OnTheHotspot(unittest.TestCase):
    def start(self, **kw):
        nm = FakeNM(saved=[("home", 0), ("phone", 5)], **kw)
        r = Run(nm)
        r.until(qh.BOOT_WAIT + qh.TICK)
        self.assertTrue(r.on_hotspot())
        nm.actions.clear()
        return nm, r

    def test_never_closes_while_someone_is_on_it(self):
        nm, r = self.start(stations=1)
        nm.in_range = {"home"}
        r.until(3600)
        self.assertEqual(nm.actions, [])
        self.assertTrue(r.on_hotspot())

    def test_unknown_station_count_counts_as_someone(self):
        nm, r = self.start(stations=None)
        nm.in_range = {"home"}
        r.until(3600)
        self.assertEqual(nm.actions, [])

    def test_goes_home_when_idle_and_home_is_back(self):
        nm, r = self.start()
        nm.in_range = {"home"}
        r.until(max(qh.IDLE, qh.RETRY) + 2 * qh.TICK)
        self.assertEqual(nm.active, nm.uuid("home"))
        self.assertEqual(nm.actions, ["down duck", "scan", "up home"])

    def test_prefers_the_higher_priority(self):
        nm, r = self.start()
        nm.in_range = {"home", "phone"}
        r.until(max(qh.IDLE, qh.RETRY) + 2 * qh.TICK)
        self.assertEqual(nm.active, nm.uuid("phone"))

    def test_reopens_when_nothing_is_in_range_and_waits_before_looking_again(self):
        nm, r = self.start()
        r.until(max(qh.IDLE, qh.RETRY) + 2 * qh.TICK)
        self.assertEqual(nm.actions, ["down duck", "scan", "up duck"])
        self.assertTrue(r.on_hotspot())
        nm.actions.clear()
        r.until(qh.RETRY - 3 * qh.TICK)
        self.assertEqual(nm.actions, [])
        r.until(4 * qh.TICK)
        self.assertEqual(nm.actions, ["down duck", "scan", "up duck"])

    def test_does_not_close_when_there_is_no_saved_wifi(self):
        nm = FakeNM()
        r = Run(nm)
        r.until(3600)
        self.assertTrue(r.on_hotspot())
        self.assertEqual(nm.actions, ["up duck"])

    def test_a_device_leaving_restarts_the_idle_wait(self):
        nm, r = self.start(stations=1)
        nm.in_range = {"home"}
        r.until(1000)
        nm.stations_ = 0
        r.until(qh.IDLE - qh.TICK)
        self.assertEqual(nm.actions, [])
        r.until(2 * qh.TICK)
        self.assertEqual(nm.active, nm.uuid("home"))


class Requests(unittest.TestCase):
    def test_open_now_and_back_home_when_idle(self):
        nm = FakeNM(saved=[("home", 0)], in_range=["home"])
        nm.active = nm.uuid("home")
        r = Run(nm)
        r.until(30)
        r.ask("open")
        r.until(qh.TICK)
        self.assertTrue(r.on_hotspot())
        r.until(max(qh.IDLE, qh.RETRY) + 2 * qh.TICK)
        self.assertEqual(nm.active, nm.uuid("home"))

    def test_close_now_even_with_someone_on_it(self):
        nm = FakeNM(saved=[("home", 0)], stations=2)
        r = Run(nm)
        r.until(qh.BOOT_WAIT + qh.TICK)
        nm.in_range = {"home"}
        r.ask("close")
        r.until(qh.TICK)
        self.assertEqual(nm.active, nm.uuid("home"))

    def test_close_with_nothing_in_range_reopens(self):
        nm = FakeNM(stations=2)
        r = Run(nm)
        r.until(qh.BOOT_WAIT + qh.TICK)
        r.ask("close")
        r.until(qh.TICK)
        self.assertTrue(r.on_hotspot())


class OtherHotspot(unittest.TestCase):
    def test_leaves_someone_elses_hotspot_alone(self):
        nm = FakeNM(saved=[("home", 0)], in_range=["home"])
        nm.profiles_["other"] = dict(uuid="other", id="Duck-Setup-Hotspot", ssid="XGO-Duck", mode="ap", hidden=False, autoconnect=True, priority=0)
        nm.active = "other"
        r = Run(nm)
        r.until(3600)
        self.assertEqual(nm.actions, [])
        self.assertEqual(nm.active, "other")


class Parsing(unittest.TestCase):
    def test_split_terse(self):
        self.assertEqual(qh.split_terse(r"a\:b:c\\d:"), ["a:b", "c\\d", ""])
        self.assertEqual(qh.unescape(r"x\:y"), "x:y")


if __name__ == "__main__":
    unittest.main()
