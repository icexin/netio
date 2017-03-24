package main

import (
	"encoding/gob"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/hashicorp/yamux"
	"github.com/kr/pty"
)

type Session struct {
	conn net.Conn

	cmdDesc command
	cmd     *exec.Cmd

	pty *os.File

	bus *yamux.Session

	scmd   *yamux.Stream
	cmddec *gob.Decoder
	cmdenc *gob.Encoder

	srpc    *yamux.Stream
	sstdin  *yamux.Stream
	sstdout *yamux.Stream
	sstderr *yamux.Stream
}

func NewSession(conn net.Conn) *Session {
	return &Session{conn: conn}
}

func (s *Session) accpetStream() error {
	var err error
	s.bus, err = yamux.Server(s.conn, nil)
	if err != nil {
		return err
	}

	if s.scmd, err = s.bus.AcceptStream(); err != nil {
		return err
	}
	if s.srpc, err = s.bus.AcceptStream(); err != nil {
		return err
	}
	if s.sstdin, err = s.bus.AcceptStream(); err != nil {
		return err
	}
	if s.sstdout, err = s.bus.AcceptStream(); err != nil {
		return err
	}
	if s.sstderr, err = s.bus.AcceptStream(); err != nil {
		return err
	}
	s.cmdenc = gob.NewEncoder(s.scmd)
	s.cmddec = gob.NewDecoder(s.scmd)
	return nil
}

func (s *Session) sendExitCode(code int) error {
	return s.cmdenc.Encode(code)
}

func (s *Session) setupCommand() error {
	err := s.cmddec.Decode(&s.cmdDesc)
	if err != nil {
		return err
	}
	desc := &s.cmdDesc
	cmd := exec.Command(desc.Name, desc.Argv...)
	cmd.Env = desc.Envs
	cmd.Dir = desc.WorkDir
	s.cmd = cmd
	return nil
}

func (s *Session) startCommand() error {
	cmd := s.cmd
	var err error
	var stdin io.WriteCloser
	var stdout, stderr io.Reader
	if s.cmdDesc.TTY {
		var fd *os.File
		fd, err = pty.Start(cmd)
		if err != nil {
			return err
		}
		stdin = fd
		stdout = fd
		stderr = strings.NewReader("")
		s.pty = fd
	} else {
		if stdin, err = cmd.StdinPipe(); err != nil {
			return err
		}
		if stdout, err = cmd.StdoutPipe(); err != nil {
			return err
		}
		if stderr, err = cmd.StderrPipe(); err != nil {
			return err
		}
		err = cmd.Start()
		if err != nil {
			return err
		}
	}
	go connect(newOutputPeer(stdin, stdout, stderr),
		newInputPeer(s.sstdin, s.sstdout, s.sstderr))
	return nil
}

func (s *Session) Serve() error {
	defer func() {
		err := recover()
		if err != nil {
			log.Printf("panic %s\n%s", err, debug.Stack())
		}
	}()

	err := s.accpetStream()
	if err != nil {
		return err
	}

	err = s.setupCommand()
	if err != nil {
		return err
	}

	log.Printf("cmd %q", append([]string{s.cmdDesc.Name}, s.cmdDesc.Argv...))

	err = s.startCommand()
	if err != nil {
		return err
	}

	go s.serveRPC()

	var code int
	err = s.cmd.Wait()
	if err != nil {
		code = err.(*exec.ExitError).Sys().(syscall.WaitStatus).ExitStatus()
	}
	s.sendExitCode(code)

	// waiting client side close connection
	io.Copy(ioutil.Discard, s.scmd)
	return nil
}
