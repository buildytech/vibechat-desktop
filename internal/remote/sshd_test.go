package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testSSHD is an in-process SSH server like the tunnel user's sshd: public-key auth only, no shell, and direct-tcpip
// only to the permitted destinations (permitopen), each mapped to a real local address.
type testSSHD struct {
	t       *testing.T
	ln      net.Listener
	hostKey ssh.Signer
	mu      sync.Mutex
	allowed map[string]bool   // authorized public keys (Marshal) by string
	permit  map[string]string // "127.0.0.1:18120" → local address
	conns   []net.Conn
}

func newHostKey(t *testing.T) ssh.Signer {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func startSSHD(t *testing.T, addr string, hostKey ssh.Signer, permit map[string]string) *testSSHD {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	d := &testSSHD{t: t, ln: ln, hostKey: hostKey, allowed: map[string]bool{}, permit: permit}
	go d.serve()
	t.Cleanup(d.close)
	return d
}

func (d *testSSHD) addr() string { return d.ln.Addr().String() }

func (d *testSSHD) authorize(pub ssh.PublicKey) {
	d.mu.Lock()
	d.allowed[string(pub.Marshal())] = true
	d.mu.Unlock()
}

// dropAll cuts every client connection (a network blip).
func (d *testSSHD) dropAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.conns {
		_ = c.Close()
	}
	d.conns = nil
}

func (d *testSSHD) close() {
	_ = d.ln.Close()
	d.dropAll()
}

func (d *testSSHD) serve() {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			d.mu.Lock()
			ok := d.allowed[string(key.Marshal())]
			d.mu.Unlock()
			if !ok {
				return nil, io.ErrUnexpectedEOF
			}
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(d.hostKey)
	for {
		c, err := d.ln.Accept()
		if err != nil {
			return
		}
		d.mu.Lock()
		d.conns = append(d.conns, c)
		d.mu.Unlock()
		go d.handle(c, cfg)
	}
}

func (d *testSSHD) handle(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		_ = c.Close()
		return
	}
	defer sc.Close()
	go func() {
		for r := range reqs {
			if r.WantReply {
				_ = r.Reply(r.Type == "keepalive@openssh.com", nil)
			}
		}
	}()
	for nc := range chans {
		if nc.ChannelType() != "direct-tcpip" {
			_ = nc.Reject(ssh.Prohibited, "port forwarding only")
			continue
		}
		var p struct {
			Host     string
			Port     uint32
			OrigHost string
			OrigPort uint32
		}
		if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, "bad request")
			continue
		}
		dest := net.JoinHostPort(p.Host, itoa(p.Port))
		local, ok := d.permit[dest]
		if !ok {
			_ = nc.Reject(ssh.Prohibited, "open failed: administratively prohibited")
			continue
		}
		up, err := net.Dial("tcp", local)
		if err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			_ = up.Close()
			continue
		}
		go ssh.DiscardRequests(creqs)
		go func() {
			defer ch.Close()
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(up, ch); done <- struct{}{} }()
			go func() { _, _ = io.Copy(ch, up); done <- struct{}{} }()
			<-done
		}()
	}
}

func itoa(n uint32) string { return strconv.FormatUint(uint64(n), 10) }
