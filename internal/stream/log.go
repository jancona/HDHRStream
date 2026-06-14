package stream

import (
	"bytes"
	"io"
	"log"
)

// logWriter forwards ffmpeg's stderr to the standard logger, line by line,
// prefixed with the channel id.
type logWriter struct {
	prefix string
	buf    bytes.Buffer
}

func newLogWriter(id string) io.Writer {
	return &logWriter{prefix: "[ffmpeg " + id + "] "}
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// No complete line yet; put the partial back.
			w.buf.Reset()
			w.buf.WriteString(line)
			break
		}
		log.Print(w.prefix, line[:len(line)-1])
	}
	return len(p), nil
}
