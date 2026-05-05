package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

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

func setupMounts(n *Node) {
	if !n.Isolate && !n.Tmpfs {
		return
	}

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

func selfPath() string {
	p, err := os.Executable()

	if err != nil {
		fmtException("os.Executable: %w", err).throw()
	}

	return p
}

func dupFromMemfd(name string, payload []byte) {
	fd, err := unix.MemfdCreate(name, 0)

	if err != nil {
		fmtException("memfd_create: %w", err).throw()
	}

	for len(payload) > 0 {
		n, err := unix.Write(fd, payload)

		if err != nil {
			fmtException("write memfd: %w", err).throw()
		}

		payload = payload[n:]
	}

	if _, err := unix.Seek(fd, 0, 0); err != nil {
		fmtException("seek memfd: %w", err).throw()
	}

	if err := unix.Dup2(fd, 0); err != nil {
		fmtException("dup2 memfd: %w", err).throw()
	}

	unix.Close(fd)
}

func cliExec() {
	var cfg ExecCfg

	if err := json.NewDecoder(os.Stdin).Decode(&cfg); err != nil {
		fmtException("decode cfg: %w", err).throw()
	}

	if len(cfg.Cmd.Args) == 0 {
		fmtException("empty args").throw()
	}

	setupMounts(&cfg.Node)

	prog := lookupPathExec(cfg.Cmd.Args[0], cfg.Env["PATH"])

	dupFromMemfd("cmd-stdin", []byte(cfg.Cmd.Stdin))

	if err := syscall.Exec(prog, cfg.Cmd.Args, envToList(cfg.Env)); err != nil {
		fmtException("exec %s: %w", prog, err).throw()
	}
}

func runWrapped(c *Cmd, n *Node, env map[string]string, out io.Writer) error {
	if !n.Isolate && !n.Tmpfs {
		prog := lookupPathExec(c.Args[0], env["PATH"])

		cmd := &exec.Cmd{
			Path:   prog,
			Args:   c.Args,
			Env:    envToList(env),
			Stdin:  strings.NewReader(c.Stdin),
			Stdout: out,
			Stderr: out,
		}

		return cmd.Run()
	}

	payload, err := json.Marshal(ExecCfg{Node: *n, Cmd: *c, Env: env})

	if err != nil {
		fmtException("marshal cfg: %w", err).throw()
	}

	unsharePath, err := exec.LookPath("unshare")

	if err != nil {
		fmtException("find unshare: %w", err).throw()
	}

	args := []string{"unshare", "-U", "-m", "-r"}

	if n.Pool != "network" {
		args = append(args, "-n")
	}

	args = append(args, selfPath(), "exec")

	cmd := &exec.Cmd{
		Path:   unsharePath,
		Args:   args,
		Stdin:  bytes.NewReader(payload),
		Stdout: out,
		Stderr: out,
	}

	return cmd.Run()
}
