package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The Pi RPC protocol is JSONL over the child's stdin and stdout: one JSON object per record, split on LF only (not on
// U+2028/U+2029, which are valid inside JSON strings, so no bufio.ScanLines-on-runes and no Node readline here).
// Stdout is read continuously: Pi stalls on a full pipe.

const maxRecord = 16 << 20

// Record is one stdout record: a command response or a session event.
type Record struct {
	Type string
	Raw  json.RawMessage
}

// readRecords calls fn for every record of r until EOF; fn returning false stops reading.
func readRecords(r io.Reader, fn func(Record) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxRecord {
			return fmt.Errorf("pi record over %d bytes", maxRecord)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(line) > 0 && (err == nil || errors.Is(err, io.EOF)) {
			rec := bytes.TrimRight(line, "\r\n")
			line = line[:0]
			if len(bytes.TrimSpace(rec)) > 0 {
				var head struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(rec, &head) == nil && head.Type != "" {
					if !fn(Record{Type: head.Type, Raw: append(json.RawMessage(nil), rec...)}) {
						return nil
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// writer serializes commands to stdin (one JSON object and an LF each).
type cmdWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (c *cmdWriter) send(cmd map[string]any) error {
	b, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}
