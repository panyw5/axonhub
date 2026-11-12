package commandcode

import (
	"bufio"
	"context"
	"io"
	"sync"
	"sync/atomic"

	"github.com/looplj/axonhub/llm/httpclient"
)

func init() {
	for _, contentType := range []string{
		"application/jsonl",
		"application/jsonl; charset=utf-8",
		"application/x-jsonlines",
	} {
		httpclient.RegisterDecoder(contentType, newJSONLDecoder)
	}
}

func newJSONLDecoder(ctx context.Context, body io.ReadCloser) httpclient.StreamDecoder {
	return &jsonlDecoder{ctx: ctx, reader: bufio.NewReader(body), body: body}
}

type jsonlDecoder struct {
	ctx     context.Context
	reader  *bufio.Reader
	body    io.ReadCloser
	current *httpclient.StreamEvent
	err     error
	closed  atomic.Bool
	once    sync.Once
}

func (d *jsonlDecoder) Next() bool {
	if d.err != nil || d.closed.Load() {
		return false
	}
	select {
	case <-d.ctx.Done():
		d.err = d.ctx.Err()
		return false
	default:
	}

	for {
		line, err := d.reader.ReadBytes('\n')
		line = trimJSONLLine(line)
		if len(line) > 0 {
			d.current = &httpclient.StreamEvent{Data: line}
			if err != nil && err != io.EOF {
				d.err = err
			}
			return true
		}
		if err != nil {
			if err != io.EOF {
				d.err = err
			}
			return false
		}
	}
}

func trimJSONLLine(line []byte) []byte {
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line
}

func (d *jsonlDecoder) Current() *httpclient.StreamEvent { return d.current }
func (d *jsonlDecoder) Err() error                       { return d.err }

func (d *jsonlDecoder) Close() error {
	var err error
	d.once.Do(func() {
		d.closed.Store(true)
		err = d.body.Close()
	})
	return err
}
