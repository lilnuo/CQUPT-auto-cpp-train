package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是服务端运行期配置。
//
// 它和 config 包（CLI 用的那个）是两回事：config 包管的是"怎么刷题"
// （选择器、超时、Prompt），这里管的是"怎么把刷题跑成一个服务"
// （监听地址、数据库、worker 数量、任务超时）。
// 之所以不塞进同一个结构体：这两种配置的变更频率和负责人都不同——
// 站点改版要动前者，扩容要动后者。
type Config struct {
	Addr string // HTTP 监听地址，如 127.0.0.1:8080

	// 数据库
	DBUser     string
	DBPassword string
	DBHost     string
	DBPort     string
	DBName     string

	// TaskSecret 是任务密码的加密密钥（AES-256-GCM）。
	//
	// **必填，且长度至少 32 字符**。没有它就拒绝启动，绝不做"那就先存明文吧"
	// 这种降级——降级的后果是真实学生的学号密码明文躺在库里。
	TaskSecret string

	// WorkerID 标识"哪个进程领走了这个任务"，写进 tasks.locked_by。
	WorkerID string

	// WorkerPollInterval 是队列空时的轮询间隔。
	//
	// 为什么不做得更实时：这个队列的量级是"每个学生每天几次"，
	// 空转间隔 2 秒完全够用；比起引入一套通知机制，多几次轻量 SELECT 更划算。
	WorkerPollInterval time.Duration

	// LockTimeout 超过这么久没更新过 locked_at 的 running 任务，视为僵死并回收。
	// 它必须**大于**单次任务的最长耗时，否则正常跑着的任务会被抢走重跑。
	LockTimeout time.Duration

	// TaskMaxRuntime 是单个任务允许的最长执行时间，超时即取消。
	//
	// 这是**防呆**而非限速：正常刷 10 道题也就几分钟。设这一层是因为
	// 一旦浏览器卡死、或者瑞数挑战永远过不去，任务会无限期占着那把串行锁，
	// 把队列后面所有任务一起拖死——超时是打破这种僵局的最后一道闸。
	TaskMaxRuntime time.Duration
}

// defaultDevDBPassword 是 schema.sql 里给应用账号设的开发口令。
//
// 写在这里是为了让 main 能识别出"还在用默认口令"，从而打一条告警——
// 它明晃晃地躺在仓库里，本地开发无所谓，部署到别处就是一个公开的数据库口令。
const defaultDevDBPassword = "cqupt_app_dev"

// LoadConfig 从环境变量读配置，缺必填项就直接报错。
//
// 服务端和 CLI 在这一点上态度不同：CLI 缺配置可以交互式问用户，
// 服务端没有任何人可问，所以只能**启动时就报错退出**。
// 带着半截配置跑起来的服务，比直接起不来更危险——它会在半夜某个请求上才暴露问题。
func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		Addr:               envOr(getenv, "SERVER_ADDR", "127.0.0.1:8080"),
		DBUser:             envOr(getenv, "DB_USER", "cqupt_app"),
		DBPassword:         envOr(getenv, "DB_PASSWORD", defaultDevDBPassword),
		DBHost:             envOr(getenv, "DB_HOST", "127.0.0.1"),
		DBPort:             envOr(getenv, "DB_PORT", "3306"),
		DBName:             envOr(getenv, "DB_NAME", "cqupt_train"),
		TaskSecret:         getenv("TASK_SECRET"),
		WorkerID:           getenv("WORKER_ID"),
		WorkerPollInterval: envDuration(getenv, "WORKER_POLL_INTERVAL", 2*time.Second),
		LockTimeout:        envDuration(getenv, "LOCK_TIMEOUT", 20*time.Minute),
		TaskMaxRuntime:     envDuration(getenv, "TASK_MAX_RUNTIME", 15*time.Minute),
	}

	if c.TaskSecret == "" {
		return c, errors.New("缺少环境变量 TASK_SECRET：它是任务密码的加密密钥，" +
			"必须设置（至少 32 字符），否则会把学生密码明文写进数据库")
	}
	if len(c.TaskSecret) < 32 {
		return c, fmt.Errorf("TASK_SECRET 太短（%d 字符），至少需要 32 字符", len(c.TaskSecret))
	}
	if c.WorkerID == "" {
		// 用主机名 + 进程号区分同一台机器上的多个进程；
		// 单机开发时这样足够，多机部署应当显式指定（例如把 Pod 名传进来）。
		host, _ := os.Hostname()
		c.WorkerID = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	if c.LockTimeout <= c.TaskMaxRuntime {
		return c, fmt.Errorf("LOCK_TIMEOUT(%s) 必须大于 TASK_MAX_RUNTIME(%s)，"+
			"否则还在正常执行的任务会被当成僵死任务抢走重跑",
			c.LockTimeout, c.TaskMaxRuntime)
	}
	return c, nil
}

// DSN 拼出 go-sql-driver/mysql 需要的连接串。
//
// parseTime=true 是为了让 DATETIME 列能直接扫进 time.Time；
// 不加的话拿到的是 []byte，每次都要手工解析。
func (c Config) DSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&loc=Local&charset=utf8mb4",
		c.DBUser, c.DBPassword, c.DBHost, c.DBPort, c.DBName)
}

// Redacted 返回可以安全打印的配置摘要。
//
// 密钥和数据库口令绝不能进日志——日志往往会被收集、转发、长期保存，
// 等于把凭据又泄漏了一遍。
func (c Config) Redacted() string {
	return fmt.Sprintf("addr=%s db=%s@%s:%s/%s worker=%s 轮询=%s 锁超时=%s 任务超时=%s 密钥=<已隐藏>",
		c.Addr, c.DBUser, c.DBHost, c.DBPort, c.DBName,
		c.WorkerID, c.WorkerPollInterval, c.LockTimeout, c.TaskMaxRuntime)
}

func envOr(getenv func(string) string, key, def string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return def
}

func envDuration(getenv func(string) string, key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def
	}
	// 先按 Go 的时长写法试（"30s" / "5m"），失败再按"纯秒数"解释，
	// 后者是为了迁就习惯写数字的运维配置。
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}
