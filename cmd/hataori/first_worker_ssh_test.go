package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Both the SSH clients and each inetd-mode sshd are children of the test,
// not of the killable main. No listener forks unrecorded connection children.
// The private broker accepts only the two SSH invocation forms used by the
// worker pool. It never accepts a command path, environment or cleanup PID.
type firstWorkerSSHOwner struct {
	brokerInfo                         os.FileInfo
	active                             bool
	beforeStart                        func()
	cli                                firstWorkerIdentity
	main                               firstWorkerIdentity
	helpers                            []firstWorkerIdentity
	processes                          *firstWorkerProcesses
	ssh, config, token, socket, remote string
	broker                             *net.UnixListener
	listener                           *net.TCPListener
	mu                                 sync.Mutex
	closed                             bool
	connections                        map[net.Conn]bool
	masters                            map[string]*firstWorkerMaster
	handlers                           sync.WaitGroup
	errors                             []error
}
type firstWorkerMaster struct {
	process           *firstWorkerProcess
	directory, socket os.FileInfo
}
type firstWorkerSSHRequest struct {
	Token string
	Args  []string
}
type firstWorkerSSHFrame struct {
	Out, Err []byte
	Exit     *int
}

func (o *firstWorkerSSHOwner) connection(c net.Conn) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || !o.active {
		c.Close()
		return false
	}
	o.connections[c] = true
	o.handlers.Add(1)
	return true
}
func (o *firstWorkerSSHOwner) finished(c net.Conn) {
	c.Close()
	o.mu.Lock()
	delete(o.connections, c)
	o.mu.Unlock()
	o.handlers.Done()
}
func (o *firstWorkerSSHOwner) recordError(err error) {
	if err != nil {
		o.mu.Lock()
		o.errors = append(o.errors, err)
		o.mu.Unlock()
	}
}
func (o *firstWorkerSSHOwner) serve(server, serverConfig string, log *os.File) {
	go func() {
		for {
			c, err := o.listener.AcceptTCP()
			if err != nil {
				return
			}
			if !o.connection(c) {
				continue
			}
			go func() {
				defer o.finished(c)
				o.mu.Lock()
				if !o.active || o.closed {
					o.mu.Unlock()
					return
				}
				file, err := c.File()
				if err != nil {
					o.mu.Unlock()
					o.recordError(err)
					return
				}
				r, err := o.processes.start(server, []string{"-i", "-e", "-f", serverConfig}, os.Environ(), []*os.File{file, file, log})
				o.mu.Unlock()
				file.Close()
				if err != nil {
					o.recordError(err)
					return
				}
				<-r.done
				o.recordError(r.err)
			}()
		}
	}()
	go func() {
		for {
			c, err := o.broker.AcceptUnix()
			if err != nil {
				return
			}
			if !o.connection(c) {
				continue
			}
			go func() { defer o.finished(c); o.run(c) }()
		}
	}()
}

// Exact shapes prevent the wrapper becoming an arbitrary SSH/command proxy.
// A mux request can address only a control path this owner created as a master.
func (o *firstWorkerSSHOwner) arguments(args []string) ([]string, error) {
	master := []string{"-M", "-S", "", "-N", "-n", "-T", "-o", "ControlPersist=no", "-o", "ForkAfterAuthentication=no", "-o", "ClearAllForwardings=yes", "-o", "ExitOnForwardFailure=yes", "-o", "BatchMode=yes", "-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "CheckHostIP=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no", "-o", "GSSAPIDelegateCredentials=no", "-o", "PermitLocalCommand=no", "-o", "RemoteCommand=none", "-o", "AddKeysToAgent=no", "-o", "ConnectTimeout=5", "-o", "ConnectionAttempts=1", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2", "--", "first-worker"}
	same := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	if len(args) == len(master) {
		master[2] = args[2]
		if !same(args, master) || !filepath.IsAbs(args[2]) {
			return nil, errors.New("fixture refused master arguments")
		}
		// The caller supplies only the newly minted control path. Require an
		// absent leaf in a private pool generation; never adopt an existing master.
		dir := filepath.Dir(args[2])
		info, err := os.Lstat(dir)
		if err != nil || info.Mode().Perm() != 0700 || !strings.HasPrefix(filepath.Base(dir), "hataori-workerssh-") || !info.IsDir() || filepath.Base(args[2]) != "control.sock" {
			return nil, errors.New("fixture refused control directory")
		}
		if _, err := os.Lstat(args[2]); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("fixture refused existing control socket")
		}
		return append([]string{"-F", o.config}, args...), nil
	}
	if len(args) != 14 && len(args) != 16 {
		return nil, errors.New("fixture refused mux arguments")
	}
	prefix := []string{"-F", "/dev/null", "-S", args[3], "-o", "ControlMaster=no", "-o", "ClearAllForwardings=no", "-o", "ExitOnForwardFailure=yes", "-O", args[11]}
	if !same(args[:12], prefix) || !same(args[len(args)-2:], []string{"--", "first-worker"}) {
		return nil, errors.New("fixture refused mux shape")
	}
	op := args[11]
	if len(args) == 16 { // forward/cancel include a single -L Unix forward.
		if (op != "forward" && op != "cancel") || args[12] != "-L" {
			return nil, errors.New("fixture refused forward")
		}
		if args[13] != filepath.Join(filepath.Dir(args[3]), "resident.sock")+":"+o.remote {
			return nil, errors.New("fixture refused foreign forward")
		}
	} else if op != "check" && op != "exit" {
		return nil, errors.New("fixture refused mux operation")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	masterRecord := o.masters[args[3]]
	if masterRecord == nil {
		return nil, errors.New("fixture refused foreign master")
	}
	select {
	case <-masterRecord.process.done:
		return nil, errors.New("fixture refused ended master")
	default:
	}
	directory, err := os.Lstat(filepath.Dir(args[3]))
	if err != nil || !os.SameFile(directory, masterRecord.directory) {
		return nil, errors.New("fixture refused replaced control directory")
	}
	socket, err := os.Lstat(args[3])
	if err != nil || socket.Mode()&os.ModeSocket == 0 || socket.Mode().Perm()&0077 != 0 {
		return nil, errors.New("fixture refused control identity")
	}
	if stat, ok := socket.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("fixture refused control owner")
	}
	if masterRecord.socket == nil {
		masterRecord.socket = socket
	} else if !os.SameFile(socket, masterRecord.socket) {
		return nil, errors.New("fixture refused replaced control socket")
	}
	return args, nil
}

func (o *firstWorkerSSHOwner) run(c *net.UnixConn) {
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var request firstWorkerSSHRequest
	if json.NewDecoder(io.LimitReader(c, 64<<10)).Decode(&request) != nil || request.Token != o.token {
		return
	}
	c.SetReadDeadline(time.Time{})
	helper, err := firstWorkerPeer(c)
	if err != nil {
		o.recordError(err)
		return
	}
	parent, err := firstWorkerParent(helper.pid)
	if err != nil {
		return
	}
	o.mu.Lock()
	main := o.main
	o.mu.Unlock()
	birth, ended, err := firstWorkerBirth(parent)
	if err != nil || ended || !((parent == main.pid && birth == main.birth) || (parent == o.cli.pid && birth == o.cli.birth)) {
		return
	}
	o.mu.Lock()
	o.helpers = append(o.helpers, helper)
	o.mu.Unlock()
	encoder := json.NewEncoder(c)
	var writeMu sync.Mutex
	send := func(frame firstWorkerSSHFrame) {
		writeMu.Lock()
		defer writeMu.Unlock()
		c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		encoder.Encode(frame)
	}
	args, err := o.arguments(request.Args)
	if err != nil {
		code := 255
		send(firstWorkerSSHFrame{Err: []byte(err.Error()), Exit: &code})
		return
	}
	stdout, outWriter, err := os.Pipe()
	if err != nil {
		o.recordError(err)
		return
	}
	defer stdout.Close()
	stderr, errWriter, err := os.Pipe()
	if err != nil {
		outWriter.Close()
		o.recordError(err)
		return
	}
	defer stderr.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		outWriter.Close()
		errWriter.Close()
		o.recordError(err)
		return
	}
	defer null.Close()
	o.mu.Lock()
	hold := o.beforeStart
	o.beforeStart = nil
	o.mu.Unlock()
	if hold != nil {
		hold()
	}
	o.mu.Lock()
	if !o.active || o.closed {
		o.mu.Unlock()
		outWriter.Close()
		errWriter.Close()
		return
	}
	r, err := o.processes.start(o.ssh, args, os.Environ(), []*os.File{null, outWriter, errWriter})
	o.mu.Unlock()
	outWriter.Close()
	errWriter.Close()
	if err != nil {
		o.recordError(err)
		return
	}
	if len(request.Args) > 2 && request.Args[0] == "-M" {
		directory, err := os.Lstat(filepath.Dir(request.Args[2]))
		if err != nil {
			o.recordError(err)
			o.processes.stop(r)
		} else {
			o.mu.Lock()
			o.masters[request.Args[2]] = &firstWorkerMaster{process: r, directory: directory}
			o.mu.Unlock()
		}
	}
	disconnected := make(chan struct{})
	go func() { io.Copy(io.Discard, c); close(disconnected) }()
	var drains sync.WaitGroup
	for i, reader := range []*os.File{stdout, stderr} {
		drains.Add(1)
		go func(i int, reader *os.File) {
			defer drains.Done()
			buffer := make([]byte, 4096)
			for {
				n, err := reader.Read(buffer)
				if n > 0 {
					frame := firstWorkerSSHFrame{}
					if i == 0 {
						frame.Out = append([]byte(nil), buffer[:n]...)
					} else {
						frame.Err = append([]byte(nil), buffer[:n]...)
					}
					send(frame)
				}
				if err != nil {
					return
				}
			}
		}(i, reader)
	}
	select {
	case <-disconnected:
		o.recordError(o.processes.stop(r))
	case <-r.done:
	}
	<-r.done
	o.recordError(r.err)
	drains.Wait()
	code := r.status
	send(firstWorkerSSHFrame{Exit: &code})
}

func (o *firstWorkerSSHOwner) setMain(pid int) {
	birth, ended, err := firstWorkerBirth(pid)
	if err != nil || ended {
		o.recordError(errors.New("fixture could not bind main generation"))
		return
	}
	o.mu.Lock()
	o.main = firstWorkerIdentity{pid, birth}
	o.active = true
	o.mu.Unlock()
}

func (o *firstWorkerSSHOwner) waitHandlers() error {
	done := make(chan struct{})
	go func() { o.handlers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(7 * time.Second):
		return errors.New("fixture SSH handler termination not confirmed")
	}
}
func (o *firstWorkerSSHOwner) cutGeneration() ([]firstWorkerIdentity, error) {
	o.mu.Lock()
	o.active = false
	o.main = firstWorkerIdentity{}
	for c := range o.connections {
		if tcp, ok := c.(*net.TCPConn); ok {
			tcp.CloseRead()
			tcp.CloseWrite()
		}
		c.Close()
	}
	o.mu.Unlock()
	// No new handler may join until setMain; in-flight authorized starts must
	// finish registration and exit before the final old-generation snapshot.
	if err := o.waitHandlers(); err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]firstWorkerIdentity(nil), o.helpers...), nil
}

func (o *firstWorkerSSHOwner) close() error {
	records := o.processes.snapshot()
	children, observationErr := firstWorkerDescendants(records)
	o.mu.Lock()
	o.closed = true
	o.active = false
	o.listener.Close()
	o.broker.Close()
	if info, err := os.Lstat(o.socket); err == nil && os.SameFile(info, o.brokerInfo) {
		if err := os.Remove(o.socket); err != nil {
			o.errors = append(o.errors, err)
		}
	}
	for c := range o.connections {
		// Shutdown closes the inherited TCP stream as well as the Go descriptor.
		if tcp, ok := c.(*net.TCPConn); ok {
			tcp.CloseRead()
			tcp.CloseWrite()
		}
		c.Close()
	}
	o.mu.Unlock()
	err := errors.Join(observationErr, o.processes.close(), o.waitHandlers())
	o.mu.Lock()
	children = append(children, o.helpers...)
	o.mu.Unlock()
	records = o.processes.snapshot()
	err = errors.Join(err, firstWorkerWaitExit(records, children))
	o.mu.Lock()
	defer o.mu.Unlock()
	if err != nil {
		o.errors = append(o.errors, err)
	}
	return errors.Join(o.errors...)
}

// This helper is only a broker client. Its real SSH child belongs to the test
// parent. Losing the main closes the lease even if main was killed before this
// helper began. Never adopt an already orphaned process as a parent generation.
func firstWorkerSSHClient(args []string) int {
	if len(args) < 3 {
		return 255
	}
	parent := os.Getppid()
	if parent <= 1 {
		return 255
	}
	c, err := net.Dial("unix", args[0])
	if err != nil {
		return 255
	}
	defer c.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if os.Getppid() != parent {
					c.Close()
					return
				}
			}
		}
	}()
	if json.NewEncoder(c).Encode(firstWorkerSSHRequest{Token: args[1], Args: args[2:]}) != nil {
		return 255
	}
	decoder := json.NewDecoder(c)
	for {
		var frame firstWorkerSSHFrame
		if decoder.Decode(&frame) != nil {
			return 255
		}
		if len(frame.Out) > 0 {
			os.Stdout.Write(frame.Out)
		}
		if len(frame.Err) > 0 {
			os.Stderr.Write(frame.Err)
		}
		if frame.Exit != nil {
			return *frame.Exit
		}
	}
}

func firstWorkerToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func firstWorkerForeignSSHClient(args []string) int {
	command := exec.Command(os.Args[0], append([]string{"first-worker-ssh"}, args...)...)
	if err := command.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		return 255
	}
	return 0
}
