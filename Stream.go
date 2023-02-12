package packet

import (
	"encoding/binary"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// Stream represents a writable and readable network stream.
type Stream struct {
	Incoming    <-chan *Packet
	Outgoing    chan<- *Packet
	in          chan *Packet
	out         chan *Packet
	connection  atomic.Value
	closeWriter chan struct{}
	onError     func(IOError)
	isClosing   bool
}

// NewStream creates a new stream with the given channel buffer size.
func NewStream(channelBufferSize int) *Stream {
	stream := &Stream{
		in:          make(chan *Packet, channelBufferSize),
		out:         make(chan *Packet, channelBufferSize),
		closeWriter: make(chan struct{}),
		onError:     func(IOError) {},
		isClosing:   false,
	}

	// The public fields point to the same channels,
	// but can only be used for receiving or sending,
	// respectively.
	stream.Incoming = stream.in
	stream.Outgoing = stream.out

	return stream
}

// Connection returns the internal TCP/UDP connection object.
func (stream *Stream) Connection() net.Conn {
	return stream.connection.Load().(net.Conn)
}

// SetConnection sets the connection that the stream uses and
// it can be called multiple times on a single stream,
// effectively allowing you to hot-swap connections in failure cases.
func (stream *Stream) SetConnection(connection net.Conn) {
	if connection == nil {
		panic(errors.New("SetConnection using nil connection"))
	}

	stream.connection.Store(connection)

	go stream.read(connection)
	go stream.write(connection)
}

// OnError sets the callback that should be called when IO errors occur.
func (stream *Stream) OnError(callback func(IOError)) {
	if callback == nil {
		panic(errors.New("OnError using nil callback"))
	}

	stream.onError = callback
}

// Close frees up the resources used by the stream and closes the connection.
func (stream *Stream) Close() {
	stream.isClosing = true
	stream.Connection().SetReadDeadline(time.Now()) // stop the read goroutine, which will stop the write goroutine and close everything
	// This makes sure that packets sent right before calling Close are actually send and not discarted
}

// This gets called after write rouine has closed and consequently after read has also been closed
func (stream *Stream) closeConn() {
	stream.Connection().Close()
	close(stream.in)
	stream.isClosing = false
}

// Proxy to stop i/o timeouts caused by closing stream
func (stream *Stream) sendError(i IOError) {
	if stream.isClosing && errors.Is(i.Error, os.ErrDeadlineExceeded) {
		return
	}
	stream.onError(i)
}

// read starts a blocking routine that will read incoming messages.
// This function is meant to be called as a concurrent goroutine.
func (stream *Stream) read(connection net.Conn) {
	defer func() {
		stream.closeWriter <- struct{}{}
	}()

	var length int64
	typeBuffer := make([]byte, 1)

	for {
		_, err := connection.Read(typeBuffer)

		if err != nil {
			stream.sendError(IOError{connection, err})
			return
		}

		err = binary.Read(connection, binary.BigEndian, &length)

		if err != nil {
			stream.sendError(IOError{connection, err})
			return
		}

		data := make([]byte, length)
		readLength := 0
		n := 0

		for readLength < len(data) {
			n, err = connection.Read(data[readLength:])
			readLength += n

			if err != nil {
				stream.sendError(IOError{connection, err})
				return
			}
		}

		stream.in <- New(typeBuffer[0], data)
	}
}

// write starts a blocking routine that will write outgoing messages.
// This function is meant to be called as a concurrent goroutine.
func (stream *Stream) write(connection net.Conn) {
	for {
		select {
		case <-stream.closeWriter:
			stream.closeConn()
			return

		case packet := <-stream.out:
			err := packet.Write(connection)

			if err != nil {
				stream.sendError(IOError{connection, err})
				return
			}
		}
	}
}
