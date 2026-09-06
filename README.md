# codex-429-autoban

一个 CPA（CLIProxyAPI）插件：**Codex 凭证收到 429（限流）后自动禁用，并在对应限额窗口刷新后自动解禁。**

## 它做什么

1. **检测 429**：每次请求完成后，插件观察用量记录。如果某个 **codex** 凭证收到了 429，就触发禁用逻辑。
2. **判断禁多久**：读上游 OpenAI 返回的 `x-codex-*` 响应头，判断是 **5 小时窗口**被打满，还是 **周限额**被打满，并取对应窗口的刷新时间作为解禁时间。
   - 5 小时窗口满了 → 5 小时刷新后解禁
   - 周窗口满了 → 下周刷新时才解禁
   - 两个都满 → 按较晚的（周）解禁
3. **自动解禁**：后台每 60 秒清理一次到期 ban；选凭证时也会额外惰性检查，到期即放回候选。
4. **保持 first-fill**：没有 ban 时完全交给 CPA 原生调度；存在 ban 时，插件按 CPA `fill-first` 的实际规则选择“最高优先级中 AuthID 最小”的可用凭证。
5. **手动加回号池**：如果你在 Codex 侧手动重置额度或使用了重置卡，可以通过插件的 Management API / 资源页立即解除持久化的 ban，不必等原来的 `reset-at`。
6. **只管 codex**：非 codex 凭证一律不干预，交给 CPA 原有逻辑。

## 怎么判断 5 小时还是周限额

OpenAI 的 ChatGPT/Codex 后端在 429 时会返回一组自定义头（不是标准的 `x-ratelimit-*`）：

| 响应头 | 含义 |
|---|---|
| `x-codex-primary-window-minutes` | `300` = 5 小时窗口 |
| `x-codex-primary-reset-at` | 5 小时窗口刷新时间（Unix 秒） |
| `x-codex-primary-used-percent` | 5 小时窗口使用率（打满时 = 100） |
| `x-codex-secondary-window-minutes` | `10080` = 7 天（周）窗口 |
| `x-codex-secondary-reset-at` | 周窗口刷新时间（Unix 秒） |
| `x-codex-secondary-used-percent` | 周窗口使用率 |

**判断逻辑**：哪个窗口的 `used-percent` 到了 100，就用那个窗口的 `reset-at` 作为解禁时间。

> 如果 429 响应里没有这些头（少数情况，比如来自中间代理的伪 429），插件保守地按 5 小时禁用（这是更常见的情形）。

## 解禁与调度边界

插件在进程内启动 60 秒轮询，删除已经到 `reset_at` 的持久化 ban；调度路径也保留惰性清理作为兜底。

CPA 当前的 scheduler plugin API 不支持“仅排除若干 AuthID 后再运行当前原生策略”。因此：无 ban 时插件返回 `Handled:false`，CPA 的全部原生策略保持不变；存在 ban 且 CPA 使用 `fill-first` 时，插件精确复刻该策略的最高优先级、同级 AuthID 字典序选择规则。若未来改用其他策略（如 weighted round-robin），plugin-only 不能无损复刻其内部状态。

如果本次路由的**全部**候选都被插件 ban，现有 plugin API 也无法把“空候选集”交回 CPA；插件只能返回 `Handled:false` 让 CPA 走原本的不可用/重试处理。因此这个极端场景无法在 plugin-only 下严格 fail-closed。

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

注意：这里清除的是本插件持久化的 **ban 状态**。请只在你确认 Codex 侧额度已经恢复（例如手动重置额度/使用重置卡）后使用，否则账号可能马上再次 429 并被重新 ban。

## 工作流程图

```
请求完成 → usage.handle（插件观察）
  │
  ├─ 不是 codex / 不是 429 → 跳过
  └─ 是 codex 且 429
        ├─ 读 x-codex-* 头，判断 5h 还是周限额
        └─ 原子写入磁盘：该凭证"到 X 时间才能再用"

后台每 60 秒轮询一次
  └─ 到了解禁时间的凭证 → 从状态文件删除并自动解禁

下次有请求来选凭证 → scheduler.pick（插件介入）
  ├─ 剔除"还没到解禁时间"的 codex 凭证
  ├─ 已过解禁时间的 → 放回候选（额外的惰性自动解禁）
  └─ 全部凭证都可用 → 完全交给 CPA 原生调度；有凭证被禁 → 按 first-fill 的“最高优先级、同级最小 AuthID”选择
```

## 状态说明

- 禁用状态持久化在 CPA 工作目录的 `data/codex-429-autoban/bans.json`；状态文件权限为 `0600`，目录为 `0700`。CPA 重启后会恢复尚未到期的 ban。
- 插件每 60 秒轮询清理过期 ban；调度时也会再次惰性清理，因此不需要等到下一次轮询才会恢复可用。
- 如果你通过 Management API 手动加回号池，清除的也是这份持久化状态；不会修改 Codex 侧额度，也不会修改 CPA 凭证文件或其刷新行为。
- 状态文件损坏时，插件会保留原文件、记录告警并以空 ban 表继续启动；需要人工检查或删除该文件。
- 日志：禁用/解禁都会通过 CPA 的日志输出（`slog`），关键字 `codex-429-autoban:`。

## 文件说明

| 文件 | 作用 |
|---|---|
| `main.go` | 插件主代码（usage.handle 检测 + scheduler.pick 过滤 + Management API 手动解禁） |
| `cpasdk/pluginabi/` | CPA 插件 ABI 常量（本地化，免 Go 1.26） |
| `cpasdk/pluginapi/` | CPA 插件类型定义（本地化） |
| `build.ps1` / `build.sh` | 编译脚本 |
