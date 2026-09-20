package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/FirePing32/harness-api/internal/logx"
	"github.com/FirePing32/harness-api/internal/oai"
)

// Reading a streamed completion.
//
// The spec says server-sent events; providers disagree in small ways that all
// break a strict parser. Observed in the wild and handled here:
//
//   - "data:" with and without a space after the colon
//   - a single event split across several data: lines, joined with newlines
//   - comment lines (": keepalive") used to hold connections open
//   - "event:" and "id:" fields that carry nothing we need
//   - no terminating "[DONE]" sentinel, so EOF is the only end signal
//   - CRLF line endings
//   - newline-delimited JSON with no SSE framing at all
//
// The parser is deliberately permissive: a provider that streams slightly wrong
// but usefully is far more common than one that streams nothing.

// Format selects the stream framing.
type Format string

const (
	// FormatAuto sniffs the framing from the first meaningful line.
	FormatAuto Format = ""
	// FormatSSE is server-sent events.
	FormatSSE Format = "sse"
	// FormatNDJSON is newline-delimited JSON with no framing.
	FormatNDJSON Format = "ndjson"
)

// maxEventBytes caps a single event. Tool-call arguments legitimately reach
// hundreds of kilobytes, so the limit is generous, but unbounded growth on a
// malformed stream would be an easy way to exhaust memory.
const maxEventBytes = 16 << 20

// ErrEventTooLarge is returned when one event exceeds maxEventBytes.
var ErrEventTooLarge = errors.New("stream event exceeded the size limit")

// Decoder yields the JSON payload of each event in a streamed response.
type Decoder struct {
	br     *bufio.Reader
	format Format

	// data accumulates the data: lines of the event being assembled.
	data []byte
}

// NewDecoder builds a Decoder. Pass FormatAuto unless the provider profile
// specifies otherwise.
func NewDecoder(r io.Reader, format Format) *Decoder {
	return &Decoder{br: bufio.NewReaderSize(r, 64<<10), format: format}
}

// Next returns the next event payload.
//
// It returns io.EOF at the end of the stream, which includes both a "[DONE]"
// sentinel and a plain connection close — several providers never send the
// sentinel, so treating its absence as an error would break them.
func (d *Decoder) Next() ([]byte, error) {
	d.data = d.data[:0]

	for {
		line, err := d.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// A final event with no trailing blank line still counts.
				if payload, ok := d.flush(); ok {
					return payload, nil
				}
			}
			return nil, err
		}

		if d.format == FormatAuto {
			d.format = sniff(line)
			if d.format == FormatAuto {
				continue // nothing conclusive yet; usually a leading blank line
			}
		}

		if d.format == FormatNDJSON {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				continue
			}
			if isDone(trimmed) {
				return nil, io.EOF
			}
			return trimmed, nil
		}

		// SSE.
		if len(bytes.TrimSpace(line)) == 0 {
			// A blank line ends an event. Blank lines between events are common
			// filler and must not produce empty payloads.
			if payload, ok := d.flush(); ok {
				return payload, nil
			}
			continue
		}

		if line[0] == ':' {
			// A comment. Providers and proxies use these as keepalives.
			continue
		}

		field, value := splitField(line)
		switch field {
		case "data":
			if len(d.data) > 0 {
				d.data = append(d.data, '\n')
			}
			d.data = append(d.data, value...)
			if len(d.data) > maxEventBytes {
				return nil, ErrEventTooLarge
			}
		default:
			// "event", "id", "retry" and anything else carry no payload we use.
		}
	}
}

// flush returns the assembled event, or reports that there was nothing to return.
func (d *Decoder) flush() ([]byte, bool) {
	trimmed := bytes.TrimSpace(d.data)
	if len(trimmed) == 0 {
		return nil, false
	}
	if isDone(trimmed) {
		return nil, false
	}
	// Copy: the caller may hold the payload while we reuse the buffer.
	out := make([]byte, len(trimmed))
	copy(out, trimmed)
	d.data = d.data[:0]
	return out, true
}

func isDone(b []byte) bool {
	return bytes.Equal(b, []byte("[DONE]"))
}

// sniff guesses the framing from one line.
func sniff(line []byte) Format {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return FormatAuto
	}
	switch {
	case trimmed[0] == ':':
		return FormatSSE
	case bytes.HasPrefix(trimmed, []byte("data:")),
		bytes.HasPrefix(trimmed, []byte("event:")),
		bytes.HasPrefix(trimmed, []byte("id:")),
		bytes.HasPrefix(trimmed, []byte("retry:")):
		return FormatSSE
	case trimmed[0] == '{', trimmed[0] == '[':
		return FormatNDJSON
	default:
		return FormatAuto
	}
}

// splitField parses one SSE field line into its name and value. Per the spec a
// single space after the colon is part of the framing, not the value; any further
// spaces are data.
func splitField(line []byte) (field, value string) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		// A bare field name with no colon is legal and carries an empty value.
		return string(line), ""
	}
	field = string(line[:i])
	v := line[i+1:]
	if len(v) > 0 && v[0] == ' ' {
		v = v[1:]
	}
	return field, string(v)
}

// readLine reads one line, transparently joining the pieces of a line longer than
// the reader's buffer. The trailing newline and any carriage return are removed.
func (d *Decoder) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := d.br.ReadLine()
		if err != nil {
			if len(buf) > 0 && errors.Is(err, io.EOF) {
				return buf, nil
			}
			return nil, err
		}
		if !isPrefix && buf == nil {
			// Fast path: the whole line was already contiguous in the buffer.
			return chunk, nil
		}
		buf = append(buf, chunk...)
		if len(buf) > maxEventBytes {
			return nil, ErrEventTooLarge
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

// Stream is an in-flight streaming completion.
type Stream struct {
	dec    *Decoder
	closer io.Closer
}

// NewStream wraps an already-open body as a Stream. Separate from Client.Stream
// so recorded transcripts can be replayed through exactly the production path.
func NewStream(rc io.ReadCloser, format Format) *Stream {
	return &Stream{dec: NewDecoder(rc, format), closer: rc}
}

// Stream starts a streaming chat completion. The caller must Close the result.
func (c *Client) Stream(ctx context.Context, req *oai.ChatCompletionRequest, format Format) (*Stream, error) {
	body, err := BuildBody(req, true)
	if err != nil {
		return nil, fmt.Errorf("encode upstream request: %w", err)
	}

	resp, err := c.do(ctx, body)
	if err != nil {
		return nil, err
	}

	return &Stream{
		dec:    NewDecoder(resp.Body, format),
		closer: resp.Body,
	}, nil
}

// Recv returns the next chunk, or io.EOF at the end of the stream.
func (s *Stream) Recv() (*oai.ChatCompletionChunk, error) {
	payload, err := s.dec.Next()
	if err != nil {
		return nil, err
	}

	// Check for an error object first. Every field of a chunk is optional, so an
	// error payload unmarshals into one perfectly happily and yields a silent
	// empty chunk — the stream would just end early with no explanation.
	if streamErr := parseStreamError(payload); streamErr != nil {
		return nil, streamErr
	}

	var chunk oai.ChatCompletionChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil, fmt.Errorf("malformed stream chunk (%d bytes): %w", len(payload), err)
	}
	return &chunk, nil
}

// Close releases the underlying response body.
func (s *Stream) Close() error { return s.closer.Close() }

// parseStreamError recognises an error object delivered inside a 200 stream,
// which is how several providers report a mid-generation failure.
func parseStreamError(payload []byte) error {
	var envelope struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Error == nil {
		return nil
	}
	return &Error{
		Status: 0, // no status: the HTTP response was a 200
		Body:   logx.Redact(strings.TrimSpace(envelope.Error.Message)),
	}
}
