package stargate

type Conn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	SetReadLimit(limit int64)
	Close() error
}

const (
	TextMessage  = 1
	CloseMessage = 8
)
