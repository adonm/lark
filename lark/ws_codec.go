package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	wsText   byte = 1
	wsBinary byte = 2
	wsClose  byte = 8
	wsPing   byte = 9
	wsPong   byte = 10
	wsLimit       = 64 << 10
)

type wsFrame struct {
	op   byte
	data []byte
}

type websocket struct {
	conn   net.Conn
	in     *bufio.Reader
	mu     sync.Mutex
	onPong func([]byte)
}

func hasHeaderToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*websocket, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	nonce, err := base64.StdEncoding.Strict().DecodeString(key)
	if r.ProtoMajor != 1 || r.ProtoMinor < 1 || r.ContentLength != 0 || len(r.TransferEncoding) != 0 ||
		!hasHeaderToken(r.Header, "Connection", "upgrade") || !hasHeaderToken(r.Header, "Upgrade", "websocket") ||
		len(r.Header.Values("Sec-WebSocket-Key")) != 1 || err != nil || len(nonce) != 16 {
		return nil, problem(400, "Expected a WebSocket upgrade")
	}
	if len(r.Header.Values("Sec-WebSocket-Version")) != 1 || r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		return nil, problem(426, "WebSocket version 13 required")
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, problem(500, "Cannot upgrade connection")
	}
	accept := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	_ = conn.SetDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		base64.StdEncoding.EncodeToString(accept[:]))
	if err != nil || rw.Flush() != nil {
		conn.Close()
		return nil, nil
	}
	return &websocket{conn: conn, in: rw.Reader}, nil
}

func (s *websocket) write(f wsFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	var h [10]byte
	h[0] = 0x80 | f.op
	n := 2
	switch size := len(f.data); {
	case size < 126:
		h[1] = byte(size)
	case size <= 65535:
		h[1], n = 126, 4
		binary.BigEndian.PutUint16(h[2:], uint16(size))
	default:
		h[1], n = 127, 10
		binary.BigEndian.PutUint64(h[2:], uint64(size))
	}
	bufs := net.Buffers{h[:n], f.data}
	_, err := bufs.WriteTo(s.conn)
	return err
}

func wsCloseFrame(code uint16) wsFrame {
	return wsFrame{wsClose, []byte{byte(code >> 8), byte(code)}}
}

func (s *websocket) fail(code uint16) (wsFrame, error) {
	_ = s.write(wsCloseFrame(code))
	return wsFrame{}, errors.New("invalid WebSocket frame")
}

func validClose(p []byte) bool {
	if len(p) == 0 {
		return true
	}
	if len(p) < 2 {
		return false
	}
	code := binary.BigEndian.Uint16(p)
	return (code >= 3000 && code <= 4999 || code >= 1000 && code <= 1014 && code != 1004 && code != 1005 && code != 1006) && utf8.Valid(p[2:])
}

// read returns one message, handling control frames between fragments.
func (s *websocket) read() (wsFrame, error) {
	var msg wsFrame
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		var h [8]byte
		if _, err := io.ReadFull(s.in, h[:2]); err != nil {
			return wsFrame{}, err
		}
		final, op, size := h[0]&128 != 0, h[0]&15, uint64(h[1]&127)
		control := op >= 8
		if h[0]&0x70 != 0 || h[1]&128 == 0 || control && (!final || size > 125) {
			return s.fail(1002)
		}
		switch size {
		case 126:
			if _, err := io.ReadFull(s.in, h[:2]); err != nil {
				return wsFrame{}, err
			}
			size = uint64(binary.BigEndian.Uint16(h[:2]))
			if size < 126 {
				return s.fail(1002)
			}
		case 127:
			if _, err := io.ReadFull(s.in, h[:]); err != nil {
				return wsFrame{}, err
			}
			size = binary.BigEndian.Uint64(h[:])
			if size <= 65535 || size>>63 != 0 {
				return s.fail(1002)
			}
		}
		if control {
			if op != wsPing && op != wsPong && op != wsClose {
				return s.fail(1002)
			}
		} else {
			switch {
			case op == 0 && msg.op != 0:
			case (op == wsText || op == wsBinary) && msg.op == 0:
				msg.op = op
			default:
				return s.fail(1002)
			}
			limit := wsLimit
			if msg.op == wsText {
				limit = maxJSON
			}
			if size > uint64(limit-len(msg.data)) {
				return s.fail(1009)
			}
		}
		if _, err := io.ReadFull(s.in, h[:4]); err != nil {
			return wsFrame{}, err
		}
		var p []byte
		if control {
			p = make([]byte, int(size))
		} else {
			n := len(msg.data)
			msg.data = append(msg.data, make([]byte, int(size))...)
			p = msg.data[n:]
		}
		if _, err := io.ReadFull(s.in, p); err != nil {
			return wsFrame{}, err
		}
		for i := range p {
			p[i] ^= h[i&3]
		}
		switch op {
		case wsPing:
			if err := s.write(wsFrame{wsPong, p}); err != nil {
				return wsFrame{}, err
			}
		case wsClose:
			if !validClose(p) {
				if len(p) >= 2 && !utf8.Valid(p[2:]) {
					return s.fail(1007)
				}
				return s.fail(1002)
			}
			_ = s.write(wsFrame{wsClose, p})
			return wsFrame{}, io.EOF
		case wsPong:
			if s.onPong != nil {
				s.onPong(p)
			}
		default:
			if final {
				if msg.op == wsText && !utf8.Valid(msg.data) {
					return s.fail(1007)
				}
				return msg, nil
			}
		}
	}
}
