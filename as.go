package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
)

type Exception struct {
	what func() error
}

func (self *Exception) throw() {
	panic(self)
}

func (self *Exception) catch(cb func(*Exception)) {
	if self != nil {
		cb(self)
	}
}

func (self *Exception) fatal(code int, prefix string) {
	fmt.Fprintf(os.Stderr, "%s%s: %v%s\n", R, prefix, self.what(), RST)
	os.Exit(code)
}

func newException(e error) *Exception {
	return &Exception{
		what: func() error {
			return e
		},
	}
}

func fmtException(format string, args ...any) *Exception {
	return newException(fmt.Errorf(format, args...))
}

func try(cb func()) (err *Exception) {
	defer func() {
		if rec := recover(); rec != nil {
			if exc, ok := rec.(*Exception); ok {
				err = exc
			} else {
				// personality check failed
				panic(rec)
			}
		}
	}()

	cb()

	return nil
}

const (
	ESC = "\x1b"
	RST = ESC + "[0m"
	R   = ESC + "[91m"
	G   = ESC + "[92m"
	Y   = ESC + "[93m"
	B   = ESC + "[94m"
	M   = ESC + "[95m"
)

func color(color string, s string) string {
	return color + s + RST
}

type semaphore struct {
	ch chan struct{}
}

func newSemaphore(n int) *semaphore {
	return &semaphore{
		ch: make(chan struct{}, n),
	}
}

func (self *semaphore) acquire() {
	self.ch <- struct{}{}
}

func (self *semaphore) release() {
	<-self.ch
}

type Cmd struct {
	Args  []string          `json:"args"`
	Stdin string            `json:"stdin"`
	Env   map[string]string `json:"env"`
}

type Node struct {
	InDirs  []string `json:"in_dir"`
	OutDirs []string `json:"out_dir"`
	Cmds    []Cmd    `json:"cmd"`
	Pool    string   `json:"pool"`
	Isolate bool     `json:"isolate"`
	Tmpfs   bool     `json:"tmpfs"`
	Tmp     string   `json:"tmp"`
}

type Graph struct {
	Nodes    []Node         `json:"nodes"`
	Targets  []string       `json:"targets"`
	Pools    map[string]int `json:"pools"`
	IxRoot   string         `json:"ix_root"`
	TrashDir string         `json:"trash_dir"`
}

func newGraph(r io.Reader) *Graph {
	graph := &Graph{}

	if err := json.NewDecoder(r).Decode(graph); err != nil {
		fmtException("can not parse input graph: %w", err).throw()
	}

	return graph
}

func (self *Graph) execute() {
	newExecutor(self).visitAll(self.Targets)
}

func toFiles(dirs []string) []string {
	res := []string{}

	for _, d := range dirs {
		res = append(res, d+"/touch")
	}

	return res
}

func outs(node *Node) []string {
	return toFiles(node.OutDirs)
}

func ins(node *Node) []string {
	return toFiles(node.InDirs)
}

func checkExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

func envMap(cmd *Cmd, thrs int) map[string]string {
	ret := make(map[string]string, len(cmd.Env)+2)

	for k, v := range cmd.Env {
		ret[k] = v
	}

	ret["make_thrs"] = fmt.Sprintf("%d", thrs)
	ret["IX_RANDOM"] = fmt.Sprintf("%d", rand.Int63())

	return ret
}

func complete(node *Node, out io.Writer) bool {
	for _, o := range outs(node) {
		if !checkExists(o) {
			return false
		}

		fmt.Fprintln(out, color(G, "READY "+o))
	}

	return true
}

func moveToTrash(trashDir, d string) {
	target := filepath.Join(trashDir, fmt.Sprintf("%d", rand.Int63()))

	err := os.Rename(d, target)

	if err == nil {
		return
	}

	if os.IsNotExist(err) {
		return
	}

	if rmErr := os.RemoveAll(d); rmErr != nil && !os.IsNotExist(rmErr) {
		fmtException("rm %s: %w", d, rmErr).throw()
	}
}

func prepareDir(trashDir, d string) {
	moveToTrash(trashDir, d)

	if err := os.MkdirAll(d, 0755); err != nil {
		fmtException("mkdir %s: %w", d, err).throw()
	}
}

func cat[T any](a []T, b []T) []T {
	return append(append([]T{}, a...), b...)
}

func (self *executor) executeNode(node *Node, thrs int, out io.Writer) {
	for _, d := range node.OutDirs {
		prepareDir(self.trashDir, d)
	}

	if node.Tmp != "" {
		prepareDir(self.trashDir, node.Tmp)
	}

	net := node.Pool == "network"
	nouts := outs(node)

	for _, o := range nouts {
		if net {
			fmt.Fprintln(out, color(M, self.complete()+" FETCH "+o))
		} else {
			fmt.Fprintln(out, color(B, self.complete()+" BUILD "+o))
		}
	}

	for i := range node.Cmds {
		cmd := &node.Cmds[i]

		if err := runWrapped(cmd, node, envMap(cmd, thrs), out); err != nil {
			for _, d := range node.OutDirs {
				moveToTrash(self.trashDir, d)
			}

			fmtException("%v failed with %w", cat(nouts, cmd.Args), err).throw()
		}
	}

	if node.Tmp != "" {
		moveToTrash(self.trashDir, node.Tmp)
	}

	syscall.Sync()

	for _, o := range outs(node) {
		if file, err := os.Create(o); err == nil {
			file.Close()
		}

		fmt.Fprintln(out, color(G, self.complete()+" LEAVE "+o))
	}

	syscall.Sync()
}

type future struct {
	f func()
	o sync.Once
}

func (self *future) callOnce() {
	self.o.Do(self.f)
}

type executor struct {
	thr      int
	trashDir string
	out      map[string]*future
	sem      map[string]*semaphore
	wait     atomic.Uint64
	done     atomic.Uint64
}

func (self *executor) complete() string {
	return fmt.Sprintf("{%d/%d}", self.done.Load()+1, self.wait.Load())
}

func (self *executor) execute(node *Node) {
	buf := os.Stdout

	if complete(node, buf) {
		return
	}

	self.wait.Add(1)
	defer self.done.Add(1)
	self.visitAll(ins(node))
	sem, _ := self.sem[node.Pool]
	sem.acquire()
	defer sem.release()
	self.executeNode(node, self.thr, buf)
}

func newNodeFuture(ex *executor, node *Node) *future {
	return &future{f: func() {
		ex.execute(node)
	}}
}

func newExecutor(graph *Graph) *executor {
	res := &executor{
		out:      map[string]*future{},
		sem:      map[string]*semaphore{},
		trashDir: graph.TrashDir,
	}

	if err := os.MkdirAll(res.trashDir, 0755); err != nil {
		fmtException("mkdir trash %s: %w", res.trashDir, err).throw()
	}

	// construct backrefs
	for i := range graph.Nodes {
		node := &graph.Nodes[i]
		futu := newNodeFuture(res, node)

		for _, out := range outs(node) {
			if _, ok := res.out[out]; ok {
				fmtException("multiple nodes generate output %s", out).throw()
			}

			res.out[out] = futu
		}
	}

	// construct scheduler
	for pool, count := range graph.Pools {
		res.sem[pool] = newSemaphore(count)
	}

	// misc
	res.thr = graph.Pools["threads"]

	// validate
	for _, node := range graph.Nodes {
		for _, in := range ins(&node) {
			if _, ok := res.out[in]; !ok {
				fmtException("no node generate %s", in).throw()
			}
		}

		if _, ok := res.sem[node.Pool]; !ok {
			fmtException("bad pool %s", node.Pool).throw()
		}
	}

	return res
}

func (self *executor) visitAll(nodes []string) {
	wg := &sync.WaitGroup{}

	for _, n := range nodes {
		f := self.out[n]

		wg.Add(1)

		go func() {
			defer wg.Done()

			try(func() {
				f.callOnce()
			}).catch(func(exc *Exception) {
				exc.fatal(2, "subcommand error")
			})
		}()
	}

	wg.Wait()
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "exec" {
		try(func() {
			cliExec()
		}).catch(func(exc *Exception) {
			exc.fatal(2, "assemble exec")
		})

		return
	}

	try(func() {
		newGraph(os.Stdin).execute()
	}).catch(func(exc *Exception) {
		exc.fatal(1, "abort")
	})
}
