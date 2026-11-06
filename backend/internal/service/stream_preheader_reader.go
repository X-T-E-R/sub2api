package service

import (
	"bufio"
	"io"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type streamLineReader interface {
	ReadString(byte) (string, error)
}

type streamLineScanner interface {
	Scan() bool
	Text() string
	Err() error
}

type preheaderLineResult struct {
	line string
	err  error
}

type preheaderLineReader struct {
	c          *gin.Context
	policy     streamPreheaderPolicy
	lines      <-chan preheaderLineResult
	clientDone <-chan struct{}
	timer      *time.Timer
	timerCh    <-chan time.Time
	lastSize   int
}

// newStreamPreheaderReader supplies the same single-writer heartbeat while an
// older, synchronous SSE parser waits for a line. Only the reader runs in the
// worker. All protocol conversion, usage observation and client writes stay in
// the existing calling goroutine. A disconnected client stops pings, not usage
// draining; closing the enclosing response releases a blocked reader.
func newStreamPreheaderReader(c *gin.Context, source streamLineReader) (streamLineReader, func()) {
	value, enabled := c.Get(streamPreheaderPolicyKey)
	if !enabled {
		return source, func() {}
	}
	policy := value.(streamPreheaderPolicy)
	lines := make(chan preheaderLineResult)
	done := make(chan struct{})
	go func() {
		defer close(lines)
		for {
			line, err := source.ReadString('\n')
			select {
			case lines <- preheaderLineResult{line, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	timer := time.NewTimer(policy.interval)
	return &preheaderLineReader{
		c: c, policy: policy, lines: lines, clientDone: c.Request.Context().Done(),
		timer: timer, timerCh: timer.C, lastSize: c.Writer.Size(),
	}, func() { timer.Stop(); close(done) }
}

func (r *preheaderLineReader) ReadString(_ byte) (string, error) {
	if size := r.c.Writer.Size(); r.timerCh != nil && size != r.lastSize {
		r.lastSize = size
		r.timer.Reset(r.policy.interval)
	}
	for {
		select {
		case line, ok := <-r.lines:
			if !ok {
				return "", io.EOF
			}
			return line.line, line.err
		case <-r.clientDone:
			r.clientDone = nil
			r.timer.Stop()
			r.timerCh = nil
		case <-r.timerCh:
			if r.c.Request.Context().Err() != nil {
				r.timerCh = nil
				continue
			}
			committing := !r.c.Writer.Written()
			ping := ": keepalive\n"
			if committing {
				r.c.Header("Content-Type", "text/event-stream")
				r.c.Header("Cache-Control", "no-cache")
				r.c.Header("X-Accel-Buffering", "no")
				r.c.Status(200)
				r.c.Set(streamPreheaderCommittedKey, r.policy.protocol)
				ping = streamPreheaderPing(r.policy.protocol)
			}
			// A comment line is legal even between a partially received SSE event's
			// fields. Do not add a blank line that could dispatch that event early.
			if _, err := r.c.Writer.WriteString(ping); err != nil {
				r.timerCh = nil
				continue
			}
			r.c.Writer.Flush()
			r.lastSize = r.c.Writer.Size()
			r.timer.Reset(r.policy.interval)
		}
	}
}

type preheaderScannerSource struct{ scanner *bufio.Scanner }

func (r preheaderScannerSource) ReadString(byte) (string, error) {
	if r.scanner.Scan() {
		return r.scanner.Text() + "\n", nil
	}
	if err := r.scanner.Err(); err != nil {
		return "", err
	}
	return "", io.EOF
}

type preheaderScanner struct {
	reader streamLineReader
	line   string
	err    error
}

func (s *preheaderScanner) Scan() bool {
	s.line, s.err = s.reader.ReadString('\n')
	s.line = strings.TrimSuffix(s.line, "\n")
	return s.err == nil
}
func (s *preheaderScanner) Text() string { return s.line }
func (s *preheaderScanner) Err() error {
	if s.err == io.EOF {
		return nil
	}
	return s.err
}
func newStreamPreheaderScanner(c *gin.Context, source *bufio.Scanner) (streamLineScanner, func()) {
	if !StreamPreheaderConfigured(c) {
		return source, func() {}
	}
	reader, stop := newStreamPreheaderReader(c, preheaderScannerSource{source})
	return &preheaderScanner{reader: reader}, stop
}
