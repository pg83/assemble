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

func fatal(exc *Exception, code int, prefix string) {
	fmt.Fprintf(os.Stderr, "%s%s: %v%s\n", R, prefix, exc, RST)
	os.Exit(code)
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

	Throw(json.NewDecoder(r).Decode(graph))

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

	rmErr := os.RemoveAll(d)

	if rmErr != nil && !os.IsNotExist(rmErr) {
		Throw(rmErr)
	}
}

func prepareDir(trashDir, d string) {
	moveToTrash(trashDir, d)
	Throw(os.MkdirAll(d, 0755))
}

func cat[T any](a []T, b []T) []T {
	return append(append([]T{}, a...), b...)
}

func (self *executor) executeNode(node *Node, thrs int, out io.Writer) {
	for _, d := range node.OutDirs {
		prepareDir(self.trashDir, d)
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

		err := runWrapped(cmd, node, envMap(cmd, thrs), out)

		if err == nil {
			continue
		}

		for _, d := range node.OutDirs {
			moveToTrash(self.trashDir, d)
		}

		ThrowFmt("%v failed with %w", cat(nouts, cmd.Args), err)
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

	Throw(os.MkdirAll(res.trashDir, 0755))

	// construct backrefs
	for i := range graph.Nodes {
		node := &graph.Nodes[i]
		futu := newNodeFuture(res, node)

		for _, out := range outs(node) {
			if _, ok := res.out[out]; ok {
				ThrowFmt("multiple nodes generate output %s", out)
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
				ThrowFmt("no node generate %s", in)
			}
		}

		if _, ok := res.sem[node.Pool]; !ok {
			ThrowFmt("bad pool %s", node.Pool)
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

			Try(func() {
				f.callOnce()
			}).Catch(func(exc *Exception) {
				fatal(exc, 2, "subcommand error")
			})
		}()
	}

	wg.Wait()
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "exec" {
		Try(func() {
			cliExec()
		}).Catch(func(exc *Exception) {
			fatal(exc, 2, "assemble exec")
		})

		return
	}

	Try(func() {
		newGraph(os.Stdin).execute()
	}).Catch(func(exc *Exception) {
		fatal(exc, 1, "abort")
	})
}
