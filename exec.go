package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

type ExecCfg struct {
	Node Node              `json:"node"`
	Cmd  Cmd               `json:"cmd"`
	Env  map[string]string `json:"env"`
}

func mountSyscall(source, target, fstype string, flags uintptr, data string) {
	if err := unix.Mount(source, target, fstype, flags, data); err != nil {
		fmtException("mount %s -> %s (fs=%s flags=%x): %w", source, target, fstype, flags, err).throw()
	}
}

func bindRO(src, dst string) {
	mountSyscall(src, dst, "", unix.MS_BIND, "")
	mountSyscall("", dst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
}

func mkdirAll(p string) {
	if err := os.MkdirAll(p, 0755); err != nil {
		fmtException("mkdir %s: %w", p, err).throw()
	}
}

func mkdir(p string) {
	if err := os.Mkdir(p, 0755); err != nil {
		fmtException("mkdir %s: %w", p, err).throw()
	}
}

func writeFile(p string, data []byte) {
	if err := os.WriteFile(p, data, 0); err != nil {
		fmtException("write %s: %w", p, err).throw()
	}
}

func setupTmpfs(buildRoot, ixRoot string) {
	mkdirAll(buildRoot)
	mountSyscall("tmpfs", buildRoot, "tmpfs", 0, "")

	trashInside := filepath.Join(buildRoot, ".trash")
	mkdir(trashInside)

	trash := filepath.Join(ixRoot, "trash")
	mkdirAll(trash)
	mountSyscall(trashInside, trash, "", unix.MS_BIND, "")
}

func setupShadow(inDirs []string, outDir, buildRoot, ixRoot string) {
	realStore := filepath.Join(ixRoot, "store")
	shadowStore := filepath.Join(buildRoot, ".shadow", "store")
	mkdirAll(shadowStore)

	for _, d := range inDirs {
		target := filepath.Join(shadowStore, filepath.Base(d))
		mkdirAll(target)
		bindRO(d, target)
	}

	if outDir != "" {
		mkdirAll(outDir)
		target := filepath.Join(shadowStore, filepath.Base(outDir))
		mkdirAll(target)
		mountSyscall(outDir, target, "", unix.MS_BIND, "")
	}

	mountSyscall(shadowStore, realStore, "", unix.MS_BIND|unix.MS_REC, "")
}

func setupSandbox(n *Node) {
	if !n.Isolate && !n.Tmpfs {
		return
	}

	runtime.LockOSThread()

	uid := os.Getuid()
	gid := os.Getgid()

	flags := unix.CLONE_NEWUSER | unix.CLONE_NEWNS

	if n.Pool != "network" {
		flags |= unix.CLONE_NEWNET
	}

	if err := unix.Unshare(flags); err != nil {
		fmtException("unshare: %w", err).throw()
	}

	writeFile("/proc/self/setgroups", []byte("deny\n"))
	writeFile("/proc/self/uid_map", []byte(fmt.Sprintf("0 %d 1\n", uid)))
	writeFile("/proc/self/gid_map", []byte(fmt.Sprintf("0 %d 1\n", gid)))

	mountSyscall("", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")

	if n.Tmp == "" {
		return
	}

	buildRoot := filepath.Dir(n.Tmp)
	ixRoot := filepath.Dir(buildRoot)

	if n.Tmpfs {
		setupTmpfs(buildRoot, ixRoot)
	}

	if n.Isolate {
		outDir := ""

		if len(n.OutDirs) > 0 {
			outDir = n.OutDirs[0]
		}

		setupShadow(n.InDirs, outDir, buildRoot, ixRoot)
	}
}

func lookupPathExec(prog, path string) string {
	if strings.Contains(prog, "/") {
		return prog
	}

	for _, p := range strings.Split(path, ":") {
		full := filepath.Join(p, prog)

		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			return full
		}
	}

	fmtException("cannot find %q in PATH=%q", prog, path).throw()
	panic(nil)
}

func envToList(env map[string]string) []string {
	res := make([]string, 0, len(env))

	for k, v := range env {
		res = append(res, k+"="+v)
	}

	return res
}

func cliExec() {
	var cfg ExecCfg

	if err := json.NewDecoder(os.Stdin).Decode(&cfg); err != nil {
		fmtException("decode cfg: %w", err).throw()
	}

	if len(cfg.Cmd.Args) == 0 {
		fmtException("empty args").throw()
	}

	setupSandbox(&cfg.Node)

	prog := lookupPathExec(cfg.Cmd.Args[0], cfg.Env["PATH"])

	cmd := &exec.Cmd{
		Path:   prog,
		Args:   cfg.Cmd.Args,
		Env:    envToList(cfg.Env),
		Stdin:  strings.NewReader(cfg.Cmd.Stdin),
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}

	err := cmd.Run()

	if err == nil {
		return
	}

	if ee, ok := err.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	}

	fmtException("run %s: %w", prog, err).throw()
}

func wrapCmdJSON(c *Cmd, n *Node, env map[string]string) ([]byte, error) {
	cfg := ExecCfg{
		Node: *n,
		Cmd:  *c,
		Env:  env,
	}

	return json.Marshal(cfg)
}

func selfPath() string {
	p, err := os.Executable()

	if err != nil {
		fmtException("os.Executable: %w", err).throw()
	}

	return p
}

func runWrapped(c *Cmd, n *Node, env map[string]string, out io.Writer) error {
	payload, err := wrapCmdJSON(c, n, env)

	if err != nil {
		return err
	}

	cmd := &exec.Cmd{
		Path:   selfPath(),
		Args:   []string{selfPath(), "exec"},
		Stdin:  bytes.NewReader(payload),
		Stdout: out,
		Stderr: out,
	}

	return cmd.Run()
}
