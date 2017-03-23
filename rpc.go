package main

import (
	"encoding/gob"
	"log"
	"os"
	"syscall"
	"time"
	"unsafe"
)

type WinResizeRequest struct {
	Rows    uint16
	Columns uint16
}

func encodeGob(enc *gob.Encoder, x interface{}) error {
	return enc.Encode(&x)
}

func handleWinResize(req WinResizeRequest, pty *os.File) {
	window := struct {
		row uint16
		col uint16
		x   uint16
		y   uint16
	}{
		req.Rows,
		req.Columns,
		0,
		0,
	}
	syscall.Syscall(
		syscall.SYS_IOCTL,
		pty.Fd(),
		syscall.TIOCSWINSZ,
		uintptr(unsafe.Pointer(&window)),
	)
}

func (s *Session) serveRPC() {
	var x interface{}
	dec := gob.NewDecoder(s.srpc)
	for {
		err := dec.Decode(&x)
		if err != nil {
			break
		}
		switch req := x.(type) {
		case WinResizeRequest:
			if s.cmdDesc.TTY {
				handleWinResize(req, s.pty)
			}
		}
	}
	time.Sleep(time.Millisecond)
	if s.cmd.ProcessState == nil {
		log.Printf("process %d killed", s.cmd.Process.Pid)
		s.cmd.Process.Kill()
		s.cmd.Wait()
	}
}

func init() {
	gob.Register(WinResizeRequest{})
}
