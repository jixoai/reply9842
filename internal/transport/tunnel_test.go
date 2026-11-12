package transport

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 生成临时自签证书（测试用）
func testCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "c.pem")
	keyFile = filepath.Join(dir, "k.pem")
	// 用既有 testdata；若不存在则跳过
	t.Logf("cert dir: %s", dir)
	return "", ""
}

// TestTCPRoundTrip 只测 TCP 隧道的帧往返，隔离上层协议问题
func TestTCPRoundTrip(t *testing.T) {
	certFile := os.Getenv("TEST_CERT")
	keyFile := os.Getenv("TEST_KEY")
	if certFile == "" || keyFile == "" {
		t.Skip("需要 TEST_CERT / TEST_KEY 环境变量")
	}

	addr := "127.0.0.1:19100"
	srv := &Server{Addr: addr, CertFile: certFile, KeyFile: keyFile}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	accepts, err := srv.Listen(ctx)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	// 服务端回显
	go func() {
		for a := range accepts {
			go func(a Accept) {
				defer a.Conn.Close()
				b, err := a.Conn.Recv()
				if err != nil {
					t.Logf("server recv err: %v", err)
					return
				}
				t.Logf("server got %d bytes", len(b))
				// 回显：套一层 transport 帧
				if err := WriteFrame(a.Conn, b); err != nil {
					t.Logf("server send err: %v", err)
				}
			}(a)
		}
	}()

	time.Sleep(200 * time.Millisecond)

	d := &Dialer{Addr: addr, ServerName: "localhost", Insecure: true, Mode: KindTCP}
	conn, err := d.Dial(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	payload := []byte("hello tunnel")
	if err := WriteFrame(conn, payload); err != nil {
		t.Fatalf("send: %v", err)
	}
	got, err := conn.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: %q vs %q", got, payload)
	}
}

// TestQUICRoundTrip 同上但走 QUIC。另跑一轮大报文：小报文可能被 QUIC 合成
// 单个 STREAM 帧而掩盖 EOF 处理缺陷，长报文才能稳定复现「数据与 FIN 同帧」。
func TestQUICRoundTrip(t *testing.T) {
	certFile := os.Getenv("TEST_CERT")
	keyFile := os.Getenv("TEST_KEY")
	if certFile == "" || keyFile == "" {
		t.Skip("需要 TEST_CERT / TEST_KEY 环境变量")
	}

	addr := "127.0.0.1:19101"
	srv := &Server{Addr: addr, CertFile: certFile, KeyFile: keyFile}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	accepts, err := srv.Listen(ctx)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for a := range accepts {
			go func(a Accept) {
				defer a.Conn.Close()
				b, err := a.Conn.Recv()
				if err != nil {
					t.Logf("server recv err: %v", err)
					return
				}
				WriteFrame(a.Conn, b)
			}(a)
		}
	}()

	time.Sleep(300 * time.Millisecond)

	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{"short", []byte("hello quic")},
		// 8 KiB 必然跨多个 STREAM 帧，覆盖「最后一次 Read 返回 (n>0, io.EOF)」
		{"multi-frame", bytes.Repeat([]byte("0123456789abcdef"), 512)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Dialer{Addr: addr, ServerName: "localhost", Insecure: true, Mode: KindQUIC}
			conn, err := d.Dial(ctx)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()

			if err := WriteFrame(conn, tc.payload); err != nil {
				t.Fatalf("send: %v", err)
			}
			got, err := conn.Recv()
			if err != nil {
				t.Fatalf("recv: %v", err)
			}
			if !bytes.Equal(got, tc.payload) {
				t.Fatalf("echo mismatch: got %d bytes, want %d bytes", len(got), len(tc.payload))
			}
		})
	}
}

// eofWithDataReader 复现 io.Reader 的合法行为：最后一次 Read 同时返回数据与
// io.EOF（quic-go 的 ReceiveStream 在对端写完即 FIN 时正是如此）。
type eofWithDataReader struct {
	data []byte
	pos  int
	// chunk 控制每次最多返回多少字节；返回最后一块时附带 io.EOF
	chunk int
}

func (r *eofWithDataReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := len(r.data) - r.pos
	if r.chunk > 0 && n > r.chunk {
		n = r.chunk
	}
	if len(p) < n {
		n = len(p)
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	if r.pos >= len(r.data) {
		return n, io.EOF // 数据 + EOF 同一次返回
	}
	return n, nil
}

// TestReadFrameEOFWithData 钉住 readFrame 的语义：Read 返回 (n>0, io.EOF) 时
// 这 n 字节必须保留。旧实现手写 readFull 遇 err 立即返回，把这批数据丢掉，
// 下游只看到 EOF —— 这就是 QUIC 回环测试 recv: EOF 的根因。
func TestReadFrameEOFWithData(t *testing.T) {
	payload := []byte("hello quic")
	frame := make([]byte, 4+len(payload))
	n := len(payload)
	frame[0], frame[1], frame[2], frame[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	copy(frame[4:], payload)

	for _, chunk := range []int{0, 1, 3, 4, 5, 7} {
		r := &eofWithDataReader{data: frame, chunk: chunk}
		got, err := readFrame(r)
		if err != nil {
			t.Fatalf("chunk=%d: readFrame: %v", chunk, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("chunk=%d: got %q, want %q", chunk, got, payload)
		}
	}
}

// shortWriter 模拟允许部分写入的 io.Writer
type shortWriter struct {
	buf      bytes.Buffer
	maxChunk int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if w.maxChunk > 0 && len(p) > w.maxChunk {
		p = p[:w.maxChunk]
	}
	return w.buf.Write(p)
}

// TestWriteAllPartialWrites 钉住 writeAll：Writer 返回 (n<len, nil) 时不能当成功。
func TestWriteAllPartialWrites(t *testing.T) {
	payload := []byte("hello quic partial write")
	for _, chunk := range []int{0, 1, 4, 9} {
		w := &shortWriter{maxChunk: chunk}
		if err := writeAll(w, payload); err != nil {
			t.Fatalf("chunk=%d: writeAll: %v", chunk, err)
		}
		if !bytes.Equal(w.buf.Bytes(), payload) {
			t.Fatalf("chunk=%d: got %q, want %q", chunk, w.buf.Bytes(), payload)
		}
	}
}
