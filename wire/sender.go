package wire

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/fancypantalons/goadb/utils/errors"
)

// Sender sends messages to the server.
type Sender interface {
	SendMessage(msg []byte) error

	NewSyncSender() SyncSender

	Close() error
}

type realSender struct {
	writer io.WriteCloser
}

func NewSender(w io.WriteCloser) Sender {
	return &realSender{w}
}

func SendMessageString(s Sender, msg string) error {
	return s.SendMessage([]byte(msg))
}

func (s *realSender) SendMessage(msg []byte) error {
	if len(msg) > MaxMessageLength {
		return errors.AssertionErrorf("message length exceeds maximum: %d", len(msg))
	}

	lengthAndMsg := fmt.Sprintf("%04x%s", len(msg), msg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return writeFullyWithTimeout(ctx, s.writer, []byte(lengthAndMsg), 5*time.Second)
}

func (s *realSender) NewSyncSender() SyncSender {
	return NewSyncSender(s.writer)
}

func (s *realSender) Close() error {
	return errors.WrapErrorf(s.writer.Close(), errors.NetworkError, "error closing sender")
}

var _ Sender = &realSender{}
