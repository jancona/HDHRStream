package stream

import (
	"bytes"
	"io"
	"log"
)

// logWriter forwards ffmpeg's stderr to the standard logger, line by line,
// prefixed with the channel id, and hands each line to an optional inspector
// (used to spot why a stream failed to start).
type logWriter struct {
	prefix  string
	inspect func(line string)
	buf     bytes.Buffer
}

func newLogWriter(id string, inspect func(line string)) io.Writer {
	return &logWriter{prefix: "[ffmpeg " + id + "] ", inspect: inspect}
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
		line = line[:len(line)-1]
		log.Print(w.prefix, line)
		if w.inspect != nil {
			w.inspect(line)
		}
	}
	return len(p), nil
}
