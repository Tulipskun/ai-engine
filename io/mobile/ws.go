// Package mobile serves the WebSocket the AIxodia app talks to: one socket per
// phone, frames are JSON, auth is the bearer token in the handshake only.
// Stdlib only (RFC 6455 framing is implemented here).
package mobile

import (
	"bufio"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
const maxFrame = 1 << 20

// ChatFunc answers one user message in a session and returns the reply text
// plus token counts.
type ChatFunc func(sessionID, text string) (reply string, inTokens, outTokens int, err error)

type inFrame struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	ClientMsgID string `json:"client_msg_id"`
	JobID       string `json:"job_id"`
}

type outFrame struct {
	Source       string `json:"source"`
	SessionID    string `json:"session_id"`
	Kind         string `json:"kind"`
	Text         string `json:"text,omitempty"`
	Role         string `json:"role,omitempty"`
	Agent        string `json:"agent,omitempty"`
	ClientMsgID  string `json:"client_msg_id,omitempty"`
	Seq          int64  `json:"seq,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
}

// Handler upgrades /ws to a WebSocket after checking the bearer token.
func Handler(token string, chat ChatFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if token == "" || subtle.ConstantTimeCompare([]byte(auth), []byte(token)) != 1 {
			http.Error(w, "token rejected", http.StatusUnauthorized)
			return
		}
		key := r.Header.Get("Sec-WebSocket-Key")
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
			!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") ||
			r.Header.Get("Sec-WebSocket-Version") != "13" || key == "" {
			http.Error(w, "websocket upgrade required", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		sum := sha1.Sum([]byte(key + wsGUID))
		accept := base64.StdEncoding.EncodeToString(sum[:])
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		if err := rw.Flush(); err != nil {
			return
		}
		serve(conn, rw.Reader, chat)
	})
}

type peer struct {
	conn net.Conn
	mu   sync.Mutex
	seq  int64
}

func (p *peer) send(f outFrame) error {
	f.Source = "ai"
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return writeFrame(p.conn, 0x1, data)
}

func serve(conn net.Conn, r *bufio.Reader, chat ChatFunc) {
	p := &peer{conn: conn}
	for {
		op, payload, err := readFrame(r)
		if err != nil {
			return
		}
		switch op {
		case 0x1: // text
			var in inFrame
			if err := json.Unmarshal(payload, &in); err != nil {
				_ = p.send(outFrame{Kind: "error", Text: "frame ที่อ่านไม่ได้"})
				continue
			}
			handle(p, in, chat)
		case 0x8: // close
			p.mu.Lock()
			_ = writeFrame(conn, 0x8, []byte{})
			p.mu.Unlock()
			return
		case 0x9: // ping
			p.mu.Lock()
			_ = writeFrame(conn, 0xA, payload)
			p.mu.Unlock()
		}
	}
}

func handle(p *peer, in inFrame, chat ChatFunc) {
	switch in.Type {
	case "hello":
		_ = p.send(outFrame{Kind: "ack", SessionID: in.SessionID})
	case "cancel":
		_ = p.send(outFrame{Kind: "done", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID})
	case "message", "":
		var parts []string
		for _, c := range in.Content {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		text := strings.Join(parts, "\n")
		_ = p.send(outFrame{Kind: "ack", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID})
		if strings.TrimSpace(text) == "" {
			_ = p.send(outFrame{Kind: "error", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID, Text: "ข้อความว่าง"})
			return
		}
		reply, inTok, outTok, err := chat(in.SessionID, text)
		if err != nil {
			log.Printf("mobile: chat: %v", err)
			_ = p.send(outFrame{Kind: "error", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID, Text: err.Error()})
			return
		}
		p.mu.Lock()
		p.seq++
		seq := p.seq
		p.mu.Unlock()
		_ = p.send(outFrame{Kind: "message", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID,
			Text: reply, Role: "model", Agent: "main", Seq: seq, InputTokens: inTok, OutputTokens: outTok})
		_ = p.send(outFrame{Kind: "done", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID, Seq: seq})
	}
}

func readFrame(r *bufio.Reader) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	op := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > maxFrame {
		return 0, nil, errors.New("frame too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, payload, nil
}

func writeFrame(w io.Writer, op byte, payload []byte) error {
	hdr := []byte{0x80 | op}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, 0, 0)
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
	default:
		hdr = append(hdr, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}
