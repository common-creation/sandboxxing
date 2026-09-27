// Package sshserver exposes the sandboxxing daemon over the SSH protocol.
//
// The user name selects the mode of the session:
//
//   - a control user (admin_users) gets the command interface: ls, new, rm,
//     restart, cp, resize, stat and friends.
//   - any other user name is interpreted as a container name. The session is
//     turned into an interactive login (or a one-shot command) inside that
//     container, which makes `ssh <name>@host` behave like a normal VM.
package sshserver

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/common-creation/sandboxxing/internal/cli"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
	"github.com/common-creation/sandboxxing/internal/vm"
)

// Server is the SSH endpoint of the daemon.
type Server struct {
	cfg      *config.Config
	log      *slog.Logger
	vms      *vm.Manager
	state    *state.State
	runner   *cli.Runner
	sshConf  *ssh.ServerConfig
	password string
	// keys are the public keys that may connect without the password.
	keys []ssh.PublicKey
	// Listener, when set, is used instead of opening cfg.SSHAddr. It allows
	// tests to select an ephemeral port.
	Listener net.Listener
}

// New prepares the server, loading or generating the host key and the shared
// access password.
func New(cfg *config.Config, log *slog.Logger, vms *vm.Manager, st *state.State, runner *cli.Runner) (*Server, error) {
	hostKey, err := loadOrCreateHostKey(cfg, log)
	if err != nil {
		return nil, err
	}
	password, err := loadOrCreatePassword(cfg, log)
	if err != nil {
		return nil, err
	}
	keys, err := loadAuthorizedKeys(cfg, log)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:      cfg,
		log:      log,
		vms:      vms,
		state:    st,
		runner:   runner,
		password: password,
		keys:     keys,
	}
	s.sshConf = &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-sandboxxing",
		PasswordCallback: func(conn ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if subtleCompare(string(pass), s.password) {
				return &ssh.Permissions{Extensions: map[string]string{
					"user":   conn.User(),
					"auth":   "password",
					"pubkey": "",
				}}, nil
			}
			s.log.Warn("authentication failed", "user", conn.User(), "remote", conn.RemoteAddr().String())
			return nil, errors.New("invalid password")
		},
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			for _, allowed := range s.keys {
				if subtleCompare(string(allowed.Marshal()), string(key.Marshal())) {
					return &ssh.Permissions{Extensions: map[string]string{
						"user":   conn.User(),
						"auth":   "publickey",
						"pubkey": ssh.FingerprintSHA256(key),
					}}, nil
				}
			}
			if len(s.keys) == 0 {
				return nil, errors.New("public key authentication is not enabled; add keys to authorized_keys_file or use the password")
			}
			s.log.Warn("public key rejected", "user", conn.User(), "fingerprint", ssh.FingerprintSHA256(key))
			return nil, errors.New("unknown public key")
		},
		BannerCallback: func(conn ssh.ConnMetadata) string {
			if s.cfg.IsAdminUser(conn.User()) {
				return "sandboxxing: control session\n"
			}
			return ""
		},
	}
	s.sshConf.AddHostKey(hostKey)
	return s, nil
}

// Password returns the shared access password.
func (s *Server) Password() string { return s.password }

// Serve accepts connections until the context is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	ln := s.Listener
	if ln == nil {
		var err error
		ln, err = net.Listen("tcp", s.cfg.SSHAddr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", s.cfg.SSHAddr, err)
		}
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	s.log.Info("listening for SSH connections", "address", ln.Addr().String())

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				wg.Wait()
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.handleConn(ctx, conn); err != nil {
				s.log.Debug("connection closed", "remote", conn.RemoteAddr().String(), "error", err)
			}
		}()
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) error {
	defer conn.Close()
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, s.sshConf)
	if err != nil {
		return err
	}
	defer sshConn.Close()
	s.log.Info("session started", "user", sshConn.User(), "remote", sshConn.RemoteAddr().String())

	go ssh.DiscardRequests(reqs)

	var sessions sync.WaitGroup
	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			return err
		}
		// Sessions run concurrently: a channel stays open for the whole
		// login, and further channels (a second session, or an agent
		// forward) must not be blocked behind it.
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			s.handleSession(ctx, sshConn, channel, requests)
		}()
	}
	sessions.Wait()
	return nil
}

func (s *Server) exitStatus(ch ssh.Channel, code int) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
}

func loadOrCreateHostKey(cfg *config.Config, log *slog.Logger) (ssh.Signer, error) {
	path := filepath.Join(cfg.DataDir, "ssh_host_rsa_key")
	if b, err := os.ReadFile(path); err == nil {
		signer, err := ssh.ParsePrivateKey(b)
		if err == nil {
			return signer, nil
		}
		log.Warn("host key is unreadable, generating a new one", "path", path, "error", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(key, "sandboxxing host key")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pemEncode(block), 0o600); err != nil {
		return nil, err
	}
	log.Info("generated SSH host key", "path", path)
	return ssh.NewSignerFromKey(key)
}

// loadAuthorizedKeys reads the optional list of allowed public keys. A missing
// file simply disables key based authentication.
func loadAuthorizedKeys(cfg *config.Config, log *slog.Logger) ([]ssh.PublicKey, error) {
	b, err := os.ReadFile(cfg.AuthorizedKeysFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", cfg.AuthorizedKeysFile, err)
	}
	var keys []ssh.PublicKey
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			log.Warn("ignoring unparsable line in authorized keys", "file", cfg.AuthorizedKeysFile, "error", err)
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		log.Warn("no usable keys found; password authentication remains available", "file", cfg.AuthorizedKeysFile)
	}
	return keys, nil
}

func loadOrCreatePassword(cfg *config.Config, log *slog.Logger) (string, error) {
	if cfg.Password != "" {
		return cfg.Password, nil
	}
	if b, err := os.ReadFile(cfg.PasswordFile); err == nil {
		if pw := strings.TrimSpace(string(b)); pw != "" {
			return pw, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(cfg.PasswordFile), 0o700); err != nil {
		return "", err
	}
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	pw := base64URL(buf)
	if err := os.WriteFile(cfg.PasswordFile, []byte(pw+"\n"), 0o600); err != nil {
		return "", err
	}
	log.Info("generated access password", "path", cfg.PasswordFile)
	return pw, nil
}

func normaliseSize(rows, cols int) (int, int) {
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}
	return rows, cols
}

func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
