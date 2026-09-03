# codex-429-autoban-monthly

一个 CPA（CLIProxyAPI）插件：**Codex 凭证收到 429（限流）后自动禁用，并在对应限额窗口刷新后自动解禁。**

## 它做什么

1. **检测 429**：每次请求完成后，插件观察用量记录。如果某个 **codex** 凭证收到了 429，就触发禁用逻辑。
2. **判断禁多久**：读上游 OpenAI 返回的 `x-codex-*` 响应头，使用实际的 `reset-at` 时间。支持 **5 小时、周限额、Free tier 月限额**及未知的 provider-specific 窗口。
   - 5 小时窗口满了 → 5 小时刷新后解禁
   - 周窗口满了 → 下周刷新时才解禁
   - 月窗口满了 → 月限额刷新时才解禁
   - 两个都满 → 按较晚的窗口解禁
3. **自动解禁**：之后每次 CPA 选凭证时，插件把"还没到解禁时间"的凭证从候选里剔除；一旦过了刷新时间，自动放回候选。
4. **手动加回号池**：如果你在 Codex 侧手动重置额度或使用了重置卡，可以通过插件的 Management API / 资源页立即解除插件内存里的 ban，不必等原来的 `reset-at`。
5. **只管 codex**：非 codex 凭证一律不干预，交给 CPA 原有逻辑。

## 怎么判断限额窗口

OpenAI 的 ChatGPT/Codex 后端在 429 时会返回一组自定义头（不是标准的 `x-ratelimit-*`）。插件优先使用实际 `reset-at`，不把未知窗口强行映射成 5 小时：

| 响应头 | 含义 |
|---|---|
| `x-codex-primary-window-minutes` | `300` = 5 小时；`40320`–`44640` = 28–31 天月窗口；其他值也支持 |
| `x-codex-primary-reset-at` | 对应窗口刷新时间（Unix 秒或 RFC3339） |
| `x-codex-primary-used-percent` | 窗口使用率（打满时 = 100） |
| `x-codex-secondary-window-minutes` | `10080` = 7 天（周）窗口；其他值也支持 |
| `x-codex-secondary-reset-at` | 对应窗口刷新时间（Unix 秒或 RFC3339） |
| `x-codex-secondary-used-percent` | 窗口使用率 |

**判断逻辑**：哪个窗口到 100，就用那个窗口的 `reset-at`；如果两个都满，使用较晚的 `reset-at`。

> 如果 429 响应里没有 reset-at 头，插件保守地按 31 天禁用；不会再错误地按 5 小时解禁 Free tier 月限额账号。

## 为什么解禁不需要定时器

CPA 插件机制是**事件驱动**的，没有后台定时器。所以解禁用"惰性"方式实现：每次有新请求来、CPA 要选凭证时，插件顺手检查"现在过了解禁时间没"——过了就放回候选。效果等同于定时解禁，且不需要额外的唤醒机制。

## Management Panel / Plugin Store install

This repo ships a `registry.json`. After a GitHub release is published, it can be used as a custom CPA plugin-store source. Add this URL to `plugins.store-sources`, then restart or refresh the management panel and search for the plugin in the store:

```yaml
plugins:
  enabled: true
  store-sources:
    - https://raw.githubusercontent.com/ysxk/codex-429-autoban/main/registry.json
```

If you want every CPA user to see it without adding a custom source, submit the same registry entry to the official `router-for-me/CLIProxyAPI-Plugins-Store` repository.

## 安装

### 1. 准备 C 编译器（CGO 必需）

CPA 插件是原生动态库，必须用 CGO 编译，所以需要 C 编译器。Windows 上装 MinGW-w64：

```powershell
winget install -e --id MartinStorsjo.LLVM-MinGW.UCRT
```

装完确认 `gcc --version` 能输出版本。

### 2. 编译

```powershell
cd codex-429-autoban
.\build.ps1            # Windows
# 或
bash build.sh          # 任意平台
```

成功后会生成 `codex-429-autoban.dll`（Windows）。

> 本插件把 CPA 的 `sdk/pluginabi`、`sdk/pluginapi` 两个包**本地化**到 `cpasdk/` 目录，因此**不需要** Go 1.26（CPA 主程序才需要），Go 1.21+ 即可编译。

### 3. 放到 CPA 插件目录

CPA 在 Windows amd64 上按顺序查找：
```
plugins/windows/amd64-<variant>/
plugins/windows/amd64/
plugins/
```

把 dll 放进去即可（推荐 `plugins/windows/amd64/codex-429-autoban.dll`）。

**插件 ID = 文件名去掉扩展名**，即 `codex-429-autoban`。

### 4. 在 config.yaml 启用

```yaml
plugins:
  enabled: true
  configs:
    codex-429-autoban:
      enabled: true
      priority: 100   # 数字越大越先执行；建议设高一点，让禁用判断先于其他调度插件
```

> 如果你的 CPA 二进制不支持插件，响应头里不会有 `httpX-CPA-SUPPORT-PLUGIN: 1`。需要用 CGO 编译版的 CPA。

## 手动加回号池（Codex 重置额度/重置卡后）

CPA 插件没有“Codex 已手动重置额度”的事件回调，所以插件无法可靠自动感知你在 Codex 侧用了重置卡。为了解决这个问题，插件提供了 Management API 和一个资源页来**手动解除 ban**。

资源页（在 CPA 管理界面的插件菜单里也会出现）：

```text
/v0/resource/plugins/codex-429-autoban/status
```

API（需要 CPA 管理密钥，支持 `Authorization: Bearer <key>` 或 `X-Management-Key`）：

```bash
# 查看当前被插件排除的账号
curl -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://localhost:8317/v0/management/plugins/codex-429-autoban/bans

# 将单个账号加回号池
curl -X POST -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"auth_id":"<AUTH_ID>"}' \
  http://localhost:8317/v0/management/plugins/codex-429-autoban/unban

# 清空全部插件 ban 状态
curl -X POST -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://localhost:8317/v0/management/plugins/codex-429-autoban/unban-all
```

注意：这里清除的是本插件的**内存 ban 状态**。请只在你确认 Codex 侧额度已经恢复（例如手动重置额度/使用重置卡）后使用，否则账号可能马上再次 429 并被重新 ban。

## 工作流程图

```
请求完成 → usage.handle（插件观察）
  │
  ├─ 不是 codex / 不是 429 → 跳过
  └─ 是 codex 且 429
        ├─ 读 x-codex-* 头，判断 5h 还是周限额
        └─ 记录：该凭证"到 X 时间才能再用"

下次有请求来选凭证 → scheduler.pick（插件介入）
  ├─ 剔除"还没到解禁时间"的 codex 凭证
  ├─ 已过解禁时间的 → 放回候选（自动解禁）
  └─ 全部凭证都可用 → 交给 CPA 原本的轮询选；有凭证被禁 → 插件从可用凭证里按优先级挑一个
```

## 状态说明

- 禁用状态保存在**插件进程内存**里。CPA 重启后会清空（重启同时也清了 CPA 自己的冷却状态，所以一致）。
- 如果你通过 Management API 手动加回号池，清除的也是这份插件内存状态；不会修改 Codex 侧额度，也不会修改 CPA 凭证文件。
- 日志：禁用/解禁都会通过 CPA 的日志输出（`slog`），关键字 `codex-429-autoban:`。

## 文件说明

| 文件 | 作用 |
|---|---|
| `main.go` | 插件主代码（usage.handle 检测 + scheduler.pick 过滤 + Management API 手动解禁） |
| `cpasdk/pluginabi/` | CPA 插件 ABI 常量（本地化，免 Go 1.26） |
| `cpasdk/pluginapi/` | CPA 插件类型定义（本地化） |
| `build.ps1` / `build.sh` | 编译脚本 |
