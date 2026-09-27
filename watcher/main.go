package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Watcher 保存一次评测的监控配置与运行状态
type Watcher struct {
	pgid      int       // 进程组 ID
	memLimit  int       // 内存限制，单位 KB
	procLimit int       // 进程数限制
	timeLimit float64   // 时间限制，单位秒（已经 *1.1 之后）
	stop      chan bool // 通知 monitor 线程退出
}

// ---------- /proc 工具函数 ----------

// getVmRSS 读取指定 PID 的常驻内存大小，单位 KB。失败返回 0。
func getVmRSS(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				v, _ := strconv.Atoi(fields[1])
				return v
			}
		}
	}
	return 0
}

// getPgidPids 返回属于指定进程组的所有 PID
func getPgidPids(pgid int) []int {
	var pids []int
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return pids
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue // 不是数字目录，跳过
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue // 进程可能刚退出
		}
		// /proc/<pid>/stat 格式：
		//   pid (comm) state ppid pgrp ...
		// comm 可能包含空格和括号，所以要从最后一个 ')' 之后开始解析
		s := string(data)
		idx := strings.LastIndex(s, ")")
		if idx < 0 || idx+2 >= len(s) {
			continue
		}
		fields := strings.Fields(s[idx+2:])
		if len(fields) < 3 {
			continue
		}
		// 从 ')' 之后开始：fields[0]=state, fields[1]=ppid, fields[2]=pgrp
		if pgidVal, err := strconv.Atoi(fields[2]); err == nil && pgidVal == pgid {
			pids = append(pids, pid)
		}
	}
	return pids
}

// ---------- 进程组击杀 ----------

// kill 向整个进程组发送 SIGKILL，兜底清理
func (w *Watcher) kill() {
	if w.pgid > 0 {
		// 注意：负数 PID 表示"进程组"
		_ = syscall.Kill(-w.pgid, syscall.SIGKILL)
	}
}

// ---------- 监控主循环 ----------

// monitor 每 10ms 采样一次，检查时间 / 内存 / 进程数。
// 返回非空字符串表示违规状态；返回空字符串表示被 stop 通知正常退出。
func (w *Watcher) monitor() string {
	start := time.Now()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-w.stop:
			return "" // 主流程正常结束，通知 monitor 退出

		case <-ticker.C:
			// 1. 时间限制
			if time.Since(start).Seconds() > w.timeLimit {
				return "TimeLimitExceeded"
			}

			// 2. 进程组内所有 PID
			pids := getPgidPids(w.pgid)

			// 3. 进程数限制（防 fork 炸弹）
			if len(pids) > w.procLimit {
				return "RuntimeError"
			}

			// 4. 内存限制（累加进程组内所有进程的 VmRSS）
			totalMem := 0
			for _, pid := range pids {
				totalMem += getVmRSS(pid)
			}
			if totalMem > w.memLimit {
				return "MemoryLimitExceeded"
			}
		}
	}
}

// ---------- 入口 ----------

func main() {
	// ---------- 解析命令行参数 ----------
	memLimit := flag.Int("mem", 256*1024, "内存限制，单位 KB（默认 256MB）")
	procLimit := flag.Int("proc", 30, "进程数限制（默认 30）")
	rawTime := flag.Float64("time", 1.0, "时间限制，单位秒（未乘 1.1）")
	prootArgs := flag.String("proot", "", "逗号分隔的 proot 参数，例如 -r,/rootfs,-b,/dev")
	execCmd := flag.String("exec", "", "要在 proot 里执行的命令（必填）")
	outFile := flag.String("out", "", "选手程序 stdout 输出到的文件（可选，不填则丢弃）")
	errFile := flag.String("err", "", "选手程序 stderr 输出到的文件（可选，不填则丢弃）")
	flag.Parse()

	if *execCmd == "" {
		fmt.Fprintln(os.Stderr, "错误：必须通过 -exec 指定要执行的命令")
		os.Exit(1)
	}

	// 时限 *1.1，吸收 proot + 采样误差
	timeLimit := *rawTime * 1.1

	// ---------- 拼装 proot 命令 ----------
	args := []string{}
	if *prootArgs != "" {
		args = append(args, strings.Split(*prootArgs, ",")...)
	}
	// 最后一项是要执行的命令字符串（proot 会把它当成 shell 命令）
	args = append(args, *execCmd)

	cmd := exec.Command("proot", args...)
	// 关键：让 proot 成为新进程组的组长，方便整组击杀
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// stdin 给 /dev/null，选手程序通常不需要读输入（或者说输入应通过文件提供）
	cmd.Stdin = nil

	// ---------- 输出重定向 ----------
	// 把选手程序的 stdout/stderr 写到文件，避免污染 watcher 自己的 stdout
	var outFd, errFd *os.File
	if *outFile != "" {
		f, err := os.Create(*outFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "无法创建输出文件: %v\n", err)
			os.Exit(1)
		}
		outFd = f
		cmd.Stdout = f
	}
	if *errFile != "" {
		f, err := os.Create(*errFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "无法创建错误文件: %v\n", err)
			os.Exit(1)
		}
		errFd = f
		cmd.Stderr = f
	}

	// ---------- 启动进程 ----------
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "启动 proot 失败: %v\n", err)
		fmt.Println("RESULT:InternalError")
		os.Exit(1)
	}

	w := &Watcher{
		pgid:      cmd.Process.Pid, // 因为设了 Setpgid，子进程 PID 就是 PGID
		memLimit:  *memLimit,
		procLimit: *procLimit,
		timeLimit: timeLimit,
		stop:      make(chan bool),
	}

	// ---------- 等待结束 / 监控 ----------
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	statusChan := make(chan string, 1)
	go func() {
		s := w.monitor()
		if s != "" {
			statusChan <- s
		}
	}()

	// 只走其中一个分支
	select {
	case err := <-done:
		// 程序自己结束（正常或异常退出）
		close(w.stop) // 通知 monitor 退出
		if outFd != nil {
			_ = outFd.Close()
		}
		if errFd != nil {
			_ = errFd.Close()
		}
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				fmt.Printf("RESULT:RuntimeError\n")
				fmt.Printf("EXIT_CODE:%d\n", exitErr.ExitCode())
			} else {
				fmt.Printf("RESULT:RuntimeError\n")
				fmt.Printf("ERROR:%v\n", err)
			}
		} else {
			fmt.Printf("RESULT:Accepted\n")
			fmt.Printf("EXIT_CODE:0\n")
		}

	case status := <-statusChan:
		// 监控发现违规，杀整组
		w.kill()
		<-done // 等待清理完成
		close(w.stop)
		if outFd != nil {
			_ = outFd.Close()
		}
		if errFd != nil {
			_ = errFd.Close()
		}
		fmt.Printf("RESULT:%s\n", status)
	}
}
