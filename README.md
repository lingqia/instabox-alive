# instabox-keepalive (Go 单二进制 + Web 前端)

InstaBox（https://instabox.sh ，dashboard: https://dashboard.instabox.sh/ ）免费机保活程序。
纯 Go 标准库，**编译产物是单个二进制**（Web 前端已内嵌），无任何第三方依赖。

> 背景：免费机 **30 分钟空闲自动暂停**，且每次连续运行有 **max-run 上限**
> （默认 480 分钟 = 8 小时绝对上限；开了 dynamic runtime 时空跑窗口只有 2 小时）。
> 撞上 max-run 被动暂停会产生 **5→20→120+ 分钟递增 cooldown**，之后才准唤醒。
> 本程序在官方 API 的规则内把这些坑提前绕开。

## 功能

| 功能 | 说明 |
|---|---|
| 活动租约续期 | 每 2 分钟 `PUT /v1/machines/{id}/activity-leases/{leaseID}` 续租，机器一直"忙"，30 分钟空闲暂停不触发 |
| 每 6h 主动重启 | 撞上限之前主动 `suspend→resume`（保留进程）或 `stop→start`。**自己发起的暂停不欠 cooldown 且重置 max-run 计数** |
| deadline 感知 | 按 `runStartedAt + maxRunMinutes` 算实际死线，死线前 10 分钟提前重启（覆盖 dynamic runtime 的 2h 动态窗口） |
| 自动恢复 | 自暂停→`wake`，手动暂停→`resume`，stopped→可选 `start`，cooldown 期间老实等待，恢复后自动重挂租约 |
| Web 控制台 | 状态卡 + 6h/8h 双进度条 + 「立即保活」「立即重启」按钮 + 实时日志，浅色/深色主题 |
| Mock 模式 | `-mock` 内置离线模拟 API，本地即可完整验证（无需真实 key） |
| 通知钩子 | `-notify-cmd` 状态变化执行任意 shell（可接 ntfy / Telegram / 企业微信…） |

**程序有意不做的事**：不伪造负载骗 dynamic runtime 的"工作分钟"延时；机器处于 abuse/安全锁（`abuseLock`）时完全不动作、只通知你去申诉。

## 能保到什么程度（实话实说）

- 机器有真实负载（服务/流量/agent 在干活）→ 窗口按文档规则延长，6h 主动重启使其几乎不间断（pause→resume 秒级、保留进程）。
- 机器纯空跑 → dynamic runtime 窗口 2h 到点，靠 deadline 感知提前重启续命，同样接近不间断。
- **但**：这是免费版规则内的合规利用，服务端随时可能改规则。这台机器对你重要就**付费**（Pro/Builder 的 `maxRunMinutes=-1`、idle 可调，根本不需要保活）。

## 项目结构

```
instabox-keepalive-go/
├── main.go          # 装配：config → client → loop + web
├── config.go        # env + flag 双通道配置
├── api_types.go     # 类型：错误信封 / Machine / JSON 解析
├── api_client.go    # HTTP 核心（幂等键、错误解析、重试信息）
├── api_ops.go       # 端点：机器查询 / 租约 / wake / suspend / resume / start / stop / 操作轮询
├── loop.go          # 核心状态机：6h 重启、8h 上限、自动恢复 + 环形日志 + 状态快照
├── web.go           # Web 路由：/ /api/status /api/log /api/action（内嵌前端，可选 token 门禁）
├── web/index.html   # 前端：Notion 风格，浅/深双主题
├── mock.go          # 内置离线 mock API（-mock）
├── mock/            # Python 版 mock + 场景冒烟测试
├── instabox-keepalive.service  # systemd 单元
└── README.md
```

## 构建

需要 Go 1.22+（无依赖，无需联网拉包）：

```bash
cd instabox-keepalive-go
go build -ldflags "-s -w" -o keepalive .     # 单文件二进制
# 交叉编译到 VPS / instabox 机器：
GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o keepalive .
```

## 运行

### 本地先看效果（mock 模式，无需 key）

```bash
./keepalive -mock
# 打开 http://localhost:8080
```

### 真实保活

```bash
export INSTABOX_API_KEY=***        # dashboard → API keys（scope: machines:read + machines:write）
export INSTABOX_MACHINE=mch_xxx    # 机器 ID 或精确名字
./keepalive                        # Web 控制台 http://<host>:8080
```

### 常用参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-machine` | env `INSTABOX_MACHINE` | 机器 ID 或精确名字 |
| `-api-key` | env `INSTABOX_API_KEY` | **建议用环境变量**，避免出现在 ps |
| `-mock` | | 内置离线模拟，本地验证/演示 |
| `-interval` | `2m` | 轮询/续租间隔（须小于 `-ttl`） |
| `-ttl` | `300` | 租约 TTL 秒（30-3600） |
| `-restart-every` | `6h` | **每 6h 主动重启**；`0` 关闭 |
| `-deadline-margin` | `10m` | 死线前这么久提前重启（覆盖 2h 动态窗口）；`0` 关闭 |
| `-restart-mode` | `pause` | `pause`=suspend+resume 保留进程；`stop`=整机重启 |
| `-auto-resume` / `-auto-start` | `true` / `false` | 自动唤醒 / 自动启动 |
| `-web-addr` | `:8080` | Web 控制台监听地址；空关闭 |
| `-web-token` | env `INSTABOX_WEB_TOKEN` | Web 访问令牌（**公网暴露时强烈建议**，URL 加 `?token=...`） |
| `-notify-cmd` | | 状态变化执行的 shell（`IB_EVENT/IB_MESSAGE/...`） |
| `-once` | | 只跑一轮退出（配 cron） |
| `-restart-now` | | 本轮强制重启（cron：`0 */6 * * * keepalive --once --restart-now`） |
| `-verbose` | | 打印每次续租/跳过原因 |
| `-log-file` | | 日志追加到文件 |

## 部署

### A. 放进 instabox 机器里跑（推荐，前端就挂在机器的公网端口上）

```bash
# 本机交叉编译
GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o keepalive .

# 传进机器（instabox 自带 ssh / 文件接口）
instabox cp keepalive mch_xxx:/data/home/keepalive
instabox exec mch_xxx 'chmod +x /data/home/keepalive'

# 机器里跑起来（key 建议用 Org Secrets 或环境变量注入，别写进脚本）
instabox exec mch_xxx 'INSTABOX_API_KEY=*** INSTABOX_MACHINE=mch_xxx INSTABOX_WEB_TOKEN=changeme \
  nohup /data/home/keepalive > /data/home/keepalive.log 2>&1 &'

# dashboard → 机器 → 端口，把 8080 发布成公网 URL，浏览器打开
# https://<公开URL>/?token=changeme
```

⚠️ 注意：程序跑在机器**内部**时，机器被暂停它也会被冻结（租约在手通常不会被暂停）。
若要"机器彻底暂停后也能自动拉起"，把守护进程放到**外部**（本机/VPS/另一台机器）跑。

### B. 本机 / VPS 常驻（systemd）

```bash
sudo cp keepalive /usr/local/bin/
sudo cp instabox-keepalive.service /etc/systemd/system/
echo 'INSTABOX_API_KEY=***' | sudo tee /etc/instabox-keepalive.env
echo 'INSTABOX_MACHINE=mch_xxx' | sudo tee -a /etc/instabox-keepalive.env
sudo chmod 600 /etc/instabox-keepalive.env
sudo systemctl daemon-reload && sudo systemctl enable --now instabox-keepalive
journalctl -u instabox-keepalive -f
```

### C. 纯 cron（不想常驻）

```cron
*/2 * * * *  INSTABOX_API_KEY=*** INSTABOX_MACHINE=mch_xxx /usr/local/bin/keepalive --once --web-addr ''
0 */6 * * *  INSTABOX_API_KEY=*** INSTABOX_MACHINE=mch_xxx /usr/local/bin/keepalive --once --restart-now --web-addr ''
```

## Web 控制台

- `GET /`：控制台（内嵌前端，浅/深主题，3 秒自动刷新）
- `GET /api/status`：状态 JSON（state、run 年龄、maxRunMinutes、deadline、cooldown、租约、下次重启倒计时…）
- `GET /api/log?since=N`：增量日志（环形缓冲 1000 条）
- `POST /api/action`：`{"action":"keepalive"}` 或 `{"action":"restart"}`
- 设了 `-web-token` 后所有请求带 `X-Keepalive-Token` 头或 `?token=`

## 测试

```bash
go build -o keepalive . && python3 mock/run_tests.py   # 6 个场景：续租 / 6h 重启 / 动态窗口提前重启 / wake / cooldown 等待 / auto-start
./keepalive -mock -once                                 # 内置 mock 单轮自检
```

## 免费版机制速查

| 机制 | 行为 | 本程序对策 |
|---|---|---|
| 空闲暂停 | 30 分钟无活动自动暂停（免费固定） | 租约续期，保持"忙" |
| max-run（静态） | 每 run 最多 480 分钟（8h） | 6h 主动重启，永远赶在前面 |
| dynamic runtime | 新 run 先 2h 窗口，按前 30 分钟真实工作量延长；空跑 2h 到点必停；**8h 绝对上限** | 死线前 10 分钟主动重启换新窗口 |
| max-run cooldown | 被动撞上限后 5-20 → 60-75 → 120-135 分钟递增冷却 | 主动重启不产生 cooldown 且重置计数；遇到 cooldown 严格等待 |
| 手动 pause/resume | 不欠 cooldown、重置计数、保留进程 | 就是主动重启用的机制 |
| abuse/安全锁 | 可达 24 小时 | **不绕过**，通知去申诉 |

## 免责与礼貌

- 全部走官方公开 REST API（文档 https://instabox.sh/docs ），无逆向、无漏洞利用。
- 服务条款与配额以官方为准；规则变化时行为可能失效（程序会降级而不是乱试）。
- InstaBox 对你有用的话，**付费支持他们**。
