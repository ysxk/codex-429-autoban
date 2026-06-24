# codex-429-autoban

一个 CPA（CLIProxyAPI）插件：**Codex 凭证收到 429（限流）后自动禁用，并在对应限额窗口刷新后自动解禁。**

## 它做什么

1. **检测 429**：每次请求完成后，插件观察用量记录。如果某个 **codex** 凭证收到了 429，就触发禁用逻辑。
2. **判断禁多久**：读上游 OpenAI 返回的 `x-codex-*` 响应头，判断是 **5 小时窗口**被打满，还是 **周限额**被打满，并取对应窗口的刷新时间作为解禁时间。
   - 5 小时窗口满了 → 5 小时刷新后解禁
   - 周窗口满了 → 下周刷新时才解禁
   - 两个都满 → 按较晚的（周）解禁
3. **自动解禁**：之后每次 CPA 选凭证时，插件把"还没到解禁时间"的凭证从候选里剔除；一旦过了刷新时间，自动放回候选。
4. **只管 codex**：非 codex 凭证一律不干预，交给 CPA 原有逻辑。

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

## 为什么解禁不需要定时器

CPA 插件机制是**事件驱动**的，没有后台定时器。所以解禁用"惰性"方式实现：每次有新请求来、CPA 要选凭证时，插件顺手检查"现在过了解禁时间没"——过了就放回候选。效果等同于定时解禁，且不需要额外的唤醒机制。

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
- 日志：禁用/解禁都会通过 CPA 的日志输出（`slog`），关键字 `codex-429-autoban:`。

## 文件说明

| 文件 | 作用 |
|---|---|
| `main.go` | 插件主代码（usage.handle 检测 + scheduler.pick 过滤） |
| `cpasdk/pluginabi/` | CPA 插件 ABI 常量（本地化，免 Go 1.26） |
| `cpasdk/pluginapi/` | CPA 插件类型定义（本地化） |
| `build.ps1` / `build.sh` | 编译脚本 |
