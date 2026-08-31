package codex

import "io"

const maxAppServerMessage = 16 << 20

type messageTransport interface {
	ReadMessage() ([]byte, error)
	WriteMessage([]byte) error
	Close() error
	Kind() string
}

var errTransportClosed = io.ErrClosedPipe
