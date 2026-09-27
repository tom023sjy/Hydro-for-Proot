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
	pgid      int    // 进程组 ID
	cmd       *exec.Cmd
	memLimit  int    // 内存限制 (KB)
	procLimit int    // 进程数限制
	timeLimit float64 // 时间限制 (秒)，已经是 *1.1 之后的
}

// ---------- /proc 工具函数 ----------

// getVmRSS 读取指定 PID 的常驻内存大小 (KB)
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
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		// stat 格式: pid (comm) state ppid pgrp ...
		// comm 可能包含空格，用右括号定位
		s := string(data)
		idx := strings.LastIndex(s, ")")
		if idx < 0 || idx+2 >= len(s) {
			continue
		}
		fields := strings.Fields(s[idx+2:])
		if len(fields) < 3 {
			continue
		}
		// fields[0]=state, fields[1]=ppid, fields[2]=pgrp
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
		_ = syscall.Kill(-w.pgid, syscall.SIGKILL)
	}
}

// ---------- 监控主循环 ----------

// monitor 返回状态字符串: "TimeLimitExceeded" / "MemoryLimitExceeded" / "RuntimeError"
// 或空字符串表示监控自身出错
func (w *Watcher) monitor() (string, error) {
	start := time.Now()
	ticker := time.NewTicker(10 * time.Millisecond) // 10ms 采样
	defer ticker.Stop()

	for range ticker.C {
		// 1. 时间限制
		if time.Since(start).Seconds() > w.timeLimit {
			return "TimeLimitExceeded", nil
		}

		// 2. 获取进程组内所有 PID
		pids := getPgidPids(w.pgid)

		// 3. 进程数限制 (fork 炸弹)
		if len(pids) > w.procLimit {
			return "RuntimeError", fmt.Errorf("process limit exceeded: %d", len(pids))
		}

		// 4. 内存限制
		totalMem := 0
		for _, pid := range pids {
			totalMem += getVmRSS(pid)
		}
		if totalMem > w.memLimit {
			return "MemoryLimitExceeded", fmt.Errorf("memory limit exceeded: %d KB", totalMem)
		}
	}
	return "", nil
}

// ---------- 入口 ----------

func main() {
	memLimit := flag.Int("mem", 256*1024, "memory limit in KB")
	procLimit := flag.Int("proc", 30, "process limit")
	rawTime := flag.Float64("time", 1.0, "time limit in seconds (before 1.1x)")
	prootArgs := flag.String("proot", "", "comma-separated proot args")
	execCmd := flag.String("exec", "", "command to execute inside proot")
	flag.Parse()

	if *execCmd == "" {
		fmt.Fprintln(os.Stderr, "Error: -exec is required")
		os.Exit(1)
	}

	// 时限 *1.1，吸收 proot + 采样误差
	timeLimit := *rawTime * 1.1

	// 拼装 proot 命令
	args := []string{}
	if *prootArgs != "" {
		args = append(args, strings.Split(*prootArgs, ",")...)
	}
	args = append(args, *execCmd)

	cmd := exec.Command("proot", args...)
	// 让 proot 成为新进程组组长，便于整组击杀
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start proot: %v\n", err)
		fmt.Println("RESULT:InternalError")
		os.Exit(1)
	}

	w := &Watcher{
		pgid:      cmd.Process.Pid,
		cmd:       cmd,
		memLimit:  *memLimit,
		procLimit: *procLimit,
		timeLimit: timeLimit,
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	statusChan := make(chan string, 1)
	errChan := make(chan error, 1)
	go func() {
		s, err := w.monitor()
		if err != nil {
			errChan <- err
			return
		}
		statusChan <- s
	}()

	select {
	case err := <-done:
		// 程序自己结束
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
		}
	case status := <-statusChan:
		// 监控发现违规
		w.kill()
		<-done // 等待清理完成
		fmt.Printf("RESULT:%s\n", status)
	case err := <-errChan:
		w.kill()
		<-done
		fmt.Printf("RESULT:InternalError\n")
		fmt.Printf("ERROR:%v\n", err)
	}
}
