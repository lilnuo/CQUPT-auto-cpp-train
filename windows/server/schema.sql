-- ============================================================================
--  CQUPT 刷题服务 · 数据库结构
--
--  执行方式（需要有建库建表权限的账号，例如 root）：
--      mysql -u root -p < server/schema.sql
--
--  三张表的职责划分：
--      tasks        任务本体 + 队列。这张表同时当队列用，不额外引入中间件。
--      task_results 每道题的作答结果（题干、答案、得分），用于复盘与统计。
--      task_events  进度事件流，用于"任务跑到哪一步了"的实时展示。
--
--  为什么 tasks 既是业务表又是队列：
--      任务量是"每个学生每天几次"的量级，不是每秒几万条。这个量级下，
--      MySQL 的行锁 + SKIP LOCKED 完全够用，而且**天然没有"消息入队成功但
--      业务事务回滚"的双写不一致问题**——任务和队列状态在同一行、同一个事务里。
--      真正的消息队列解决的是"每秒十万级 + 多消费者组 + 分区顺序"，
--      那是另一个量级的问题。上中间件之前先问自己：我的瓶颈真在那里吗？
-- ============================================================================

CREATE DATABASE IF NOT EXISTS cqupt_train
  DEFAULT CHARACTER SET utf8mb4
  DEFAULT COLLATE utf8mb4_0900_ai_ci;

USE cqupt_train;

-- ---------------------------------------------------------------------------
-- 任务表（兼队列）
-- ---------------------------------------------------------------------------
DROP TABLE IF EXISTS task_events;
DROP TABLE IF EXISTS task_results;
DROP TABLE IF EXISTS tasks;

CREATE TABLE tasks (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,

  -- ---- 任务参数 ----
  username      VARCHAR(64)     NOT NULL                COMMENT '学号',
  mode          VARCHAR(16)     NOT NULL DEFAULT 'quiz' COMMENT 'quiz / progap / progapdump',
  num           INT UNSIGNED    NOT NULL                COMMENT '计划刷多少题',

  -- ---- 密码：加密存储，绝不存明文 ----
  -- AES-256-GCM 密文（含随机 nonce 前缀），密钥来自环境变量 TASK_SECRET。
  -- 为什么不存明文：这是**真实学生的学号密码**，一旦库被 dump 出去就是事故。
  -- 为什么不干脆不存：worker 需要它才能登录。若只在内存里传给 worker，
  -- 服务一重启，队列里没开始的任务就全废了——队列的存在意义就是跨重启把活干完。
  password_enc  VARBINARY(512)  NOT NULL                COMMENT 'AES-256-GCM 加密后的密码',

  -- ---- 状态机 ----
  -- pending   排队中，可被领取
  -- running   已被某个 worker 领走
  -- succeeded 跑完且成功
  -- failed    跑完但失败（看 error 字段）
  -- canceled  人工取消
  status        VARCHAR(16)     NOT NULL DEFAULT 'pending',
  error         TEXT            NULL                    COMMENT '失败原因',

  -- ---- 队列字段 ----
  -- available_at 让"重试退避"不需要额外的定时器：失败的任务把时间往后挪一点，
  -- 抢任务的条件里带上 available_at <= NOW()，它自然就不会被立刻再抢到。
  available_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '到点才可被领取',
  locked_by     VARCHAR(64)     NULL                    COMMENT '领取它的 worker 标识（见 idx_locked_by 的说明）',
  locked_at     DATETIME(3)     NULL                    COMMENT '领取时刻，用于识别僵死任务',
  try_count     INT UNSIGNED    NOT NULL DEFAULT 0      COMMENT '已被领取次数',
  max_try       INT UNSIGNED    NOT NULL DEFAULT 2      COMMENT '最多领取几次',

  -- ---- 结果 ----
  score         INT             NULL                    COMMENT '最终总分',
  question_done INT UNSIGNED    NOT NULL DEFAULT 0      COMMENT '已完成题数（进度用）',

  -- ---- 时间 ----
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  started_at    DATETIME(3)     NULL,
  finished_at   DATETIME(3)     NULL,

  PRIMARY KEY (id),

  -- 索引 1：抢任务（最热的查询）
  --
  -- 查询固定长这样：
  --     WHERE status = 'pending' AND available_at <= NOW(3)
  --     ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED
  --
  -- 为什么是 (status, id)，而不是"看起来更贴题"的 (status, available_at, id)：
  --   available_at 用的是**范围**条件（<=）。范围一旦生效，索引内部就不再按
  --   "后面的列"有序了，所以 (status, available_at, id) 其实满足不了 ORDER BY id，
  --   MySQL 还得额外做一次 filesort。
  --   （这个错误是 v1.3.0 开发时被 EXPLAIN 抓出来的：5000 行、0 条 pending 时，
  --    优化器选的是 idx_status_created + Sort 步骤，idx_claim 压根没被用上。）
  --
  -- (status, id) 则天然有序：status 等值先定位到一段连续区间，区间内本身就是
  -- id 升序，于是 ORDER BY id 完全由索引满足，没有排序步骤。available_at 退化成
  -- 区间内的一个过滤条件——处于重试退避中的任务本来就是极少数，跳过它继续找即可。
  --
  -- 实测（MySQL 9.4，5000 行）：
  --     5000 条 pending   → Index range scan on idx_claim，无 Sort，读 1 行
  --     仅 1 条 pending   → Index lookup on idx_claim，无 Sort
  --     0 条 pending      → 优化器改去扫 PRIMARY 全表：读 5000 行、4.6ms ★
  --
  -- ★ 这是本设计里唯一不理想的情况：服务空闲、表里却积着一批历史任务时，
  --   优化器的代价模型认为"顺着主键扫"几乎免费（估出 0.0046 的代价），
  --   于是一轮轮询白读一遍全表。表越大越慢，而空闲时轮询最频繁。
  --   所以抢任务的 SQL 里显式写了 FORCE INDEX (idx_claim)：
  --   实测 4.6ms → 0.047ms（约 100 倍），且不再随表增长。
  --   代价是——以后若重命名或删掉 idx_claim，这条 SQL 会立刻报错。
  --   这正是想要的：响声越大越好，而不是悄悄退化成全表扫描。
  KEY idx_claim (status, id),

  -- 索引 2：回收僵死任务
  -- 只需要扫 "还在 running 且很久没动" 的行，单列（locked_at）即可。
  -- 不加 status 前缀是有意的：running 的行本来就极少数，单列索引更小。
  KEY idx_locked_at (locked_at),

  -- 索引 3：按学号查历史
  -- 典型查询是"某学号最近的任务"，所以第二列放 created_at 让排序也走索引。
  KEY idx_username_created (username, created_at),

  -- 索引 4：进度页按状态统计
  KEY idx_status_created (status, created_at)
) ENGINE=InnoDB
  DEFAULT CHARSET=utf8mb4
  COLLATE=utf8mb4_0900_ai_ci
  COMMENT='刷题任务（兼队列）';

-- ---------------------------------------------------------------------------
-- 每题结果表
-- ---------------------------------------------------------------------------
CREATE TABLE task_results (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  task_id     BIGINT UNSIGNED NOT NULL,
  pid         VARCHAR(64)     NOT NULL DEFAULT ''  COMMENT '题目标识，如 answerForm3 / 程序题标题',
  question    TEXT            NULL                 COMMENT '题干（可能很长，用于复盘）',
  answer      TEXT            NULL                 COMMENT '模型给出的答案',
  score       DECIMAL(6,2)    NOT NULL DEFAULT 0   COMMENT '该题所得分数',
  tries       INT UNSIGNED    NOT NULL DEFAULT 1   COMMENT '实际作答次数',
  passed      TINYINT(1)      NOT NULL DEFAULT 0   COMMENT '是否拿到分',
  created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),

  PRIMARY KEY (id),

  -- 一个任务里同一个 pid 只该有一条记录（重答会更新它，而不是插新行）。
  -- 这个唯一键同时也是"重答时 UPDATE 而不是 INSERT"的保证——
  -- 靠应用层判断容易漏，交给数据库更稳。
  UNIQUE KEY uk_task_pid (task_id, pid),

  -- 复盘页按任务拉全部结果
  KEY idx_task_id (task_id)
) ENGINE=InnoDB
  DEFAULT CHARSET=utf8mb4
  COLLATE=utf8mb4_0900_ai_ci
  COMMENT='每道题的作答结果';

-- ---------------------------------------------------------------------------
-- 进度事件表
-- ---------------------------------------------------------------------------
CREATE TABLE task_events (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  task_id     BIGINT UNSIGNED NOT NULL,

  -- seq 是"同一任务内递增的序号"。为什么不用 created_at 排序：
  -- 同一毫秒内产生的事件时间戳可能相同，排序不稳定；用序号才能稳定复现顺序。
  -- 而且 (task_id, seq) 上的唯一键能让"重复上报同一个事件"变成幂等操作。
  seq         INT UNSIGNED    NOT NULL,
  kind        VARCHAR(24)     NOT NULL COMMENT 'started/login_ok/assignment_in/question_done/finished',
  detail      VARCHAR(512)    NOT NULL DEFAULT '',
  pid         VARCHAR(64)     NOT NULL DEFAULT '',
  score       DECIMAL(6,2)    NOT NULL DEFAULT 0,
  created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),

  PRIMARY KEY (id),
  UNIQUE KEY uk_task_seq (task_id, seq),
  KEY idx_task_id (task_id)
) ENGINE=InnoDB
  DEFAULT CHARSET=utf8mb4
  COLLATE=utf8mb4_0900_ai_ci
  COMMENT='任务进度事件';

-- ---------------------------------------------------------------------------
-- 应用账号：只给 DML 权限，不给 DDL
--
-- 为什么不直接用 root：应用账号只需要增删改查。收回建表/删表权限之后，
-- 就算应用里存在 SQL 注入，攻击者也改不了表结构、删不掉库。
-- 代价是以后改 schema 必须用 root 手动执行，这是一笔划算的交换。
-- ---------------------------------------------------------------------------
CREATE USER IF NOT EXISTS 'cqupt_app'@'localhost' IDENTIFIED BY 'cqupt_app_dev';
GRANT SELECT, INSERT, UPDATE, DELETE ON cqupt_train.* TO 'cqupt_app'@'localhost';
FLUSH PRIVILEGES;

SELECT '✅ cqupt_train 建库完成，应用账号 cqupt_app 已授权（仅 DML）' AS result;
