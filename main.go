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
	"path/filepath"
	"sync"
	"syscall"

	"github.com/BurntSushi/toml"
	"github.com/hashicorp/yamux"
)

var (
	addr       = flag.String("addr", "", "listen address or server address")
	clientMode = flag.Bool("c", false, "client mode")
	allocTTY   = flag.Bool("t", false, "alloc tty on server")
)

var (
	gcfg config
)

type config struct {
	Addr string
}

type command struct {
	Name string
	Argv []string
	TTY  bool
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

func serveConn(conn net.Conn) {
	defer conn.Close()
	session, err := yamux.Server(conn, nil)
	if err != nil {
		log.Printf("make session error:%s", err)
		return
	}
	cmdStream, _ := session.AcceptStream()
	stdin, _ := session.AcceptStream()
	stdout, _ := session.AcceptStream()
	stderr, _ := session.AcceptStream()

	var cmd command
	err = gob.NewDecoder(cmdStream).Decode(&cmd)
	if err != nil {
		log.Printf("decode command error:%s", err)
		return
	}
	log.Printf("%s %s %q", conn.RemoteAddr(), cmd.Name, cmd.Argv)

	app := exec.Command(cmd.Name, cmd.Argv...)
	appStdin, _ := app.StdinPipe()
	appStdout, _ := app.StdoutPipe()
	appStderr, _ := app.StderrPipe()

	go connect(newOutputPeer(appStdin, appStdout, appStderr),
		newInputPeer(stdin, stdout, stderr))

	err = app.Start()
	if err != nil {
		io.WriteString(stderr, err.Error())
		gob.NewEncoder(cmdStream).Encode(-1)
		return
	}

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

func runClient() {
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
		log.Printf("make session error:%s", err)
		return
	}

	cmdStream, _ := session.OpenStream()
	stdin, _ := session.OpenStream()
	stdout, _ := session.OpenStream()
	stderr, _ := session.OpenStream()

	cmd := newCommand(cmdname, cmdargv...)
	cmd.TTY = *allocTTY
	gob.NewEncoder(cmdStream).Encode(cmd)

	// when connect returns, we know command has exited
	connect(newOutputPeer(stdin, stdout, stderr),
		newInputPeer(os.Stdin, nopCloser{os.Stdout}, nopCloser{os.Stderr}))

	// get command exit code
	var code int
	err = gob.NewDecoder(cmdStream).Decode(&code)
	if err != nil {
		log.Printf("decode exit code:%s", err)
		return
	}

	// 客户端主动关闭连接, 让服务端知道客户端已经接收完所有的stdout和stderr，同时也已经接收到
	// exit code，这个时候服务端可以放心关闭客户端连接
	session.Close()
	conn.Close()
	os.Exit(int(code))
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
	flag.Parse()

	err := parseConfig()
	if err != nil {
		log.Fatal(err)
	}

	if *clientMode {
		runClient()
	} else {
		runServer()
	}
}
