package mailer

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSMTPRenderEncodesHeadersAndRejectsInjection(t *testing.T) {
	saved := Frozen{From: "Dev <dev@example.test>", To: []string{"admin@example.test"}, Message: Message{Subject: "验证码 123456", Text: "plain", HTML: "<p>html</p>"}}
	raw, e := renderRFC822(saved)
	if e != nil {
		t.Fatal(e)
	}
	head := strings.SplitN(string(raw), "\r\n\r\n", 2)[0]
	if strings.Contains(head, "验证码") || !strings.Contains(head, "utf-8") || !strings.Contains(head, "multipart/alternative") {
		t.Fatal("subject not encoded or multipart missing", head)
	}
	if _, e = renderRFC822(Frozen{From: "Dev <dev@example.test>", To: []string{"a@example.test"}, Message: Message{Subject: "ok\r\nBcc: attacker@example.test"}}); e == nil {
		t.Fatal("header injection accepted")
	}
	if _, e = renderRFC822(Frozen{From: "Dev <dev@example.test>", To: []string{"a@example.test\r\nBcc: x@y.z"}, Message: Message{Subject: "ok"}}); e == nil {
		t.Fatal("recipient injection accepted")
	}
}

func TestSMTPSendFrozenValidatesEnvelope(t *testing.T) {
	s, e := NewSMTP("127.0.0.1", 25, "", "", "Dev <dev@example.test>")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.SendFrozen(context.Background(), `{"from":"Other <other@example.test>","to":["a@example.test"],"subject":"x"}`, "k"); e == nil {
		t.Fatal("foreign sender accepted")
	}
	if _, e = s.SendFrozen(context.Background(), `{"from":"Dev <dev@example.test>","to":["bad"],"subject":"x"}`, "k"); e == nil {
		t.Fatal("invalid recipient accepted")
	}
	if _, e = s.SendFrozen(context.Background(), `not-json`, "k"); e == nil {
		t.Fatal("garbage body accepted")
	}
}

// fixtureSMTPServer speaks just enough SMTP for one authenticated message.
func fixtureSMTPServer(t *testing.T, received chan<- string) string {
	t.Helper()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		reader, writer := bufio.NewReader(conn), bufio.NewWriter(conn)
		reply := func(line string) { writer.WriteString(line + "\r\n"); writer.Flush() }
		reply("220 fixture ESMTP")
		var data strings.Builder
		inData := false
		for {
			line, e := reader.ReadString('\n')
			if e != nil {
				return
			}
			if inData {
				data.WriteString(line)
				if line == ".\r\n" {
					inData = false
					received <- data.String()
					reply("250 2.0.0 queued")
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				reply("250-fixture greets you\r\n250 AUTH PLAIN")
			case strings.HasPrefix(line, "AUTH PLAIN"):
				reply("235 2.7.0 authenticated")
			case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
				reply("250 2.1.0 ok")
			case strings.HasPrefix(line, "DATA"):
				inData = true
				reply("354 end with dot")
			case strings.HasPrefix(line, "QUIT"):
				reply("221 2.0.0 bye")
				return
			default:
				reply("250 2.0.0 ok")
			}
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return port
}

func TestSMTPDeliversOverLoopbackRelay(t *testing.T) {
	received := make(chan string, 1)
	port := fixtureSMTPServer(t, received)
	s, e := NewSMTP("127.0.0.1", mustPort(t, port), "fixture-user", "fixture-pass", "Dev <dev@example.test>")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SendCode(context.Background(), "admin", "admin@example.test", "123456", "test-id"); e != nil {
		t.Fatal("loopback delivery failed", e)
	}
	select {
	case body := <-received:
		if !strings.Contains(body, "From: \"GPTibo\" <dev@example.test>") || !strings.Contains(body, "To: admin@example.test\r\n") || !strings.Contains(body, "Subject:") {
			t.Fatal("delivered message malformed", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message never delivered")
	}
	var frozen Frozen
	body, e := s.Freeze("admin@example.test", Message{Subject: "x", Text: "y"})
	if e != nil || json.Unmarshal([]byte(body), &frozen) != nil || frozen.From != "Dev <dev@example.test>" {
		t.Fatal("freeze mismatch", e)
	}
}

func mustPort(t *testing.T, value string) int {
	t.Helper()
	port, e := strconv.Atoi(value)
	if e != nil || port < 1 {
		t.Fatal("bad port", value)
	}
	return port
}
