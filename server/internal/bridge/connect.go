package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client speaks Connect JSON (unary and server streaming) to one bridge.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// Error is a Connect error answer.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

func (e *Error) Error() string { return "bridge: " + e.Code + ": " + e.Message }

func (c *Client) url(service, method string) string {
	return strings.TrimRight(c.BaseURL, "/") + "/sdk.v1." + service + "/" + method
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Unary calls service/method with req and decodes the answer into resp (nil: discard).
func (c *Client) Unary(ctx context.Context, service, method string, req, resp any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(service, method), bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("content-type", "application/json")
	r.Header.Set("connect-protocol-version", "1")
	r.Header.Set("authorization", "Bearer "+c.Token)
	res, err := c.http().Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		e := &Error{Status: res.StatusCode}
		if json.Unmarshal(raw, e) != nil || e.Code == "" {
			e.Code, e.Message = "http_"+fmt.Sprint(res.StatusCode), strings.TrimSpace(string(raw))
		}
		return e
	}
	if resp == nil {
		return nil
	}
	return json.Unmarshal(raw, resp)
}

// Stream calls a server-streaming method and hands each message to fn until the stream ends, fn errors or ctx ends.
func (c *Client) Stream(ctx context.Context, service, method string, req any, fn func(json.RawMessage) error) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	var env bytes.Buffer
	env.WriteByte(0)
	_ = binary.Write(&env, binary.BigEndian, uint32(len(body)))
	env.Write(body)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(service, method), &env)
	if err != nil {
		return err
	}
	r.Header.Set("content-type", "application/connect+json")
	r.Header.Set("connect-protocol-version", "1")
	r.Header.Set("authorization", "Bearer "+c.Token)
	res, err := c.http().Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		e := &Error{Status: res.StatusCode}
		if json.Unmarshal(raw, e) != nil || e.Code == "" {
			e.Code, e.Message = "http_"+fmt.Sprint(res.StatusCode), strings.TrimSpace(string(raw))
		}
		return e
	}
	br := bufio.NewReaderSize(res.Body, 64<<10)
	for {
		var head [5]byte
		if _, err := io.ReadFull(br, head[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		n := binary.BigEndian.Uint32(head[1:])
		if n > 64<<20 {
			return fmt.Errorf("bridge: frame of %d bytes", n)
		}
		msg := make([]byte, n)
		if _, err := io.ReadFull(br, msg); err != nil {
			return err
		}
		if head[0]&0x02 != 0 { // end of stream: { error?, metadata? }
			var end struct {
				Error *Error `json:"error"`
			}
			_ = json.Unmarshal(msg, &end)
			if end.Error != nil {
				return end.Error
			}
			return nil
		}
		if err := fn(msg); err != nil {
			return err
		}
	}
}
