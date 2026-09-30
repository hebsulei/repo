package session

import "io"

type closedWriteCloser struct{}

func (closedWriteCloser) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (closedWriteCloser) Close() error              { return nil }
