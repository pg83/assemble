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
	err := unix.Mount(source, target, fstype, flags, data)

	if err != nil {
		ThrowFmt("mount %s -> %s (fs=%s flags=%x): %w", source, target, fstype, flags, err)
	}
}

func bindRO(src, dst string) {
	mountSyscall(src, dst, "", unix.MS_BIND, "")
	mountSyscall("", dst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
}

func mkdirAll(p string) {
	Throw(os.MkdirAll(p, 0755))
}

func setupTmpfs(target string) {
	mountSyscall("tmpfs", target, "tmpfs", 0, "")
}

func setupShadow(inDirs []string, outDirs []string, storeInside string) {
	for _, d := range inDirs {
		target := filepath.Join(storeInside, filepath.Base(d))
		mkdirAll(target)
		bindRO(d, target)
	}

	for _, d := range outDirs {
		mkdirAll(d)
		target := filepath.Join(storeInside, filepath.Base(d))
		mkdirAll(target)
		mountSyscall(d, target, "", unix.MS_BIND, "")
	}
}

func copyFileMode(src, dst string, mode os.FileMode) {
	data := Throw2(os.ReadFile(src))
	Throw(os.WriteFile(dst, data, mode))
}

func copyTree(src, dst string) {
	Throw(filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)

		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}

		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)

			if err != nil {
				return err
			}

			return os.Symlink(link, target)
		}

		data, err := os.ReadFile(path)

		if err != nil {
			return err
		}

		return os.WriteFile(target, data, info.Mode().Perm())
	}))
}

func setupRoot(cfg *ExecCfg) {
	n := &cfg.Node

	if n.Tmp == "" {
		return
	}

	if !n.Isolate && !n.Tmpfs {
		mkdirAll(n.Tmp)
		return
	}

	buildRoot := filepath.Dir(n.Tmp)
	ixRoot := filepath.Dir(buildRoot)

	if n.Tmpfs {
		setupTmpfs(buildRoot)
		mkdirAll(n.Tmp)
	}

	if !n.Isolate {
		return
	}

	prepRoot := filepath.Join(n.Tmp, "root")
	mkdirAll(prepRoot)
	mountSyscall(prepRoot, prepRoot, "", unix.MS_BIND, "")

	inside := func(p string) string {
		return filepath.Join(prepRoot, strings.TrimPrefix(p, "/"))
	}

	procDir := inside("/proc")
	mkdirAll(procDir)
	mountSyscall("/proc", procDir, "", unix.MS_BIND|unix.MS_REC, "")

	devDir := inside("/dev")
	mkdirAll(devDir)

	for _, d := range []string{"null", "zero", "random", "urandom"} {
		target := filepath.Join(devDir, d)
		Throw2(os.Create(target)).Close()
		mountSyscall("/dev/"+d, target, "", unix.MS_BIND, "")
	}

	for _, s := range []string{"stdin", "stdout", "stderr"} {
		link := Throw2(os.Readlink("/dev/" + s))
		Throw(os.Symlink(link, filepath.Join(devDir, s)))
	}

	shmDir := filepath.Join(devDir, "shm")
	mkdirAll(shmDir)
	mountSyscall("/dev/shm", shmDir, "", unix.MS_BIND, "")

	binDir := inside("/bin")
	mkdirAll(binDir)
	copyFileMode(lookupPathExec("sh", cfg.Env["PATH"]), filepath.Join(binDir, "sh"), 0755)

	usrBinDir := inside("/usr/bin")
	mkdirAll(usrBinDir)
	copyFileMode(lookupPathExec("env", cfg.Env["PATH"]), filepath.Join(usrBinDir, "env"), 0755)

	etcDir := inside("/etc")
	mkdirAll(etcDir)
	Throw(os.WriteFile(filepath.Join(etcDir, "passwd"), []byte("root:x:0:0:none:/:/bin/sh\n"), 0644))
	Throw(os.WriteFile(filepath.Join(etcDir, "group"), []byte("root:x:0:\n"), 0644))

	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		copyFileMode("/etc/resolv.conf", filepath.Join(etcDir, "resolv.conf"), 0644)
	}

	if _, err := os.Stat("/etc/ssl"); err == nil {
		copyTree("/etc/ssl", filepath.Join(etcDir, "ssl"))
	}

	storeInside := inside(filepath.Join(ixRoot, "store"))
	mkdirAll(storeInside)

	if n.Isolate {
		setupShadow(n.InDirs, n.OutDirs, storeInside)
	} else {
		mountSyscall(filepath.Join(ixRoot, "store"), storeInside, "", unix.MS_BIND|unix.MS_REC, "")
	}

	buildInside := inside(filepath.Join(ixRoot, "build"))
	mkdirAll(buildInside)
	mkdirAll(filepath.Join(buildInside, filepath.Base(n.Tmp)))

	putOld := filepath.Join(prepRoot, "old_root")
	mkdirAll(putOld)
	Throw(unix.PivotRoot(prepRoot, putOld))
	Throw(os.Chdir("/"))
	Throw(unix.Unmount("/old_root", unix.MNT_DETACH))
	Throw(os.Remove("/old_root"))
}

func lookupPathExec(prog, path string) string {
	if strings.Contains(prog, "/") {
		return prog
	}

	for _, p := range strings.Split(path, ":") {
		full := filepath.Join(p, prog)
		info, err := os.Stat(full)

		if err == nil && !info.IsDir() {
			return full
		}
	}

	ThrowFmt("cannot find %q in PATH=%q", prog, path)
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
	return Throw2(os.Executable())
}

func dupStdinFromBytes(payload []byte) {
	r, w := Throw3(os.Pipe())

	if len(payload) > 0 {
		Throw2(unix.FcntlInt(w.Fd(), unix.F_SETPIPE_SZ, len(payload)))
		Throw2(w.Write(payload))
	}

	Throw(w.Close())
	Throw(unix.Dup2(int(r.Fd()), 0))
	Throw(r.Close())
}

func cliExec() {
	var cfg ExecCfg

	Throw(json.NewDecoder(os.Stdin).Decode(&cfg))

	if len(cfg.Cmd.Args) == 0 {
		ThrowFmt("empty args")
	}

	setupRoot(&cfg)

	prog := lookupPathExec(cfg.Cmd.Args[0], cfg.Env["PATH"])

	dupStdinFromBytes([]byte(cfg.Cmd.Stdin))

	Throw(syscall.Exec(prog, cfg.Cmd.Args, envToList(cfg.Env)))
}

func runWrapped(c *Cmd, n *Node, env map[string]string, out io.Writer) error {
	payload := Throw2(json.Marshal(ExecCfg{Node: *n, Cmd: *c, Env: env}))

	var args []string
	var unsh []string

	if n.Pool != "network" {
		unsh = append(unsh, "-n")
	}

	if n.Isolate || n.Tmpfs {
		unsh = append(unsh, "-m")
	}

    if len(unsh) > 0 {
        args = append(args, "unshare", "-U", "-r")
        args = append(args, unsh...)
    }

    args = append(args, selfPath(), "exec")

	cmd := &exec.Cmd{
		Path:   Throw2(exec.LookPath(args[0])),
		Args:   args,
		Stdin:  bytes.NewReader(payload),
		Stdout: out,
		Stderr: out,
	}

	return cmd.Run()
}
