package cohesion

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
)

// remoteTransport is the `/connect` WebSocket over mutual TLS: one frame per message.
type remoteTransport struct {
	conn *websocket.Conn
}

// The most one frame from cohesiond may be: a transcript with its images.
const readLimit = 256 << 20

func connectRemote(ctx context.Context, server Server) (transport, error) {
	config, err := server.tlsConfig("http/1.1")
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: config}}
	conn, _, err := websocket.Dial(ctx, "wss://"+server.authority()+"/connect", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(readLimit)
	return &remoteTransport{conn: conn}, nil
}

func (t *remoteTransport) send(frame []byte) error {
	return t.conn.Write(context.Background(), websocket.MessageText, frame)
}

func (t *remoteTransport) receive() ([]byte, error) {
	_, data, err := t.conn.Read(context.Background())
	return data, err
}

func (t *remoteTransport) close() { t.conn.Close(websocket.StatusNormalClosure, "") }
