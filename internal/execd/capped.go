package execd

import (
	"bytes"
	"io"
)

type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int64
	written   int64
	truncated bool
}

func newCappedBuffer(limit int64) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.written += int64(len(p))
	if b.limit <= 0 {
		if len(p) > 0 {
			b.truncated = true
		}
		return len(p), nil
	}
	remaining := b.limit - int64(b.buf.Len())
	if remaining <= 0 {
		if len(p) > 0 {
			b.truncated = true
		}
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	return b.buf.String()
}

func (b *cappedBuffer) Truncated() bool {
	return b.truncated
}

type cappedTeeWriter struct {
	dst       io.Writer
	limit     int64
	written   int64
	truncated bool
}

func newCappedTeeWriter(dst io.Writer, limit int64) *cappedTeeWriter {
	return &cappedTeeWriter{dst: dst, limit: limit}
}

func (w *cappedTeeWriter) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		if len(p) > 0 {
			w.truncated = true
		}
		return len(p), nil
	}
	remaining := w.limit - w.written
	if remaining <= 0 {
		if len(p) > 0 {
			w.truncated = true
		}
		return len(p), nil
	}
	toWrite := p
	if int64(len(p)) > remaining {
		toWrite = p[:remaining]
		w.truncated = true
	}
	if len(toWrite) > 0 {
		if _, err := w.dst.Write(toWrite); err != nil {
			return 0, err
		}
		w.written += int64(len(toWrite))
	}
	return len(p), nil
}

func (w *cappedTeeWriter) Truncated() bool {
	return w.truncated
}
