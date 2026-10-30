
---

### macOS: opening Quay the first time

Quay is not signed and notarized with an Apple Developer ID yet, so macOS stops it the first time you open it ("Quay" cannot be opened, or Apple cannot check it for malicious software). To open it:

1. Double-click Quay once. When macOS refuses, click **Done** (not *Move to Trash*).
2. Open **System Settings → Privacy & Security**, scroll down to **Security**, and click **Open Anyway** next to the line about Quay.
3. Click **Open Anyway** again and confirm with your password or Touch ID.

Or run this once in Terminal: `xattr -dr com.apple.quarantine /Applications/Quay.app`

You do this once per download; updates that Quay installs by itself open without it. Windows may show SmartScreen: choose **More info → Run anyway**. This note is on every release until Quay is signed.

### macOS：第一次打开 Quay

Quay 还没有用 Apple 开发者证书签名和公证，所以第一次打开时 macOS 会拦住它（提示「无法打开 Quay」，或「Apple 无法检查其是否包含恶意软件」）。打开方法：

1. 先双击打开一次 Quay。被拦住时点「完成」（不要点「移到废纸篓」）。
2. 打开「系统设置 → 隐私与安全性」，往下拉到「安全性」，在关于 Quay 的那一行旁边点「仍要打开」。
3. 再点一次「仍要打开」，输入密码或用触控 ID 确认。

也可以在「终端」里运行一次：`xattr -dr com.apple.quarantine /Applications/Quay.app`

每次下载只需要做一次；Quay 自己安装的更新不需要再做。Windows 可能弹出 SmartScreen，选「更多信息 → 仍要运行」。在 Quay 完成签名之前，每个版本都会附上这段说明。
