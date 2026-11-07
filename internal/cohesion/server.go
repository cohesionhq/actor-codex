package cohesion

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultPort is the HTTPS port cohesiond listens on unless told otherwise.
const DefaultPort = 3000

// LocalIdentity is the first line a client sends on the Unix socket.
type LocalIdentity struct {
	// A random key, kept with the client's configuration: the same key is the same client.
	Key string `json:"key"`
	// The name a client new to the daemon is listed under.
	Name string `json:"name"`
}

// ClientIdentity is a client certificate and its private key, both PEM.
type ClientIdentity struct {
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"privateKey"`
}

// Server is a cohesiond, and how this client identifies itself to it:
// `local` over its Unix socket, or `remote` over mutual TLS. It is kept in
// the client's configuration as cohesion-ts keeps it.
type Server struct {
	Kind string `json:"kind"`
	// local.
	Socket string `json:"socket,omitempty"`
	// remote.
	Host              string `json:"host,omitempty"`
	Port              int    `json:"port,omitempty"`
	ServerFingerprint string `json:"server_fingerprint,omitempty"`
	// LocalIdentity for local, ClientIdentity for remote.
	Identity json.RawMessage `json:"identity"`
}

// LocalServer is a new local key for the cohesiond on `socket`, listed as
// `name` when it first connects.
func LocalServer(socket, name string) (Server, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return Server{}, err
	}
	identity, _ := json.Marshal(LocalIdentity{Key: hex.EncodeToString(key), Name: name})
	return Server{Kind: "local", Socket: socket, Identity: identity}, nil
}

// DefaultSocketPath is where a cohesiond started without `--socket` listens.
func DefaultSocketPath() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "cohesiond", "cohesiond.sock")
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = home
		}
		return filepath.Join(base, "cohesiond", "cohesiond.sock")
	default:
		base := os.Getenv("XDG_RUNTIME_DIR")
		if base == "" {
			base = filepath.Join(home, ".config", "cohesiond")
		}
		return filepath.Join(base, "cohesiond.sock")
	}
}

// SetupCode is what a setup code carries: base64("key|address|fingerprint").
type SetupCode struct {
	Key         string
	Host        string
	Port        int
	Fingerprint string
}

// DecodeSetupCode reads a setup code from `cohesion pair`.
func DecodeSetupCode(code string) (SetupCode, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(code))
	parts := strings.Split(string(raw), "|")
	if err != nil || len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return SetupCode{}, errors.New("that isn't a setup code")
	}
	host, port := parseAddress(parts[1])
	return SetupCode{Key: parts[0], Host: host, Port: port, Fingerprint: parts[2]}, nil
}

// parseAddress splits host, host:port or [v6]:port; a bare IPv6 address has
// several colons and no port.
func parseAddress(address string) (string, int) {
	if host, port, err := net.SplitHostPort(address); err == nil {
		if n, err := strconv.Atoi(port); err == nil {
			return host, n
		}
	}
	return address, DefaultPort
}

// Pair pairs with the cohesiond a setup code names: makes this client a key
// and certificate, and registers it under `name`. The code works once. It
// returns the server to keep, and the name the client is listed under.
func Pair(ctx context.Context, setupCode, name string) (Server, string, error) {
	code, err := DecodeSetupCode(setupCode)
	if err != nil {
		return Server{}, "", err
	}
	identity, err := newIdentity(name)
	if err != nil {
		return Server{}, "", err
	}
	raw, _ := json.Marshal(identity)
	server := Server{Kind: "remote", Host: code.Host, Port: code.Port, ServerFingerprint: code.Fingerprint, Identity: raw}
	config, err := server.tlsConfig("http/1.1")
	if err != nil {
		return Server{}, "", err
	}
	body, _ := json.Marshal(map[string]string{"setup_key": code.Key, "client_name": name})
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: config}, Timeout: 30 * time.Second}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+server.authority()+"/register", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		return Server{}, "", err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return Server{}, "", errors.New("cohesiond refused the setup code; run `cohesion pair` on the server for a new one")
	}
	var frame struct {
		OK *struct {
			Data struct {
				ClientName string `json:"client_name"`
			} `json:"data"`
		} `json:"ok"`
		Error *ServerError `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&frame); err != nil {
		return Server{}, "", fmt.Errorf("cohesiond sent a malformed response: %w", err)
	}
	if frame.Error != nil {
		return Server{}, "", frame.Error
	}
	if frame.OK == nil {
		return Server{}, "", errors.New("cohesiond sent a malformed response")
	}
	return server, frame.OK.Data.ClientName, nil
}

// newIdentity makes a P-256 key and a self-signed certificate for it.
// cohesiond identifies clients by key fingerprint, not by the certificate.
func newIdentity(name string) (ClientIdentity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return ClientIdentity{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return ClientIdentity{}, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return ClientIdentity{}, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return ClientIdentity{}, err
	}
	return ClientIdentity{
		Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		PrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
	}, nil
}

// KeyFingerprint is the lowercase hex SHA-256 of a certificate's
// SubjectPublicKeyInfo, how cohesiond and its clients are identified.
func KeyFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

func (s Server) authority() string {
	return net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
}

// tlsConfig presents the client's certificate and trusts only a server whose
// key is the one its setup code named. There is no CA: cohesiond signs a new
// certificate with its key on every start, so the key is what's checked; Go
// still verifies the handshake, proving the server holds that key.
func (s Server) tlsConfig(alpn ...string) (*tls.Config, error) {
	var identity ClientIdentity
	if err := json.Unmarshal(s.Identity, &identity); err != nil {
		return nil, fmt.Errorf("the configuration's identity is malformed: %w", err)
	}
	cert, err := tls.X509KeyPair([]byte(identity.Certificate), []byte(identity.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("the configuration's identity is malformed: %w", err)
	}
	if s.ServerFingerprint == "" {
		return nil, errors.New("the configuration names no cohesiond key")
	}
	fingerprint := s.ServerFingerprint
	authority := s.authority()
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // replaced by the key check below, before anything is sent
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("cohesiond presented no certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			if KeyFingerprint(leaf) != fingerprint {
				return fmt.Errorf("the server at %s isn't the cohesiond this client paired with", authority)
			}
			return nil
		},
		NextProtos: alpn,
		MinVersion: tls.VersionTLS12,
	}, nil
}

// transport is an open connection to cohesiond carrying one JSON frame at a time.
type transport interface {
	send(frame []byte) error
	// receive blocks for the next frame; it fails once the connection ends.
	receive() ([]byte, error)
	close()
}

func connect(ctx context.Context, server Server) (transport, error) {
	if server.Kind == "local" {
		return connectLocal(ctx, server)
	}
	return connectRemote(ctx, server)
}

// localTransport is the Unix socket: one frame per line after the handshake.
type localTransport struct {
	conn   net.Conn
	reader *bufio.Reader
}

func connectLocal(ctx context.Context, server Server) (transport, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", server.Socket)
	if err != nil {
		return nil, err
	}
	// One line: the configuration keeps it indented.
	var identity bytes.Buffer
	if err := json.Compact(&identity, server.Identity); err != nil {
		conn.Close()
		return nil, fmt.Errorf("the configuration's identity is malformed: %w", err)
	}
	if _, err := conn.Write(append(identity.Bytes(), '\n')); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReaderSize(conn, 1<<20)
	line, err := readLine(reader)
	if err != nil {
		conn.Close()
		return nil, err
	}
	var reply struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(line, &reply); err != nil {
		conn.Close()
		return nil, fmt.Errorf("cohesiond sent a malformed handshake: %w", err)
	}
	if reply.Outcome != "accepted" {
		conn.Close()
		return nil, errors.New(reply.Reason)
	}
	return &localTransport{conn: conn, reader: reader}, nil
}

func readLine(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		if err == io.EOF && len(line) > 0 {
			return line, nil
		}
		return nil, err
	}
	return bytes.TrimRight(line, "\r\n"), nil
}

func (t *localTransport) send(frame []byte) error {
	_, err := t.conn.Write(append(frame, '\n'))
	return err
}

func (t *localTransport) receive() ([]byte, error) { return readLine(t.reader) }

func (t *localTransport) close() { t.conn.Close() }
