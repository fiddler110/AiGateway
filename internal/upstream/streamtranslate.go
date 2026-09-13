package upstream

import (
	"bufio"
	"bytes"
	"fmt"
	"io"

	"github.com/scottymacleod/aigateway/internal/provider"
)

// maxNativeLineBytes caps one native stream line buffered while waiting for
// its terminator. A longer line fails with bufio.ErrTooLong, which handlers
// already report as an oversized response.
const maxNativeLineBytes = 4 * 1024 * 1024

// streamReadChunk is how much is read from the upstream body at a time.
const streamReadChunk = 32 * 1024

// translatedStream applies a provider.StreamTranslator to a native upstream
// stream at the upstream boundary (P0.13), so handlers only ever read
// canonical OpenAI SSE.
//
// The unit is one line: each complete native line, without its "\n" or
// "\r\n", is passed to Feed as soon as it arrives, and every returned line is
// emitted followed by "\n". SSE is line-framed for every provider format
// (OpenAI, Anthropic, and Gemini with alt=sse), so a stateful translator can
// track "event:" lines and blank-line event boundaries itself. The openai
// identity translator therefore reproduces the input bytes, apart from
// "\r\n" becoming "\n", which the handlers' line scanner normalises anyway.
//
// Failure semantics match the handlers' scanner: when the body read fails
// (response limit, stream_timeout cancellation, dropped connection), lines
// completed before the failure are still delivered, the unterminated
// partial line is discarded rather than fed as if complete, Done is not
// called, and the read error is returned unchanged so errors.Is still
// recognises it.
type translatedStream struct {
	src        io.ReadCloser
	translator provider.StreamTranslator
	upstream   string

	partial []byte // native bytes after the last "\n"
	out     []byte // translated bytes not yet returned to the reader
	err     error  // terminal error (io.EOF on clean end), returned once out drains
	readBuf []byte
}

func newTranslatedStream(src io.ReadCloser, t provider.StreamTranslator, upstreamName string) *translatedStream {
	return &translatedStream{src: src, translator: t, upstream: upstreamName}
}

func (s *translatedStream) Read(p []byte) (int, error) {
	for len(s.out) == 0 && s.err == nil {
		s.fill()
	}
	if len(s.out) > 0 {
		n := copy(p, s.out)
		s.out = s.out[n:]
		return n, nil
	}
	return 0, s.err
}

// fill does one read from the upstream body and translates every line it
// completes. It returns after a single read so a line reaches the client as
// soon as its terminator arrives.
func (s *translatedStream) fill() {
	if s.readBuf == nil {
		s.readBuf = make([]byte, streamReadChunk)
	}
	n, rerr := s.src.Read(s.readBuf)
	data := s.readBuf[:n]
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			if len(s.partial)+len(data) > maxNativeLineBytes {
				s.err = bufio.ErrTooLong
				return
			}
			s.partial = append(s.partial, data...)
			break
		}
		line := data[:i]
		if len(s.partial) > 0 {
			line = append(s.partial, line...)
			s.partial = s.partial[:0]
		}
		data = data[i+1:]
		if len(line) > maxNativeLineBytes {
			s.err = bufio.ErrTooLong
			return
		}
		if !s.feed(bytes.TrimSuffix(line, []byte{'\r'})) {
			return
		}
	}

	switch {
	case rerr == nil:
	case rerr == io.EOF:
		// A final unterminated line on a clean EOF is complete.
		if len(s.partial) > 0 {
			line := bytes.TrimSuffix(s.partial, []byte{'\r'})
			s.partial = nil
			if !s.feed(line) {
				return
			}
		}
		lines, _, terr := s.translator.Done()
		if terr != nil {
			s.err = fmt.Errorf("translate stream end from upstream %s: %w", s.upstream, terr)
			return
		}
		s.emit(lines)
		s.err = io.EOF
	default:
		s.partial = nil
		s.err = rerr
	}
}

// feed translates one native line, returning false if translation failed.
func (s *translatedStream) feed(line []byte) bool {
	lines, err := s.translator.Feed(line)
	if err != nil {
		s.err = fmt.Errorf("translate stream from upstream %s: %w", s.upstream, err)
		return false
	}
	s.emit(lines)
	return true
}

func (s *translatedStream) emit(lines [][]byte) {
	for _, l := range lines {
		s.out = append(s.out, l...)
		s.out = append(s.out, '\n')
	}
}

func (s *translatedStream) Close() error { return s.src.Close() }
