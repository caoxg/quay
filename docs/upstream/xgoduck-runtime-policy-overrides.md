# Proposal for xgoduck_runtime_arduino: load user-installed policies from `data/policies/`

*Draft of an issue (or a pull request) for [LuwuDynamics/xgoduck_runtime_arduino](https://github.com/LuwuDynamics/xgoduck_runtime_arduino). Written against commit 8837d5a.*

## Problem

The runtime loads its five models from `python/` at start (`policy.py`, `controller.py`). To try a model someone else trained, a user has to overwrite a file in `python/`. That causes three problems:

- **Updates undo it.** Every reinstall or update (App Lab, Duck Setup, git) puts the stock file back.
- **A bad file stops the robot.** If a model fails to load (wrong shape, damaged download), `Controller.__init__` raises and the app does not start at all.
- **Nobody can tell what is loaded.** `/api/status` reports file names, not whether a model is stock or user-installed.

## Proposal

Look in the app's `data/policies/` before `python/`. `data/` is the folder every installer already keeps across updates, and backups include it.

1. **Lookup order.** For each model name, use `data/policies/<name>` if it exists, else `python/<name>`.
2. **Fallback.** If a model from `data/policies/` fails to load, log why and load the stock one, so the robot keeps working.
3. **Report the source** of each model in `/api/status`, e.g. `"models": {"xgoduck_walk.onnx": "data/policies"}`.

Removing a file from `data/policies/` restores the stock model. No stock copy is needed, and code files are never modified.

## Patch (sketch)

```diff
--- a/python/policy.py
+++ b/python/policy.py
@@
 PYTHON_DIR = Path(__file__).resolve().parent
+# Models the user installed live in the app's data/policies/, which updates and backups keep. A file
+# there takes the place of the stock model with the same name in python/.
+OVERRIDE_DIR = PYTHON_DIR.parent / 'data' / 'policies'
+
+def model_path(name: str) -> Path:
+    p = OVERRIDE_DIR / name
+    return p if p.is_file() else PYTHON_DIR / name
 
 def default_onnx() -> Optional[Path]:
-    p = PYTHON_DIR / 'xgoduck_walk.onnx'
+    p = model_path('xgoduck_walk.onnx')
     return p if p.is_file() else None
@@ def load(self, path=None, *, use_model_home=False):
         p = Path(path) if path else default_onnx()
         if p is None:
             return False, 'default model xgoduck_walk.onnx was not found'
-        if not p.is_file():
-            p = PYTHON_DIR / p.name
+        if not p.is_absolute() or not p.is_file():
+            p = model_path(p.name)
@@
-            self.file = p.name
+            self.file = p.name
+            self.source = 'data/policies' if p.parent == OVERRIDE_DIR else 'python'
```

```diff
--- a/python/controller.py
+++ b/python/controller.py
@@
-from policy import Policy
+from policy import Policy, OVERRIDE_DIR, PYTHON_DIR
@@
+def load_model(policy, name, **kw):
+    """Load name from data/policies/ or python/; fall back to the stock model if an installed one fails."""
+    ok, message = policy.load(name, **kw)
+    if not ok and (OVERRIDE_DIR / name).is_file():
+        ok, stock = policy.load(PYTHON_DIR / name, **kw)
+        message = f'{name} from data/policies failed ({message}); using the stock model: {stock}'
+    return ok, message
@@ class Controller:
-        ok, message = self.roller_policy.load('xgoduck_roller.onnx', use_model_home=True)
+        ok, message = load_model(self.roller_policy, 'xgoduck_roller.onnx', use_model_home=True)
```

The same change applies to the walk, get-up, pick and roulade loads. `status()` adds:

```python
models={p.file: p.source for p in (self.walk_policy, self.roller_policy, self.getup_policy, self.pick_policy, self.roulade_policy)},
```

## Compatibility

- Nothing changes when `data/policies/` is absent.
- `data/` is mounted with the rest of the app folder, so the container needs no extra mount.
- Tools stay independent of the runtime's internals. A tool only writes or deletes `data/policies/<name>.onnx` and restarts the app. Quay ([policy support](https://github.com/caoxg/quay)) would switch its built-in profile to:

  ```yaml
  policies:
    dir: data/policies
    stock_dir: python
  ```

  and stop replacing files in `python/`.

## 中文摘要

建议运行时先从 App 的 `data/policies/` 加载同名模型，找不到再用 `python/` 里的原版。

- 用户装的模型保存在 `data/` 里，更新和备份都会保留。
- 加载失败时自动退回原版，不会因为一个坏模型导致整个 App 启动不了。
- `/api/status` 报告每个模型的来源。
- 删掉 `data/policies/` 里的文件，就恢复原版。

工具（例如 Quay）只需要写入或删除这个目录里的文件，不必再改动代码目录。
