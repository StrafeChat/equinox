package stargate

import "time"

type Conn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	SetReadLimit(limit int64)
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
	SetPongHandler(h func(string) error)
	Close() error
}

const (
	TextMessage  = 1
	PingMessage  = 9
	CloseMessage = 8
)
