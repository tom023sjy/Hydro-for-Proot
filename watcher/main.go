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

type Watcher struct {
	pgid      int
	memLimit  int
	procLimit int
	timeLimit float64
	stop      chan bool
}

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
		s := string(data)
		idx := strings.LastIndex(s, ")")
		if idx < 0 || idx+2 >= len(s) {
			continue
		}
		fields := strings.Fields(s[idx+2:])
		if len(fields) < 3 {
			continue
		}
		if pgidVal, err := strconv.Atoi(fields[2]); err == nil && pgidVal == pgid {
			pids = append(pids, pid)
		}
	}
	return pids
}

func (w *Watcher) kill() {
	if w.pgid > 0 {
		_ = syscall.Kill(-w.pgid, syscall.SIGKILL)
	}
}

func (w *Watcher) monitor() string {
	start := time.Now()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-w.stop:
			return ""
		case <-ticker.C:
			if time.Since(start).Seconds() > w.timeLimit {
				return "TimeLimitExceeded"
			}
			pids := getPgidPids(w.pgid)
			if len(pids) > w.procLimit {
				return "RuntimeError"
			}
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

func main() {
	memLimit := flag.Int("mem", 256*1024, "内存限制 KB")
	procLimit := flag.Int("proc", 30, "进程数限制")
	rawTime := flag.Float64("time", 1.0, "时间限制 秒")
	prootArgs := flag.String("proot", "", "逗号分隔的 proot 参数（TMOE Debian 里留空）")
	execCmd := flag.String("exec", "", "要执行的命令")
	outFile := flag.String("out", "", "stdout 输出文件")
	errFile := flag.String("err", "", "stderr 输出文件")
	flag.Parse()

	if *execCmd == "" {
		fmt.Fprintln(os.Stderr, "错误：必须通过 -exec 指定要执行的命令")
		os.Exit(1)
	}

	timeLimit := *rawTime * 1.1

	// ===== 这里是改动点 =====
	// 如果 -proot 为空，直接执行 -exec 命令（TMOE Debian 场景）
	// 如果 -proot 非空，走 proot 包装（纯 Termux 场景）
	var cmd *exec.Cmd
	if *prootArgs == "" {
		// 直接执行。用 sh -c 是为了支持 "cmd arg1 arg2" 这种带空格的命令
		cmd = exec.Command("sh", "-c", *execCmd)
	} else {
		args := strings.Split(*prootArgs, ",")
		args = append(args, *execCmd)
		cmd = exec.Command("proot", args...)
	}
	// ========================

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = nil

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

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		fmt.Println("RESULT:InternalError")
		os.Exit(1)
	}

	w := &Watcher{
		pgid:      cmd.Process.Pid,
		memLimit:  *memLimit,
		procLimit: *procLimit,
		timeLimit: timeLimit,
		stop:      make(chan bool),
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	statusChan := make(chan string, 1)
	go func() {
		s := w.monitor()
		if s != "" {
			statusChan <- s
		}
	}()

	select {
	case err := <-done:
		close(w.stop)
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
		w.kill()
		<-done
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
