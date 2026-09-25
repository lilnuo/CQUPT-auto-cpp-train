# CQUPT 刷题服务（v1.3.0 新增）

把刷题流程从「终端里跑一次」变成「提交任务 → 后台排队执行 → 查进度」。

CLI 仍然保留（根目录 `go run .`），两者**共用** `config` / `ai` / `train` 三层，
只有入口不同：CLI 可以停下来问用户、可以等人手动过验证码；
服务是无人的，配置只能来自环境变量，缺一项就直接起不来。

---

## 为什么需要排队（而不是直接并发跑）

worker 是**串行**的，一次只跑一个任务。这不是没来得及做并发，是四条物理约束：

1. Chrome 对同一个 profile 目录有单实例锁。两个进程共用 profile 时，
   后启动的那个只会把网址丢给已有实例然后自己退出——调试端口没开，CDP 根本连不上。
2. CDP 调试端口是固定的一个。并发就得分配端口并管理其生命周期。
3. 同一个学号只能有一个登录会话。同一账号在两个浏览器里同时登录，
   后登录的会把前一个踢下线，两边都乱套。
4. 流程本身很重：起真实浏览器、等 20 秒过 WAF 挑战、跑 OCR、逐题调大模型。
   单机并发两三个就已经不是 CPU 瓶颈，而是「站点会不会判定异常」的问题了。

所以正确的做法不是在 worker 里硬塞并发，而是**让请求方排队**：
HTTP 立刻返回任务 id，客户端拿 id 轮询进度。用户体感是「提交即返回」。

---

## 一、准备数据库

需要 MySQL 8.0+（用的 `SKIP LOCKED`、`NOW(3)`、递归 CTE 都是 8.0 起才有的）。

```bash
mysql -u root -p < server/schema.sql
```

这会建 `cqupt_train` 库、三张表，以及应用账号 `cqupt_app`（**只给 DML 权限，不给 DDL**）。

用应用账号连的原因：万一存在 SQL 注入，攻击者也改不了表结构、删不掉库。
代价是以后改 schema 必须用 root 手动执行——这笔交换划算。

---

## 二、配置

全部通过环境变量。**服务端没有任何人可以问，所以缺必填项就直接启动失败**——
带着半截配置跑起来的服务比直接起不来更危险，它会在半夜某个请求上才暴露问题。

| 变量 | 必填 | 默认值 | 说明 |
|------|:----:|--------|------|
| `TASK_SECRET` | ✅ | — | 任务密码的加密密钥（AES-256-GCM），**至少 32 字符** |
| `SERVER_ADDR` | | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_USER` | | `cqupt_app` | |
| `DB_PASSWORD` | | `cqupt_app_dev` | |
| `DB_HOST` | | `127.0.0.1` | |
| `DB_PORT` | | `3306` | |
| `DB_NAME` | | `cqupt_train` | |
| `WORKER_ID` | | 主机名-进程号 | 写进 `tasks.locked_by`，多机部署时应显式指定 |
| `WORKER_POLL_INTERVAL` | | `2s` | 队列空时的轮询间隔 |
| `LOCK_TIMEOUT` | | `20m` | 超过这么久没续租的 running 任务视为僵死；**必须大于 `TASK_MAX_RUNTIME`** |
| `TASK_MAX_RUNTIME` | | `15m` | 单个任务的最长执行时间 |
| `SHUTDOWN_GRACE` | | `30s` | 收到停止信号后等 worker 收尾的最长时间 |
| `LOG_LEVEL` | | `info` | `debug` / `info` / `warn` / `error` |
| `LOG_FORMAT` | | `text` | `text`（本地可读）/ `json`（线上机器解析） |

刷题相关的配置（模型 Key、站点选择器、各类超时、重答阈值）仍走 `.env` / `prompts.json`，
和 CLI 完全一样——服务端启动时也会调 `config.Load()` 和 `ai.InitAI()`。

> `LOCK_TIMEOUT` 与 `TASK_MAX_RUNTIME` 的关系不是"建议"而是"必须"：
> 锁超时更短的话，**还在正常执行**的任务会被当成僵死任务抢走重跑，
> 同一个学号被两个浏览器同时登录，两边都乱套。配错会被 `LoadConfig` 直接拒掉。

生成一个密钥：

```bash
openssl rand -base64 36
```

---

## 三、启动

```bash
export TASK_SECRET="$(openssl rand -base64 36)"
go run ./server
```

或编译成二进制：

```bash
go build -o cqupt-server ./server && ./cqupt-server
```

启动成功的日志长这样：

```
配置加载完成 配置="addr=127.0.0.1:8080 db=cqupt_app@127.0.0.1:3306/cqupt_train worker=xxx-123 轮询=2s 锁超时=20m0s 任务超时=15m0s 密钥=<已隐藏>"
数据库连接正常
worker 启动，开始监听队列 标识=xxx-123
HTTP 服务启动 监听=127.0.0.1:8080
```

启动时会真的 `Ping` 一次数据库。不做这一步的话，数据库连不上时服务照样能起来、
`/healthz` 照样返回 ok、负载均衡器照样往里送流量，然后每个请求都失败。

---

## 四、接口

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/healthz` | 探活。**会真的查一次数据库**，否则数据库挂了探活还是会绿 |
| `POST` | `/api/tasks` | 提交任务，返回 `202 Accepted` + 任务 id |
| `GET` | `/api/tasks?limit=50` | 任务列表 |
| `GET` | `/api/tasks/{id}` | 单个任务（含进度事件时间线） |
| `GET` | `/api/tasks/{id}/results` | 每道题的作答结果 |
| `DELETE` | `/api/tasks/{id}` | 取消任务（**只有排队中的能取消**） |
| `GET` | `/` | HTML 进度页，每 5 秒自动刷新 |

提交任务：

```bash
curl -X POST http://127.0.0.1:8080/api/tasks \
  -H 'Content-Type: application/json' \
  -d '{"username":"2023xxxxxx","password":"你的密码","mode":"quiz","num":10}'
```

```json
{"id":123,"status":"pending","提示":"任务已入队，可用 GET /api/tasks/123 查询进度"}
```

返回 `202` 而不是 `200` 是有意的：`202 Accepted` 的语义是「请求已被接受，但还没处理完」，
这正是队列的语义。返回 200 会让人以为任务已经跑完了。

`mode` 取值与 CLI 一致：`quiz`（默认）/ `progap` / `progapdump`。

参数校验直接复用 `train.Request.Validate()`，**HTTP 层不重新实现一遍**——
两边各写一套，迟早会不一致。

---

## 五、数据模型

三张表，职责分明：

| 表 | 作用 |
|----|------|
| `tasks` | 任务本体**兼队列** |
| `task_results` | 每道题一行（题干/答案/得分/是否通过），`UNIQUE(task_id,pid)` 让重答变成更新 |
| `task_events` | 进度事件流，`UNIQUE(task_id,seq)` 让重复上报幂等 |

`tasks` 既当业务表又当队列，是因为任务量是「每个学生每天几次」而不是「每秒几万条」。
这个量级下 MySQL 的行锁 + `SKIP LOCKED` 完全够用，而且**天然没有「消息入队成功但
业务事务回滚」的双写不一致**——任务和队列状态在同一行、同一个事务里。

抢任务的全部实现就是这一句：

```sql
SELECT id FROM tasks FORCE INDEX (idx_claim)
WHERE status = 'pending' AND available_at <= NOW(3)
ORDER BY id LIMIT 1
FOR UPDATE SKIP LOCKED
```

- `FOR UPDATE` 给选中的行加排他锁，防止两个 worker 同时改同一行；
- `SKIP LOCKED` 遇到已锁住的行**直接跳过而不等锁**。没有它，10 个 worker 来抢只有 1 个能拿到锁，
  另外 9 个全在原地等——而它们等的那一行马上就会被改成 running，等到了也是白等。
  结果是吞吐被锁等待拖垮，worker 越多越慢。
- 索引是 `(status, id)` 而**不是** `(status, available_at, id)`。原因见 `schema.sql` 里索引 1 的注释：
  `available_at` 是范围条件，范围一旦生效索引内就不再按后面的列有序，那个索引其实满足不了
  `ORDER BY id`。这是 v1.3.0 用 `EXPLAIN` 抓出来的错误，实测数据也记在 schema 里。
- `FORCE INDEX` 治的是另一种退化：队列空、表里积着历史任务时，优化器会改成主键全表扫描
  （5000 行读全部、4.6ms），而空队列恰恰是轮询最频繁的时刻。强制走索引后是 0.047ms。

---

## 六、可靠性机制

| 机制 | 解决的问题 |
|------|-----------|
| `available_at` | 重试退避。失败的任务把时间往后挪，抢任务的条件带上 `available_at <= NOW(3)`，不需要额外定时器 |
| 续租心跳 | 正常任务可能跑十几分钟，`locked_at` 一直不变会被误判僵死 → 心跳间隔取 `LOCK_TIMEOUT/3`，下限 10s |
| 锁丢失即中止 | 续租时若发现锁已不在自己手上，主动取消本次执行，避免和接管者同时刷同一个学号。注意**不能只看 `RowsAffected==0`**：它返回的是「实际改变的行数」，同一毫秒内连续续租时新旧 `locked_at` 相等，MySQL 会报 0 行（实测 `Rows matched: 1 / Changed: 0`），那不是丢锁。所以 0 行时要回查一次「锁在谁手里」再下结论 |
| `RecoverStale` | worker 被 `kill -9` 后任务永远停在 running，谁也不会再碰它。没有这个补偿，队列会随每次异常退出**静默漏掉**任务 |
| `FailExhausted` | `RecoverStale` 把用尽重试的任务放回 pending 后会被反复领取失败再放回，死循环。它负责截住 |
| `context.WithoutCancel` | 任务超时后 ctx 已被取消，但「把失败写进数据库」恰恰是超时情况下最必须完成的一步 |
| 收尾校验 `locked_by` | 防止已被回收的任务被原 worker 回头覆盖成成功，把新 worker 的结果冲掉 |
| 密码 AES-256-GCM | 不存明文（真实学生密码泄漏即事故）；不干脆不存（服务一重启，未开始的任务全废） |
| `taskView` 内外结构分离 | `store.Task` 含 `PasswordEnc`，对外视图是另一个结构体——让「泄漏一个字段」在结构上不可能 |

### 密码是怎么处理的

- 明文只在 `handleCreateTask` 里存在一个函数调用的时间，立刻加密，不落库、不进日志；
- 密文格式 `nonce || ciphertext || tag`，每次加密都用 `crypto/rand` 新取 nonce
  （**同一密钥下 nonce 绝不重复**，重复会让攻击者异或还原明文）；
- 选 GCM 而不是 CBC：CBC 只管加密不管防篡改，攻击者能在不知道密钥时翻转密文比特；
- `TASK_SECRET` 没有就不启动，**绝不做「那就先存明文吧」的降级**。

---

## 七、优雅退出

收到 `SIGINT` / `SIGTERM` 后的顺序是：

1. **先停 HTTP**，不再接受新连接，把存量请求（毫秒级）处理完；
2. **再停 worker**：不再领新任务，但**把手上的任务跑完**，然后才退出；
3. 等 worker 超过 `SHUTDOWN_GRACE` 仍没结束，就强制退出——被中断的任务停在 `running`，
   下次启动由锁超时 + `RecoverStale` 捞回来（这条兜底路径本来就必须存在）。

顺序不能反：反过来进程退出时可能还有请求正在写数据库。

**为什么关闭时不直接砍掉任务**：一次刷题要几分钟，砍在中间的话，上游报的是
`context.Canceled`，这次尝试就白费了。让它跑完只是让进程多活几分钟。

---

## 八、已知限制

诚实列出来，避免下一个人踩：

1. **任务在关闭时被硬性中断会丢一次尝试。** 进程被 `kill -9` 或超过 `SHUTDOWN_GRACE` 时，
   任务停在 running，靠锁超时回收——这意味着最长要等 `LOCK_TIMEOUT` 才会被重新领取。
2. **错误分类仍不够精确。** `retryable()` 只能靠 `errors.Is` 判断哨兵错误，
   判断不了的一律当可重试。目前有两个哨兵：`train.ErrManualLoginRequired`
   与 `train.ErrBrowserBusy`（本机端口 / profile 被另一个 Chrome 占用）。
   真正严谨的做法是让 `train` 包用自定义错误类型显式标注「永久失败」，
   这是它该有的演进方向。
3. **单机单 worker。** 多 worker 会撞上 Chrome profile 锁与同账号互踢（见文首四条约束）。
   要扩容得先解决「一个任务独占一个 profile + 一个 CDP 端口」的资源分配问题。
4. **进度页没有鉴权**，且默认只监听 `127.0.0.1`。要对外暴露必须先加认证，
   否则任何人都能看到所有学号与错误信息。
5. **数据库层不做级联删除**（有意不建外键）。删任务时要自己先清 `task_results` 与 `task_events`。
6. **`schema.sql` 里给应用账号设的默认口令是弱口令**，而且它就写在仓库里。
   本机开发无所谓（只监听 `127.0.0.1`、账号只有 DML 权限），
   但只要这个服务要放到别处跑，它就是一个公开的数据库口令。
   服务启动时若检测到仍在使用默认口令会打一条 `WARN` 提示；
   正式部署请先改口令并同步更新 `DB_PASSWORD`。

---

## 九、测试

纯逻辑测试（加密、配置校验、错误分类、模板转义、中间件）不需要任何外部依赖：

```bash
go test ./server/
```

队列语义的测试**连真库**（默认跳过，不配 DSN 就不跑）：

```bash
CQUPT_TEST_DSN='cqupt_app:cqupt_app_dev@tcp(127.0.0.1:3306)/cqupt_train?parseTime=true&loc=Local&charset=utf8mb4' \
  go test ./server/ -v
```

其中两条值得单独说：

- `TestClaimTaskExactlyOnceUnderConcurrency` —— 16 个 goroutine 抢 12 个任务，
  断言每个任务**恰好被领走一次**。去掉 `FOR UPDATE` 它会稳定失败。
- `TestClaimQueryPlan` —— 对 `store.go` 里那条 `claimSQL` **本身**做 `EXPLAIN`，
  断言走 `idx_claim` 且没有 filesort。索引设计本来是写在注释里的一段推理，
  这条测试把「设计意图」钉成了「可验证的事实」。

### 端到端验证记录（v1.3.0）

用假学号跑通了全流程（真实登录被沙箱环境挡住，Chrome 起不来，见下），逐项确认：

| 场景 | 结果 |
|------|------|
| 同时提交两个任务 | 严格串行：A 跑 `11:23:19→11:23:31`，B 才在 `11:23:31` 开始，无重叠；`try_count` 各为 1 |
| 失败重试 | 第 1 次失败 → 退避 **30.08s** → 第 2 次 → 终态，`try_count=2/2` |
| 取消排队中的任务 | `DELETE` 返回 200，状态 `canceled`，`try_count=0`（从未被领取） |
| 参数校验 | 空学号 / 题量为 0 / 未知模式均 `400`，文案来自 `train` 包的业务规则 |
| 优雅退出 | SIGTERM 后立即拒新连接；手上任务**跑完**并正常退回队列；日志「本次退出没有留下未收尾的任务」 |
| 跨重启续跑 | 重启后 **8ms 内**接手上次留下的 pending 任务 |
| `kill -9` 后回收 | 任务僵死在 running；重启后于 `locked_at + LOCK_TIMEOUT` 整点被 `RecoverStale` 放回并重新领取 |
| 进度页 | 渲染正常，事件时间线跨重试连续编号（seq 1、2 分别对应两次尝试） |

> 端到端时浏览器环节是失败的（`建立浏览器控制连接失败: context canceled`）。
> 这个失败**用 CLI 跑同样复现**，属于当前执行环境（沙箱里 Chrome 无法完成启动/连接）
> 而非服务化引入的问题——两个入口行为一致，正是我们要的结论。
> 不过它顺带暴露出一个真实的分类错误：上游报的 `context.Canceled` 被误判成
> 「我们自己取消了」，导致任务一次尝试就终态。已改为按「我方 ctx 是否被取消」来区分，
> 详见 `worker.go` 里 `retryable` 的注释。
