// Bridge to the Go service. In a plain browser (no Wails runtime) a mock backend is used so the
// UI can be developed and previewed: open frontend/index.html?mock through any static server.
const PKG = 'github.com/caoxg/quay/internal/gui.Service';
let rt = null;
if (!new URLSearchParams(location.search).has('mock')) {
  try { rt = await import('/wails/runtime.js'); } catch { rt = null; }
}
export const isMock = !rt;

const listeners = new Map();
function localOn(name, cb) {
  if (!listeners.has(name)) listeners.set(name, new Set());
  listeners.get(name).add(cb);
  return () => listeners.get(name).delete(cb);
}
function localEmit(name, data) { (listeners.get(name) || []).forEach((cb) => cb(data)); }

export function on(name, cb) {
  if (rt) return rt.Events.On(name, (ev) => cb(ev.data));
  return localOn(name, cb);
}

export function call(method, ...args) {
  if (rt) return rt.Call.ByName(`${PKG}.${method}`, ...args);
  const fn = mock[method];
  if (!fn) return Promise.reject(new Error('mock: no ' + method));
  return Promise.resolve().then(() => fn(...args));
}

// ---------------------------------------------------------------- mock backend
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let seq = 0;
const nid = () => 'm' + (++seq) + Math.random().toString(16).slice(2, 6);
const mockState = {
  boards: [{ name: 'duck', kind: 'ssh', host: 'duck.local', port: 22, user: 'arduino', boardId: '2c3e91a7' }, { name: 'desk', kind: 'adb', serial: '' }],
  def: 'duck',
  lang: '',
  connected: {},
  mirror: { pypi: '', debian: '' },
  hostname: 'duck',
  relay: false,
  relayUpstream: '',
  appcli: '0.13.0',
  upgraded: '',
  apps: [
    {
      folder: 'xgoduck_runtime_arduino', path: '/home/arduino/ArduinoApps/xgoduck_runtime_arduino', id: 'user:xgoduck_runtime_arduino', name: 'XGO Duck',
      description: 'XGO Duck on Arduino Uno Q, based on Microduck. Linux runs ONNX policies at 50 Hz; the STM32 drives 15 servos and a QMI8658 at 100 Hz.',
      icon: '🦆', status: 'running', default: true, data: ['gamepad-bindings.json', 'zero_pos.json'],
      deploy: { repo: 'https://github.com/LuwuDynamics/xgoduck_runtime_arduino', ref: 'master', commit: '9dc36787bc61b102c2da0f97c9c758e639e90ab8', subject: 'Add servo scanning functionality', commitDate: '2026-10-12T08:00:00Z', profile: 'xgoduck', deployedAt: new Date(Date.now() - 3 * 864e5).toISOString(), quay: 'dev' },
      profile: null, systemReady: false, ports: [9527], webUrl: 'http://duck.local:9527/',
    },
    {
      folder: 'weather_station', path: '/home/arduino/ArduinoApps/weather_station', id: 'user:weather_station', name: 'Weather Station',
      description: 'Reads a BME280 over Qwiic and charts temperature and humidity in the browser.', icon: '🌦️', status: 'stopped', default: false, data: [],
      deploy: { repo: 'https://github.com/example/unoq-weather', ref: 'v1.2.0', commit: 'a1b2c3d4e5f60718293a4b5c6d7e8f9012345678', subject: 'Release 1.2.0', commitDate: '2026-09-20T08:00:00Z', deployedAt: new Date(Date.now() - 14 * 864e5).toISOString(), quay: 'dev' },
      profile: null, systemReady: null, ports: [7000], webUrl: 'http://duck.local:7000/',
    },
  ],
};
const duckProfile = {
  id: 'xgoduck', name: 'XGO-Duck', description: 'Duck-shaped biped robot (Microduck lineage) — walking, get-up and pick policies at 50 Hz.', folder: 'xgoduck_runtime_arduino',
  source: { repo: 'https://github.com/LuwuDynamics/xgoduck_runtime_arduino' }, origin: 'builtin',
  system: { description: 'Sets the Linux↔MCU router serial link to 2 Mbps (the runtime\'s tools/router-2m.sh). The setting is global: other apps that expect the stock 115200 baud stop working until it is reverted.', descriptionZh: '把 Linux 和 MCU 之间的路由串口提速到 2 Mbps（运行时自带的 tools/router-2m.sh）。这是全板设置：其他按默认 115200 波特率工作的 App 在撤销前都会失灵。', check: '…', apply: 'bash tools/router-2m.sh', revert: '…', stopApp: true },
  beforeStart: { run: 'python3 tools/prepare-bridge.py', resolveOnFailure: true, description: 'Patches the RouterBridge and RPClite libraries the sketch uses (a bigger RPC stack, bounded message framing) and rebuilds the firmware when they change.', descriptionZh: '给固件用到的 RouterBridge、RPClite 库打补丁（更大的 RPC 栈、限定长度的消息帧），库有变化时自动重新编译固件。' },
  web: { port: 9527 }, health: { url: 'http://127.0.0.1:9527/api/status', timeout: 240 },
  uninstall: { run: '…', description: 'Puts back the library files prepare-bridge.py patched.', descriptionZh: '把 prepare-bridge.py 打过补丁的库文件恢复原样。' },
  actions: [{ label: 'Calibration', labelZh: '标定', url: '/calibrate.html', description: 'Set each servo\'s center.' }, { label: 'Servo setup', labelZh: '舵机设置', url: '/servo.html' }],
  safety: { url: 'http://127.0.0.1:9527/api/status', field: 'enabled', message: 'The servos are powered (torque on).' },
};
duckProfile.policies = {
  dir: 'python', input: [1, 61], output: [1, 14], matchMetadata: ['joint_names', 'observation_names'],
  index: { url: 'https://raw.githubusercontent.com/caoxg/awesome-xgo-duck/main/policies/index.yaml' },
  slots: [
    { id: 'walk', file: 'xgoduck_walk.onnx', name: 'Walk', nameZh: '行走', description: 'Default walking model, also used for the recovery walk.' },
    { id: 'roller', file: 'xgoduck_roller.onnx', name: 'Roller walk', nameZh: '轮滑', description: 'Used in Roller mode.', requireMetadata: ['default_joint_pos'] },
    { id: 'getup', file: 'xgoduck_getup.onnx', name: 'Get up', nameZh: '起身' },
    { id: 'pick', file: 'xgoduck_pick.onnx', name: 'Pick', nameZh: '啄食' },
    { id: 'roulade', file: 'xgoduck_roulade.onnx', name: 'Roulade', nameZh: '翻滚' },
  ],
};
mockState.apps[0].profile = duckProfile;
mockState.apps[0].policies = {};
const MOCK_POLICIES = [
  { id: 'walk-luwu-2026-09', app: 'xgoduck', slot: 'walk', name: 'Walk (runtime of 2026-09-22)', nameZh: '行走（2026-09-22 版运行时）', author: 'Luwu Dynamics',
    description: 'The walking model the runtime shipped until 2026-10-22.', descriptionZh: '运行时在 2026-10-22 之前自带的行走模型。', url: 'https://raw.githubusercontent.com/…/xgoduck_walk.onnx',
    sha256: '5727b909d5ea1a0f9ff63bf4cea9707339dea6a522374c7a120da633605b923f', size: 793706, created: '2026-09-22', homepage: 'https://github.com/LuwuDynamics/xgoduck_runtime_arduino' },
  { id: 'roller-soft-demo', app: 'xgoduck', slot: 'roller', name: 'Softer roller (demo)', nameZh: '更柔和的轮滑（演示）', author: 'someone',
    description: 'Mock entry for the UI preview.', url: 'https://example.com/roller.onnx', sha256: 'a'.repeat(64), size: 794173, license: 'Apache-2.0', created: '2026-10-23', video: 'https://example.com/video' },
];

function emitOp(op, e) { localEmit('quay:op', { op, time: Date.now(), ...e }); }

async function simulate(kind, board, folder, steps, result, opts = {}) {
  const op = nid();
  setTimeout(async () => {
    emitOp(op, { type: 'begin', kind, board, folder });
    if (board && !mockState.connected[board]) {
      emitOp(op, { type: 'step', step: 'connect', state: 'run', text: 'Connect to arduino@duck.local', kind, board, folder });
      await sleep(500);
      if (!mockState.askedPassword) {
        mockState.askedPassword = true;
        const ok = await mockPrompt({ kind: 'secret', title: 'SSH login', message: 'Password for arduino@duck.local' });
        if (!ok) { emitOp(op, { type: 'end', state: 'cancel', error: 'cancelled', kind, board, folder }); return; }
      }
      mockState.connected[board] = true;
      emitOp(op, { type: 'step', step: 'connect', state: 'ok', text: 'Connected to arduino@duck.local', kind, board, folder });
    }
    for (const s of steps) {
      emitOp(op, { type: 'step', step: s[0], state: 'run', text: s[1], kind, board, folder });
      await sleep(s[3] ?? 450);
      for (const l of s[2] || []) { emitOp(op, { type: 'log', text: l, kind, board, folder }); await sleep(90); }
      if (s[4] === 'fail') { emitOp(op, { type: 'step', step: s[0], state: 'fail', text: s[5], kind, board, folder }); emitOp(op, { type: 'end', state: 'fail', error: s[5], unsafe: !!s[6], kind, board, folder }); return; }
      emitOp(op, { type: 'step', step: s[0], state: s[4] || 'ok', text: s[1], kind, board, folder });
    }
    if (opts.warn) emitOp(op, { type: 'warn', text: opts.warn, kind, board, folder });
    opts.after?.();
    emitOp(op, { type: 'end', state: 'ok', result: typeof result === 'function' ? result() : result, kind, board, folder });
  }, 10);
  return op;
}

const prompts = new Map();
function mockPrompt(p) {
  return new Promise((resolve) => { const id = nid(); prompts.set(id, resolve); localEmit('quay:prompt', { id, ...p }); });
}

function statusData(board) {
  if (board === 'desk') throw new Error('no UNO Q found over USB (use a data cable)');
  return {
    board, target: `arduino@${mockState.hostname}.local`, user: 'arduino', home: '/home/arduino', hostname: mockState.hostname, model: 'Arduino UNO Q', serial: '2c3e91a7', isUnoQ: true,
    ips: [{ iface: 'wlan0', ip: '192.168.1.42' }], appcli: true, appcliVersion: `Arduino App CLI version ${mockState.appcli}`,
    os: { id: 'debian', name: 'Debian GNU/Linux', pretty: 'Debian GNU/Linux 13 (trixie)', version: '13.1', codename: 'trixie' },
    mirror: { pip: mockState.mirror.pypi, pipFallback: mockState.mirror.pipFallback, uv: mockState.mirror.pypi, debian: mockState.mirror.debian || 'http://deb.debian.org/debian', debianFallback: mockState.mirror.debianFallback || [], managed: !!(mockState.mirror.pypi || mockState.mirror.debian) },
    authorizedKeys: mockState.keyUploaded ? ['AAAAC3NzaC1lZDI1NTE5AAAAIMockKey'] : [],
    relay: { installed: mockState.relay, active: mockState.relay },
    apps: JSON.parse(JSON.stringify(mockState.apps)), broken: [], checkedAt: Date.now(),
  };
}

// A system update like the one a real UNO Q had pending (361 packages, 824 MB), with the long tail
// of small Debian packages filled in.
function systemData() {
  const p = (name, from, to, size, source, extra = {}) => ({ name, from, to, size, source, arduino: source === 'apt-repo.arduino.cc', desktop: false, ...extra });
  let pkgs = [
    p('arduino-app-lab', '0.10.0', '0.11.0', 186423376, 'apt-repo.arduino.cc', { desktop: true }),
    p('firmware-qcom-dsp', '', '0~20260916-2~bpo13+1', 133062000, 'deb.debian.org'),
    p('chromium', '143.0.7499.40-1~deb13u1', '155.0.8059.79-1~deb13u1', 79167000, 'deb.debian.org', { desktop: true }),
    p('arduino-app-cli', '0.13.0', '0.14.0', 55386796, 'apt-repo.arduino.cc'),
    p('firmware-atheros', '20250410-2', '20260916-1~bpo13+1', 39634000, 'deb.debian.org'),
    p('chromium-common', '143.0.7499.40-1~deb13u1', '155.0.8059.79-1~deb13u1', 36280000, 'deb.debian.org', { desktop: true }),
    p('arduino-router', '0.10.0', '0.11.0', 2091564, 'apt-repo.arduino.cc'),
    p('libasound2t64', '1.2.14-1', '1.2.16.1-1~bpo13+1', 410000, 'apt-repo.arduino.cc'),
    p('libasound2-data', '1.2.14-1', '1.2.16.1-1~bpo13+1', 42000, 'apt-repo.arduino.cc'),
    p('bash', '5.2.37-2+b5', '5.2.37-2+b10', 1456340, 'deb.debian.org'),
  ];
  const rest = 863905000 - pkgs.reduce((a, x) => a + x.size, 0);
  for (let i = 0; i < 351; i++) pkgs.push(p(`lib${['gtk', 'x11', 'ssl', 'gst', 'qt', 'av'][i % 6]}-${i}`, '1.0', '1.1', Math.round(rest / 351), 'deb.debian.org'));
  if (mockState.upgraded === 'all') pkgs = [];
  if (mockState.upgraded === 'partial') pkgs = pkgs.filter((x) => x.desktop);
  if (mockState.upgraded === 'arduino') pkgs = pkgs.filter((x) => !x.arduino);
  const sum = (l) => ({ count: l.length, size: l.reduce((a, x) => a + x.size, 0) });
  const hosts = [...new Set(pkgs.map((x) => x.source))];
  return {
    packages: pkgs, total: sum(pkgs), arduino: sum(pkgs.filter((x) => x.arduino)), desktop: sum(pkgs.filter((x) => x.desktop)),
    sources: hosts.map((h) => ({ host: h, ...sum(pkgs.filter((x) => x.source === h)) })).sort((a, b) => b.size - a.size),
    rebootRequired: false, running: mockState.apps.filter((a) => a.status === 'running').map((a) => a.name),
    appcliVersion: `Arduino App CLI version ${mockState.appcli}`, checkedAt: Date.now(),
  };
}

const MIRRORS = [
  ['tuna', 'Tsinghua TUNA', '清华 TUNA', 'https://mirrors.tuna.tsinghua.edu.cn/pypi/web/simple', 'https://mirrors.tuna.tsinghua.edu.cn/debian'],
  ['ustc', 'USTC', '中科大 USTC', 'https://mirrors.ustc.edu.cn/pypi/simple', 'https://mirrors.ustc.edu.cn/debian'],
  ['aliyun', 'Alibaba Cloud', '阿里云', 'https://mirrors.aliyun.com/pypi/simple', 'https://mirrors.aliyun.com/debian'],
  ['tencent', 'Tencent Cloud', '腾讯云', 'https://mirrors.cloud.tencent.com/pypi/simple', 'https://mirrors.cloud.tencent.com/debian'],
  ['huawei', 'Huawei Cloud', '华为云', 'https://repo.huaweicloud.com/repository/pypi/simple', 'https://repo.huaweicloud.com/debian'],
  ['bfsu', 'BFSU', '北外 BFSU', 'https://mirrors.bfsu.edu.cn/pypi/web/simple', 'https://mirrors.bfsu.edu.cn/debian'],
  ['nju', 'Nanjing University', '南京大学', 'https://mirror.nju.edu.cn/pypi/web/simple', 'https://mirror.nju.edu.cn/debian'],
  ['sjtu', 'SJTU', '上海交大', 'https://mirror.sjtu.edu.cn/pypi/web/simple', 'https://mirror.sjtu.edu.cn/debian'],
].map(([id, name, nameZh, pypi, debian]) => ({ id, name, nameZh, pypi, debian }));
// The results a real UNO Q measured (MB/s, ms).
const MIRROR_RESULTS = [['tuna', 11.7, 347, 158], ['ustc', 14.1, 93, 90], ['aliyun', 0.26, 213, 104], ['tencent', 4.0, 151, 385], ['huawei', 10.2, 87, 52],
  ['bfsu', 10.2, 209, 209], ['nju', 12.6, 115, 99], ['sjtu', 13.8, 130, 331], ['official', 10.8, 605, 465]]
  .map(([id, mbs, ms, pms]) => ({ id, debian: { ok: true, ms, speed: Math.round(mbs * 1048576), security: true }, pypi: { ok: true, ms: pms } }));

const mock = {
  Info: () => ({ version: 'dev', os: new URLSearchParams(location.search).get('os') || 'darwin', lang: mockState.lang, backupsDir: '~/Library/Application Support/Quay/backups', catalog: [duckProfile], mirror: 'https://mirrors.tuna.tsinghua.edu.cn/pypi/web/simple', debianMirror: 'https://mirrors.tuna.tsinghua.edu.cn/debian',
    mirrors: MIRRORS, official: { id: 'official', name: 'Official servers', nameZh: '官方源', pypi: 'https://pypi.org/simple', debian: 'http://deb.debian.org/debian' },
    publicKey: mockState.hasKey || !new URLSearchParams(location.search).has('nokey') ? { path: '/Users/me/.ssh/id_ed25519.pub', blob: 'AAAAC3NzaC1lZDI1NTE5AAAAIMockKey' } : null }),
  Licenses: () => ({ notice: 'Quay\nCopyright 2026 Xiaogang Cao', thirdParty: 'Quay includes the following third-party software…\n\n(the real list is generated by build/licenses.sh)' }),
  Boards: () => ({ boards: mockState.boards, default: mockState.def }),
  SaveBoard: (t, oldName, makeDefault) => {
    mockState.boards = mockState.boards.filter((b) => b.name !== oldName && b.name !== t.name).concat([t]);
    if (makeDefault) mockState.def = t.name;
  },
  RemoveBoard: (n) => { mockState.boards = mockState.boards.filter((b) => b.name !== n); },
  SetDefault: (n) => { mockState.def = n; },
  SetLang: (l) => { mockState.lang = l; },
  USBDevices: async () => { await sleep(300); return [{ serial: '2a1f9c03', state: 'device' }]; },
  Respond: (id, value, ok) => { const r = prompts.get(id); prompts.delete(id); r?.(ok); },
  Cancel: () => {},
  Disconnect: () => {},
  PickBackup: () => '/Users/me/Library/Application Support/Quay/backups/xgoduck_runtime_arduino-9f3a21c7-20261023-101500.tgz',
  PickExport: (n) => '/Users/me/Downloads/' + n,
  PickFolder: () => '/Users/me/src/my-unoq-app',
  PickKey: () => '/Users/me/.ssh/id_ed25519',
  OpenURL: (u) => window.open(u, '_blank'),
  Reveal: () => {},
  Status: (board) => {
    const op = nid();
    setTimeout(async () => {
      emitOp(op, { type: 'begin', kind: 'status', board });
      await sleep(board === 'desk' ? 700 : 600);
      try { emitOp(op, { type: 'end', state: 'ok', kind: 'status', board, result: statusData(mockState.boards.find((b) => b.name === board) ? board : board) }); } catch (e) { emitOp(op, { type: 'end', state: 'fail', kind: 'status', board, error: e.message }); }
    }, 10);
    return op;
  },
  Refs: () => simulate('refs', '', '', [], { url: 'https://github.com/LuwuDynamics/xgoduck_runtime_arduino', defaultBranch: 'master', refs: [{ name: 'master', kind: 'branch', commit: '466512e9426f61fb07d9be9b3f20df0171692150' }, { name: 'next-audio-video-tof', kind: 'branch', commit: '44b2ac8d' }] }),
  Inspect: (req) => simulate('inspect', '', '', [['fetch', 'Get ' + (req.repo || req.dir), ['Enumerating objects: 214, done.', 'Total 214 (delta 98)'], 900]], () => ({
    key: nid(), folder: 'xgoduck_runtime_arduino', profile: duckProfile,
    bundle: { repo: 'https://github.com/LuwuDynamics/xgoduck_runtime_arduino', ref: req.ref || 'master', commit: '466512e9426f61fb07d9be9b3f20df0171692150', subject: 'Update XGO Duck runtime and servo protection setup', date: '2026-10-22T08:00:00Z', app: { name: 'XGO Duck', description: 'XGO Duck on Arduino Uno Q, based on Microduck.', icon: '🦆' }, folder: 'xgoduck_runtime_arduino', files: 33 },
  })),
  Deploy: (req) => simulate('deploy', req.board, req.folder, [
    ['board', 'Check the board'], ['safety', 'Check it is safe to stop XGO Duck'],
    ['backup', 'Back up data/', ['saved ~/Library/Application Support/Quay/backups/xgoduck_runtime_arduino-9f3a21c7-20261023-101833.tgz']],
    ['stop', 'Stop XGO Duck', ['App stopped']], ['upload', 'Upload 33 files (6.1 MB)', [], 900],
    ['install', 'Install into ~/ArduinoApps/xgoduck_runtime_arduino (data/ kept)', ['installed 33 files into /home/arduino/ArduinoApps/xgoduck_runtime_arduino']],
    ['before_start', 'Run the app\'s preparation hook', ['RouterBridge 0.4.3 RPC stack already 4096 bytes', 'RPClite 0.3.1 bounded framing installed / updated']],
    ['start', 'Start the app', ['Starting app XGO Duck…', 'Building sketch (cached)', 'App started'], 1300],
  ], { folder: req.folder, commit: '466512e9426f61fb07d9be9b3f20df0171692150', previous: '9dc36787bc61b102c2da0f97c9c758e639e90ab8', needsSetup: true }, {
    warn: 'one-time system setup for XGO-Duck is not done yet: run Prepare (it needs sudo) before the app can work',
    after: () => { const a = mockState.apps[0]; a.deploy.commit = '466512e9426f61fb07d9be9b3f20df0171692150'; a.deploy.subject = 'Update XGO Duck runtime and servo protection setup'; a.deploy.deployedAt = new Date().toISOString(); a.status = 'running'; },
  }),
  Update: (b, f) => mock.Deploy({ board: b, folder: f }),
  Start: (b, f) => simulate('start', b, f, [['start', 'Start', ['App started'], 900]], null, { after: () => { mockState.apps.find((a) => a.folder === f).status = 'running'; } }),
  // The mock duck holds torque while running, so stopping it needs force (like the real safety probe).
  Stop: (b, f, force) => simulate('stop', b, f, !force && f === 'xgoduck_runtime_arduino'
    ? [['safety', 'Check it is safe to stop', [], 450, 'fail', "The servos are powered (torque on). Turn torque off in the duck's web UI or put the duck down first.", true]]
    : [['safety', 'Check it is safe to stop', [], 450, force ? 'skip' : 'ok'], ['stop', 'Stop', ['App stopped'], 700]], null, { after: () => { mockState.apps.find((a) => a.folder === f).status = 'stopped'; } }),
  Restart: (b, f) => simulate('restart', b, f, [['stop', 'Stop'], ['start', 'Start', [], 900]]),
  Autostart: (b, f) => simulate('autostart', b, f, [['autostart', 'Boot app']], null, { after: () => mockState.apps.forEach((a) => { a.default = a.folder === f; }) }),
  Prepare: (b, f, revert) => simulate(revert ? 'revert' : 'prepare', b, f, [['stop', 'Stop XGO Duck'], ['system', 'Apply system setup', ['ExecStart=/usr/bin/arduino-router --serial-baudrate 2000000 …'], 1200], ['verify', 'Verify'], ['start', 'Start XGO Duck again', [], 900]], null, { after: () => { mockState.apps[0].systemReady = !revert; } }),
  Mirror: (b, m) => simulate('mirror', b, '', [['mirror', m.pypi || m.debian ? 'Use mirrors' : 'Restore the original configuration',
    m.pypi || m.debian ? [m.pypi ? 'pip and uv now use ' + m.pypi : '', m.debian ? '/etc/apt/sources.list.d/debian.sources now uses ' + m.debian : ''].filter(Boolean) : ['restored /etc/pip.conf', 'restored /etc/apt/sources.list.d/debian.sources'], 900]],
  null, { after: () => { mockState.mirror = { pypi: m.pypi || '', pipFallback: m.pypiFallback || '', debian: m.debian || '', debianFallback: m.debian ? [...(m.debianFallback || []), 'http://deb.debian.org/debian'] : [] }; } }),
  MirrorTest: (b) => {
    const op = nid();
    setTimeout(async () => {
      emitOp(op, { type: 'begin', kind: 'mirrortest', board: b });
      for (const r of MIRROR_RESULTS) { await sleep(280); emitOp(op, { type: 'info', text: r.id, data: r, kind: 'mirrortest', board: b }); }
      const ranked = [...MIRROR_RESULTS].sort((x, y) => y.debian.speed - x.debian.speed);
      emitOp(op, { type: 'end', state: 'ok', result: ranked, kind: 'mirrortest', board: b });
    }, 10);
    return op;
  },
  UploadKey: (b, generate) => simulate('uploadkey', b, '', [['key', 'Upload the SSH public key', [], 700]], () => {
    const created = generate && !mockState.hasKey && new URLSearchParams(location.search).has('nokey');
    if (created) mockState.hasKey = true;
    const added = !mockState.keyUploaded; mockState.keyUploaded = true; return { path: '/Users/me/.ssh/id_ed25519.pub', added, created };
  }),
  Relay: (b, on, up) => simulate('relay', b, '', [['relay', on ? 'Download through this computer while Quay is connected' : 'Download directly again', [on ? "apt downloads through Quay's computer whenever Quay is connected" : 'apt downloads directly again'], 900]],
    null, { after: () => { mockState.relay = on; if (on) mockState.relayUpstream = up || ''; } }),
  RelaySettings: () => ({ upstream: mockState.relayUpstream, effective: mockState.relayUpstream || 'http://127.0.0.1:7890' }),
  SetRelayUpstream: (u) => { mockState.relayUpstream = u; return mock.RelaySettings(); },
  Scan: () => simulate('scan', '', '', [['scan', 'Look for boards', [], 1100]], () => [
    { name: 'bench', host: 'bench.local', ips: ['192.168.2.171'], board: 'unoq', model: 'Arduino UNO Q', serial: '2a7f00c1', version: '0.1' },
    { name: mockState.hostname, host: mockState.hostname + '.local', ips: ['192.168.1.42'], board: 'unoq', model: 'Arduino UNO Q', serial: '2c3e91a7', version: '0.1', saved: mockState.boards.find((x) => x.boardId === '2c3e91a7')?.name || '' },
  ]),
  AddBoard: (t, makeDefault, uploadKey) => simulate('addboard', '', '', [['connect', 'Connect to ' + (t.host || 'USB'), [], 900]], () => {
    const name = t.kind === 'adb' ? 'rig' : (t.host.split('.')[0] || 'board').toLowerCase();
    mockState.boards = mockState.boards.filter((x) => x.name !== name).concat([{ ...t, name, boardId: name === mockState.hostname ? '2c3e91a7' : 'x' + name }]);
    if (makeDefault || !mockState.def) mockState.def = name;
    if (uploadKey) mockState.keyUploaded = true;
    return statusData(name);
  }),
  Rename: (b, n) => simulate('rename', b, '', [['rename', 'Rename the board to ' + n], ['announce', `Announce ${n}.local on the network`, [], 700], ['verify', 'Check the new name']], () => {
    const bd = mockState.boards.find((x) => x.name === b);
    if (bd) { if (bd.host === b + '.local') bd.host = n + '.local'; bd.name = n; }
    if (mockState.def === b) mockState.def = n;
    mockState.hostname = n;
    return statusData(n);
  }),
  SystemCheck: (b, refresh) => simulate('syscheck', b, '', refresh
    ? [['refresh', 'Refresh the package lists', ['Hit:1 http://deb.debian.org/debian trixie InRelease', 'Get:2 https://apt-repo.arduino.cc stable InRelease [3,141 B]', 'Fetched 3,141 B in 2s (1,520 B/s)'], 1400], ['list', 'List the available updates', [], 500]]
    : [['list', 'List the available updates', [], 700]], () => systemData()),
  // Download, then install, with progress events like the real one (361 packages, 824 MB).
  SystemUpdate: (b, opt) => {
    const op = nid(), base = { kind: 'sysupdate', board: b };
    const step = (step, state, text) => emitOp(op, { ...base, type: 'step', step, state, text });
    const prog = async (stepKey, total, unit, ms) => {
      for (let i = 1; i <= 40; i++) {
        await sleep(ms / 40);
        const pc = (100 * i) / 40;
        emitOp(op, { ...base, type: 'progress', step: stepKey, data: { percent: pc, done: Math.round((total * pc) / 100), total, unit } });
      }
    };
    setTimeout(async () => {
      emitOp(op, { ...base, type: 'begin' });
      if (!opt.downloadOnly && mockState.apps.some((a) => a.status === 'running')) { step('safety', 'run', 'Check it is safe to interrupt the running apps'); await sleep(300); step('safety', 'ok', 'Check it is safe to interrupt the running apps'); }
      step('refresh', 'run', 'Refresh the package lists'); await sleep(600); step('refresh', 'ok', 'Refresh the package lists');
      if (!mockState.downloaded) {
        step('download', 'run', 'Download the updates (824 MB)'); await prog('download', 863905000, 'bytes', 6000); step('download', 'ok', 'Download the updates (824 MB)');
        mockState.downloaded = true;
      }
      if (opt.downloadOnly) { emitOp(op, { ...base, type: 'end', state: 'ok', result: { remaining: 361, downloaded: true } }); return; }
      step('upgrade', 'run', 'Install the updates'); await prog('upgrade', 361, 'packages', 6000); step('upgrade', 'ok', 'Install the updates');
      step('result', 'run', 'Check the result'); await sleep(300); step('result', 'ok', 'Check the result');
      mockState.upgraded = opt.arduinoOnly ? 'arduino' : opt.skipDesktop ? 'partial' : 'all'; mockState.appcli = '0.14.0';
      emitOp(op, { ...base, type: 'end', state: 'ok', result: { remaining: opt.skipDesktop ? 3 : 0, rebootRequired: false, appcliVersion: 'Arduino App CLI version 0.14.0' } });
    }, 10);
    return op;
  },
  Backup: (b, f) => simulate('backup', b, f, [['backup', 'Back up data/', [], 800]], '/Users/me/Library/Application Support/Quay/backups/' + f + '-9f3a21c7-20261023-102000.tgz'),
  Restore: (b, f) => simulate('restore', b, f, [['check', 'Check the backup'], ['stop', 'Stop'], ['restore', 'Restore data/', ['restored: gamepad-bindings.json zero_pos.json']], ['start', 'Start again', [], 900]]),
  Export: (b, f, p) => simulate('export', b, f, [['export', 'Export', [], 900]], p),
  Erase: (req) => simulate('erase', req.board, req.folder, [['safety', 'Safety check'], ['backup', 'Back up data/'], ['destroy', 'Remove containers'], ['autostart', 'Clear the boot default'], ['uninstall', 'Uninstall hook'], ['remove', 'Delete the folder']], '', { after: () => { mockState.apps = mockState.apps.filter((a) => a.folder !== req.folder); } }),
  Logs: (b, f) => {
    const op = nid();
    let alive = true;
    setTimeout(async () => {
      emitOp(op, { type: 'begin', kind: 'logs', board: b, folder: f });
      const lines = ['INFO  main: loading xgoduck_walk.onnx, xgoduck_getup.onnx, xgoduck_pick.onnx, xgoduck_roulade.onnx', 'INFO  bridge: 2 Mbps, MCU state at 100 Hz', 'INFO  web: serving on :9527', 'INFO  control: policy loop 50.0 Hz, inference 2.1 ms'];
      for (let i = 0; alive && i < 40; i++) { emitOp(op, { type: 'log', text: lines[i % lines.length], kind: 'logs', board: b, folder: f }); await sleep(i < 4 ? 60 : 1200); }
    }, 10);
    mock.Cancel = () => { alive = false; };
    return op;
  },
  PolicyCatalog: () => simulate('policy-catalog', '', '', [[ 'index', 'Read the indexes', [], 600 ]], { entries: MOCK_POLICIES, problems: [], indexes: [duckProfile.policies.index.url, ...(mockState.policyIndexes || [])] }),
  Policies: (b, f) => simulate('policies', b, f, [], () => ({ app: 'xgoduck', folder: f, overlay: false, running: mockState.apps[0].status === 'running',
    slots: duckProfile.policies.slots.map((sl) => { const act = mockState.apps[0].policies[sl.id] || null; return { ...sl, active: act, sha256: act ? act.sha256 : 'stock-' + sl.id, changed: false }; }) })),
  CheckPolicy: (req) => simulate('policy-check', req.board, req.folder, req.entry ? [['download', 'Download ' + req.entry.name, [], 700]] : [], () => {
    const slot = req.entry?.slot || req.slot;
    if (!slot) throw new Error('choose a slot');
    const bad = /bad/i.test(req.file || '');
    return { slot, sha256: req.entry?.sha256 || 'b'.repeat(64), size: req.entry?.size || 801234, same: mockState.apps[0].policies[slot]?.sha256 === (req.entry?.sha256 || 'b'.repeat(64)),
      model: { inputs: [{ name: 'obs', shape: [bad ? 62 : 1, bad ? 62 : 61] }], outputs: [{ name: 'actions', shape: [1, 14] }], metadata: {} },
      problems: bad ? ['input obs is [62,62], the app feeds [1,61]', 'metadata joint_names is missing (the stock model has "left_hip_yaw,…")'] : [], file: req.file || '/Users/me/Library/Caches/Quay/policies/models/x.onnx' };
  }),
  InstallPolicy: (req) => {
    const slot = req.entry?.slot || req.slot, running = mockState.apps[0].status === 'running';
    return simulate('policy', req.board, req.folder, [
      ...(req.entry ? [['download', 'Download ' + req.entry.name + ' (checksum verified)', [], 500]] : []),
      ['model', 'Check the model', [], 400], ['upload', 'Upload', [], 500],
      ...(running ? [['safety', 'Check it is safe to stop XGO Duck', [], 300], ['stop', 'Stop XGO Duck', ['App stopped'], 500]] : []),
      ['switch', 'Put the model in place', [], 400], ...(running ? [['start', 'Start XGO Duck with the new model', ['App started'], 1200]] : []),
    ], null, { after: () => { mockState.apps[0].policies = { ...mockState.apps[0].policies, [slot]: { id: req.entry?.id || '', name: req.entry?.name || req.name || 'model', author: req.entry?.author || req.author || '', sha256: req.entry?.sha256 || 'b'.repeat(64), size: 800000, source: req.entry ? '' : (req.file || '').split('/').pop(), installedAt: new Date().toISOString() } }; } });
  },
  RestorePolicies: (b, f, slots) => simulate('policyrestore', b, f, [['safety', 'Check it is safe to stop XGO Duck'], ['stop', 'Stop XGO Duck'], ['stock', 'Put the stock models back'], ['start', 'Start XGO Duck again', [], 900]], null, {
    after: () => { const p = { ...mockState.apps[0].policies }; (slots.length ? slots : Object.keys(p)).forEach((k) => delete p[k]); mockState.apps[0].policies = p; } }),
  PickPolicyFile: () => '/Users/me/Downloads/model_5500.onnx',
  SetPolicyIndexes: (urls) => { mockState.policyIndexes = urls; },
  CheckUpdates: (apps) => simulate('check-updates', '', '', [], apps.filter((a) => a.deploy || a.profile?.source).map((a) => {
    const latest = a.folder.startsWith('xgo') ? '8837d5a3bc7cf3746f6da2b55c7b9191026b9838' : a.deploy.commit;
    const u = { folder: a.folder, repo: a.deploy?.repo || a.profile.source.repo, latest, commits: -1, unknown: !a.deploy,
      subject: a.folder.startsWith('xgo') ? 'Add roller walking controls and simplify the user guide' : a.deploy?.subject, date: '2026-10-23T10:14:19Z' };
    if (a.deploy) { u.behind = a.deploy.commit !== latest; u.commits = u.behind ? 3 : 0; }
    return u;
  })),
};
