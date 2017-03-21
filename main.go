package main

import (
	"encoding/gob"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/crypto/ssh/terminal"

	"github.com/BurntSushi/toml"
	"github.com/hashicorp/yamux"
	"github.com/kr/pty"
)

type stringListFlags []string

func (l *stringListFlags) String() string {
	return fmt.Sprintf("%v", []string(*l))
}

func (l *stringListFlags) Set(v string) error {
	*l = append(*l, v)
	return nil
}

var (
	addr       = flag.String("addr", "", "listen address or server address")
	serverMode = flag.Bool("s", false, "server mode")
	allocTTY   = flag.Bool("t", false, "alloc tty on server")
	compress   = flag.Bool("c", false, "compress on stream")
	workdir    = flag.String("w", "", "workdir")
	envs       stringListFlags
)

var (
	gcfg config
)

type config struct {
	Addr string
}

type command struct {
	Name     string
	Argv     []string
	TTY      bool
	Envs     []string
	WorkDir  string
	Compress bool
}

func newCommand(name string, argv ...string) *command {
	return &command{
		Name: name,
		Argv: argv,
	}
}

type inputPeer struct {
	Stdin          io.Reader
	Stdout, Stderr io.WriteCloser
}

func newInputPeer(in io.Reader, out, err io.WriteCloser) *inputPeer {
	return &inputPeer{
		Stdin:  in,
		Stdout: out,
		Stderr: err,
	}
}

type outputPeer struct {
	Stdin          io.WriteCloser
	Stdout, Stderr io.Reader
}

func newOutputPeer(in io.WriteCloser, out, err io.Reader) *outputPeer {
	return &outputPeer{
		Stdin:  in,
		Stdout: out,
		Stderr: err,
	}
}

func connect(out *outputPeer, in *inputPeer) {
	w := new(sync.WaitGroup)
	w.Add(2)
	go func() {
		io.Copy(out.Stdin, in.Stdin)
		out.Stdin.Close()
	}()

	go func() {
		io.Copy(in.Stdout, out.Stdout)
		in.Stdout.Close()
		w.Done()
	}()

	go func() {
		io.Copy(in.Stderr, out.Stderr)
		in.Stderr.Close()
		w.Done()
	}()

	w.Wait()
}

func listenCloseAndClean(sess *yamux.Session, app *exec.Cmd) {
	sess.Accept()
	if app.ProcessState == nil {
		log.Printf("process %d killed", app.Process.Pid)
		app.Process.Kill()
		app.Wait()
	}
}

func serveConn(conn net.Conn) {
	defer conn.Close()
	defer func() {
		err := recover()
		if err != nil {
			log.Printf("panic:%s\n%s", err, debug.Stack())
		}
	}()

	session, err := yamux.Server(conn, nil)
	if err != nil {
		log.Printf("make session error:%s", err)
		return
	}
	cmdStream, _ := session.AcceptStream()
	stdin, _ := session.AcceptStream()
	stdout, _ := session.AcceptStream()
	stderr, _ := session.AcceptStream()

	// decode command
	var cmd command
	err = gob.NewDecoder(cmdStream).Decode(&cmd)
	if err != nil {
		log.Printf("decode command error:%s", err)
		return
	}
	log.Printf("%s %s %q", conn.RemoteAddr(), cmd.Name, cmd.Argv)

	app := exec.Command(cmd.Name, cmd.Argv...)
	app.Env = cmd.Envs
	app.Dir = cmd.WorkDir
	var appStdin io.WriteCloser
	var appStdout, appStderr io.Reader
	if cmd.TTY {
		fd, err := pty.Start(app)
		if err != nil {
			io.WriteString(stderr, err.Error())
			gob.NewEncoder(cmdStream).Encode(-1)
			return
		}
		appStdin = fd
		appStdout = fd
		appStderr = strings.NewReader("")
	} else {
		appStdin, _ = app.StdinPipe()
		appStdout, _ = app.StdoutPipe()
		appStderr, _ = app.StderrPipe()
		err = app.Start()
		if err != nil {
			io.WriteString(stderr, err.Error())
			gob.NewEncoder(cmdStream).Encode(-1)
			return
		}

	}

	go listenCloseAndClean(session, app)
	go connect(newOutputPeer(appStdin, appStdout, appStderr),
		newInputPeer(stdin, stdout, stderr))

	var code int
	err = app.Wait()
	if err != nil {
		code = err.(*exec.ExitError).Sys().(syscall.WaitStatus).ExitStatus()
	}
	gob.NewEncoder(cmdStream).Encode(code)

	// waiting client side close connection
	io.Copy(ioutil.Discard, cmdStream)
	log.Printf("%s closed", conn.RemoteAddr())
	return
}

func runServer() {
	l, err := net.Listen("tcp", gcfg.Addr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go serveConn(conn)
	}
}

type nopCloser struct{ io.Writer }

func (c nopCloser) Close() error {
	return nil
}

func ignoreSigs() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGINT, syscall.SIGSTOP)
	for range c {
	}
}

func runClient() int {
	if flag.NArg() < 1 {
		log.Fatal("usage netio [option] cmd ...")
	}
	cmdname := flag.Arg(0)
	cmdargv := flag.Args()[1:]

	conn, err := net.Dial("tcp", gcfg.Addr)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	session, err := yamux.Client(conn, nil)
	if err != nil {
		log.Fatalf("make session error:%s", err)
	}

	cmdStream, _ := session.OpenStream()
	stdin, _ := session.OpenStream()
	stdout, _ := session.OpenStream()
	stderr, _ := session.OpenStream()

	cmd := newCommand(cmdname, cmdargv...)
	cmd.TTY = *allocTTY
	cmd.Envs = []string(envs)
	cmd.WorkDir = *workdir
	gob.NewEncoder(cmdStream).Encode(cmd)

	if *allocTTY {
		// ignore signals
		go ignoreSigs()

		// make terminal into raw mode
		oldState, err := terminal.MakeRaw(0)
		if err != nil {
			log.Printf("make raw terminal error:%s", err)
			return -1
		}
		defer terminal.Restore(0, oldState)
	}

	// when connect returns, we know command has exited
	connect(newOutputPeer(stdin, stdout, stderr),
		newInputPeer(os.Stdin, nopCloser{os.Stdout}, nopCloser{os.Stderr}))

	// get command exit code
	var code int
	err = gob.NewDecoder(cmdStream).Decode(&code)
	if err != nil {
		log.Printf("decode exit code:%s", err)
		return -1
	}

	// 客户端主动关闭连接, 让服务端知道客户端已经接收完所有的stdout和stderr，同时也已经接收到
	// exit code，这个时候服务端可以放心关闭客户端连接
	session.Close()
	conn.Close()
	return code
}

func parseConfig() error {
	cfgpath := filepath.Join(os.Getenv("HOME"), ".netiorc")
	if _, err := os.Stat(cfgpath); err == nil {
		_, err = toml.DecodeFile(cfgpath, &gcfg)
		if err != nil {
			return fmt.Errorf("parse .netiorc error:%s", err)
		}
	}
	if *addr != "" {
		gcfg.Addr = *addr
	}
	return nil
}

func main() {
	flag.Var(&envs, "e", "envs")
	flag.Parse()

	err := parseConfig()
	if err != nil {
		log.Fatal(err)
	}

	if !*serverMode {
		os.Exit(runClient())
	} else {
		runServer()
	}
}
