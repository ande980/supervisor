package supervisor

// +build linux

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/mholt/archiver"
)

// WebApp defines the contract for the main loop in a long running API.
type WebApp func(context.Context, net.Listener) error

var (
	image = flag.String("container-image", "alpine.3.8.tar.gz", "The container image to use as a root fs.")
)

// Supervisor holds the state we need to manage a child process.
// This is based on this series of talks by Liz Rice:
// https://www.youtube.com/watch?v=Utf-A4rODH8
// https://www.youtube.com/watch?v=_TsSmSu57Zo
// https://www.youtube.com/watch?v=8fi7uSYlOdc
type Supervisor struct {
	mu     sync.Mutex
	addr   string
	app    WebApp
	child  *exec.Cmd
	ln     net.Listener
	ctx    context.Context
	cancel context.CancelFunc
}

// New instantiates a supervisor layer with the specified address
// and main entrypoint of a web API.
func New(addr string, app WebApp) *Supervisor {
	return &Supervisor{
		addr: addr,
		app:  app,
	}
}

// Run is the entry point for the supervisor layer.
func (s *Supervisor) Run(ctx context.Context) error {
	if !flag.Parsed() {
		flag.Parse()
	}

	s.ctx, s.cancel = context.WithCancel(ctx)
	defer s.cancel()

	if err := createContainerFs(*image); err != nil {
		return err
	}

	ln, created, err := listener(s.addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	s.ln = ln

	if !created {
		return s.runChild()
	}

	return s.run()
}

func (s *Supervisor) run() error {
	f, err := file(s.ln)
	if err != nil {
		return err
	}
	defer f.Close()

	cmd := exec.Command("/proc/self/exe", os.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{f}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:   syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
		Unshareflags: syscall.CLONE_NEWNS,
		Setpgid:      true,
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cmd start: %v", err)
	}

	s.mu.Lock()
	s.child = cmd
	s.mu.Unlock()
	return s.runManaged()
}

func (s *Supervisor) runChild() error {
	if err := cgroups(); err != nil {
		return err
	}

	if err := syscall.Sethostname([]byte("container")); err != nil {
		return fmt.Errorf("setting hostname: %v", err)
	}

	if err := syscall.Chroot(".container"); err != nil {
		return fmt.Errorf("chroot: %v", err)
	}

	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir: %v", err)
	}

	if err := syscall.Mount("proc", "proc", "proc", 0, ""); err != nil {
		return fmt.Errorf("mount proc: %v", err)
	}
	defer func() {
		if err := syscall.Unmount("proc", 0); err != nil {
			fmt.Fprintf(os.Stderr, "unmount proc: %v", err)
		}
	}()

	return s.runUnmanaged()
}

func (s *Supervisor) runManaged() error {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	done := make(chan error, 1)
	go func() {
		if err := s.child.Wait(); err != nil {
			done <- fmt.Errorf("wait: %v", err)
		}
		close(done)
	}()

	select {
	case err := <-done:
		return err
	case <-s.ctx.Done():
		if err := s.ctx.Err(); err != nil && err != context.Canceled {
			return err
		}
		return nil
	case <-sigs:
		s.cancel()
		return <-done
	}
}

func (s *Supervisor) runUnmanaged() error {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	done := make(chan error, 1)
	go func() {
		done <- s.app(s.ctx, s.ln)
	}()

	select {
	case err := <-done:
		return err
	case <-s.ctx.Done():
		if err := s.ctx.Err(); err != nil && err != context.Canceled {
			return err
		}
		return nil
	case <-sigs:
		s.cancel()
		return <-done
	}
}

func cgroups() error {
	cgroups := "/sys/fs/cgroup/"
	pids := filepath.Join(cgroups, "pids")
	os.Mkdir(filepath.Join(pids, "container"), 0755)

	mem := filepath.Join(cgroups, "memory")
	os.Mkdir(mem, 0755)

	if err := ioutil.WriteFile(filepath.Join(mem, "memory.limit_in_bytes"), []byte(""), 0700); err != nil {
		return fmt.Errorf("setting mem limit: %v", err)
	}

	if err := ioutil.WriteFile(filepath.Join(pids, "container/pids.max"), []byte("20"), 0700); err != nil {
		return fmt.Errorf("setting max pids: %v", err)
	}
	// Removes the new cgroup in place after the container exits
	if err := ioutil.WriteFile(filepath.Join(pids, "container/notify_on_release"), []byte("1"), 0700); err != nil {
		return fmt.Errorf("notify on release: %v", err)
	}

	if err := ioutil.WriteFile(filepath.Join(pids, "container/cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0700); err != nil {
		return fmt.Errorf("writing procs: %v", err)
	}
	return nil
}

func file(ln net.Listener) (*os.File, error) {
	switch t := ln.(type) {
	case *net.TCPListener:
		f, err := t.File()
		if err != nil {
			return nil, fmt.Errorf("listener file: %v", err)
		}
		return f, nil
	default:
		return nil, fmt.Errorf("unsupported listener type: %T", ln)
	}
}

func createContainerFs(imagePath string) error {
	_, err := os.Stat(".container")
	if err == nil {
		return nil
	}

	if !os.IsNotExist(err) {
		return fmt.Errorf("stat container directory: %v", err)
	}
	if err := os.MkdirAll(".container", os.ModePerm); err != nil {
		return fmt.Errorf("creating container directory: %v", err)
	}

	return archiver.TarGz.Open(*image, ".container")
}

func listener(addr string) (net.Listener, bool, error) {
	f := os.NewFile(uintptr(3), os.Args[0])
	if f != nil {
		defer f.Close()
		ln, err := net.FileListener(f)
		return ln, false, err
	}

	// We have to create the listener and manage the child
	ln, err := net.Listen("tcp", addr)
	return ln, true, err
}
