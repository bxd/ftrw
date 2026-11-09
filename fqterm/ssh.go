package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshHandshakeTimeout 罩住 TCP 通了之后的 SSH 版本协商与 KEX。对端是个不该连的端口
// （比如代理把 HTTP 口当成了目标）时对方就是不应答，没这个值会永远挂住。
const sshHandshakeTimeout = 20 * time.Second

// SSHConn drives an SSH2 session (PTY shell) and exposes a uniform byte stream.
type SSHConn struct {
	client *ssh.Client
	sess   *ssh.Session
	stdin  io.WriteCloser
	out    io.Reader

	closed   chan struct{}
	closeOne sync.Once
}

// ConnectSSH establishes an SSH session. termType/cols/rows configure the PTY.
// pc 非空时先把 TCP 经代理打到对端，再在已拨好的连接上做 SSH 握手 —— 代理必须发生
// 在传输层，否则只是把「连代理」当成了「连上了 BBS」。
func ConnectSSH(host string, port int, user string, auth AuthMethod, termType string, cols, rows int, hostKeyCallback ssh.HostKeyCallback, pc ProxyConfig) (*SSHConn, error) {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	cfg := &ssh.ClientConfig{
		User:            user,
		HostKeyCallback: hostKeyCallback,
		ClientVersion:   "SSH-2.0-FQTerm_3.0",
	}
	switch auth.Kind {
	case AuthPassword:
		cfg.Auth = []ssh.AuthMethod{
			ssh.Password(auth.Password),
			ssh.KeyboardInteractive(passwordOnlyInteractive(auth.Password)),
		}
	case AuthPublicKey:
		key, err := os.ReadFile(auth.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("read key: %w", err)
		}
		var signer ssh.Signer
		if auth.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(key, []byte(auth.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(key)
		}
		if err != nil {
			return nil, fmt.Errorf("parse key: %w", err)
		}
		cfg.Auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	default:
		return nil, fmt.Errorf("unsupported auth method")
	}
	if termType == "" {
		termType = "xterm-256color"
	}
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	// 自己拨 TCP 而不是用 ssh.Dial：代理拨号在这里才能接进去。代价是握手也得自己
	// 管 deadline —— 原先 ssh.Dial 连 cfg.Timeout 都没设，对端不回 banner 就能把
	// 一次 RPC 永久挂住。
	dctx, cancel := context.WithTimeout(context.Background(), proxyDialTimeout)
	conn, err := proxyDial(dctx, pc, "tcp", addr)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("连 %s 失败：%w", addr, err)
	}
	conn.SetDeadline(time.Now().Add(sshHandshakeTimeout))
	nConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	conn.SetDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return nil, err
	}
	client := ssh.NewClient(nConn, chans, reqs)
	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, err
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:   1,
		ssh.VERASE: 0x7f,
	}
	if err := sess.RequestPty(termType, rows, cols, modes); err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	if err := sess.Shell(); err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	return &SSHConn{
		client: client,
		sess:   sess,
		stdin:  stdin,
		out:    io.MultiReader(stdout, stderr),
		closed: make(chan struct{}),
	}, nil
}

func (c *SSHConn) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *SSHConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

func (c *SSHConn) resize(cols, rows int) error {
	return c.sess.WindowChange(rows, cols)
}

func (c *SSHConn) Close() error {
	var err error
	c.closeOne.Do(func() {
		close(c.closed)
		c.sess.Close()
		err = c.client.Close()
	})
	return err
}

// AuthKind enumerates SSH authentication strategies.
type AuthKind int

const (
	AuthNone AuthKind = iota
	AuthPassword
	AuthPublicKey
)

// AuthMethod carries SSH credentials.
type AuthMethod struct {
	Kind       AuthKind `json:"kind"`
	Password   string   `json:"password"`
	KeyFile    string   `json:"keyFile"`
	Passphrase string   `json:"passphrase"`
}

// passwordOnlyInteractive answers any keyboard-interactive prompt with the
// password. Many BBS/SSH servers use "ssh-interactive" auth.
func passwordOnlyInteractive(password string) func(name, instruction string, questions []string, echos []bool) ([]string, error) {
	return func(name, instruction string, questions []string, echos []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for i := range questions {
			answers[i] = password
		}
		return answers, nil
	}
}

// KnownHosts implements a minimal TOFU (trust-on-first-use) host key store so
// SSH connections are not blindly trusted on subsequent connects.
type KnownHosts struct {
	mu   sync.Mutex
	path string
}

func NewKnownHosts(dir string) *KnownHosts {
	return &KnownHosts{path: filepath.Join(dir, "known_hosts.txt")}
}

// Callback returns an ssh.HostKeyCallback that records unknown keys and rejects
// changed keys.
func (k *KnownHosts) Callback() ssh.HostKeyCallback {
	return func(addr string, remote net.Addr, key ssh.PublicKey) error {
		fingerprint := ssh.FingerprintSHA256(key)
		k.mu.Lock()
		defer k.mu.Unlock()
		data, err := os.ReadFile(k.path)
		if err == nil {
			for _, line := range bytes.Split(data, []byte("\n")) {
				parts := bytes.Fields(line)
				if len(parts) == 2 && string(parts[0]) == addr {
					if string(parts[1]) == fingerprint {
						return nil
					}
					return fmt.Errorf("host key changed for %s (possible MITM)", addr)
				}
			}
		}
		// Trust on first use: append.
		f, err := os.OpenFile(k.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil // non-fatal
		}
		defer f.Close()
		fmt.Fprintf(f, "%s %s\n", addr, fingerprint)
		return nil
	}
}
