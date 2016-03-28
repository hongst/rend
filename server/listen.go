package server

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"github.com/netflix/rend/binprot"
	"github.com/netflix/rend/common"
	"github.com/netflix/rend/handlers"
	"github.com/netflix/rend/metrics"
	"github.com/netflix/rend/orcas"
	"github.com/netflix/rend/textprot"
)

func ListenAndServe(l ListenArgs, s ServerConst, o orcas.OrcaConst, h1, h2 handlers.HandlerConst) {
	var listener net.Listener
	var err error

	switch l.Type {
	case ListenTCP:
		listener, err = net.Listen("tcp", fmt.Sprintf(":%d", l.Port))
		if err != nil {
			log.Printf("Error binding to port %d\n", l.Port)
			return
		}

	case ListenUnix:
		listener, err = net.Listen("unix", l.Path)
		if err != nil {
			log.Printf("Error binding to unix socket at %s\n", l.Path)
			return
		}

	default:
		panic(fmt.Sprintf("Unsupported server listen type: %s", l.Type))
	}

	for {
		remote, err := listener.Accept()
		if err != nil {
			log.Println("Error accepting connection from remote:", err.Error())
			remote.Close()
			continue
		}
		metrics.IncCounter(MetricConnectionsEstablishedExt)

		if l.Type == ListenTCP {
			tcpRemote := remote.(*net.TCPConn)
			tcpRemote.SetKeepAlive(true)
			tcpRemote.SetKeepAlivePeriod(30 * time.Second)
		}

		// construct L1 handler using given constructor
		l1, err := h1()
		if err != nil {
			log.Println("Error opening connection to L1:", err.Error())
			remote.Close()
			continue
		}
		metrics.IncCounter(MetricConnectionsEstablishedL1)

		// construct l2
		l2, err := h2()
		if err != nil {
			log.Println("Error opening connection to L2:", err.Error())
			l1.Close()
			remote.Close()
			continue
		}
		metrics.IncCounter(MetricConnectionsEstablishedL2)

		// spin off a goroutine here to handle determining the protocol used for the connection.
		// The server loop can't be started until the protocol is known. Another goroutine is
		// necessary here because we don't want to block accepting new connections if the current
		// new connection doesn't send data immediately.
		go func(remoteConn net.Conn) {
			remoteReader := bufio.NewReader(remoteConn)
			remoteWriter := bufio.NewWriter(remoteConn)

			var reqParser common.RequestParser
			var responder common.Responder

			// A connection is either binary protocol or text. It cannot switch between the two.
			// This is the way memcached handles protocols, so it can be as strict here.
			binary, err := isBinaryRequest(remoteReader)
			if err != nil {
				// must be an IO error. Abort!
				abort([]io.Closer{remoteConn, l1, l2}, err)
				return
			}

			if binary {
				reqParser = binprot.NewBinaryParser(remoteReader)
				responder = binprot.NewBinaryResponder(remoteWriter)
			} else {
				reqParser = textprot.NewTextParser(remoteReader)
				responder = textprot.NewTextResponder(remoteWriter)
			}

			server := s([]io.Closer{remoteConn, l1, l2}, reqParser, o(l1, l2, responder))

			go server.Loop()
		}(remote)
	}
}
