package main

// es_ingest_progress.go 是 es:* 命令族灌数进度条的统一接入口。
//
// 为什么要有 stdoutIsTerminal：github.com/schollz/progressbar/v3 v3.19.1 **不做 TTY 检测、
// 不会自动降级** —— 一旦构造并 Start，就会周期性往 writer 里刷 '\r' 与控制字符，无论输出
// 是管道、文件还是 CI 日志。重定向场景下这些字符会污染结论（es:migrate --json 的 stdout
// 必须是可解析的纯 JSON）。因此非 TTY 直接不构造渲染器，而不是构造了再想办法静音。
//
// 不引入新依赖：x/term 与 go-isatty 在 go.mod 里只是 indirect，为它们改依赖清单不值得；
// 判「是不是字符设备」用标准库 os.Stat 足够（与 es_cutover.go 的 cutoverStdinIsInteractive
// 同一口径，同样把 /dev/null 这种「字符设备但没人看」的情况判为非 TTY）。

import (
	"os"

	"shop/internal/es/sync"
)

// stdoutIsTerminal 判标准输出是否是给人看的终端。
func stdoutIsTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false // 管道 / 重定向到文件
	}
	if devNull, oerr := os.Open(os.DevNull); oerr == nil {
		defer devNull.Close()
		if nfi, serr := devNull.Stat(); serr == nil && os.SameFile(fi, nfi) {
			return false // 是字符设备，但输出被丢弃，不是人
		}
	}
	return true
}

// startIngestProgress 启动一条灌数进度渲染器，返回关闭函数（调用方 defer 即可，未启动时是空函数）。
// quiet=true（--json）或非 TTY 时不构造渲染器，保证 stdout 只有结论本身。
func startIngestProgress(stats *sync.SyncStats, desc string, quiet bool) func() {
	if !ingestProgressEnabled(quiet, stdoutIsTerminal()) {
		return func() {}
	}
	prog := sync.NewProgress(stats, desc, 0)
	prog.Start()
	return prog.Shutdown
}

// ingestProgressEnabled 是「要不要给灌数接进度条」的唯一决策点（抽成纯函数供单测钉住真值表）：
// 只有「无人消费输出的场景」才降级，即 --json（stdout 是一份必须可解析的结论）与非 TTY。
func ingestProgressEnabled(quiet, tty bool) bool {
	return tty && !quiet
}
