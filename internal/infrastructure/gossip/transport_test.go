package gossip_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/gossip"
)

type testGateway struct {
	domain.MeshGateway
	address string
}

func (g testGateway) Dial(ctx context.Context, _ string, _ uint16) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", g.address)
}

func TestMeshTransportRequiresAndSendsSharedToken(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Header.Get("Authorization") != "Bearer shared-secret" {
			t.Error("gossip authentication header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"ack"}`))
	}))
	defer server.Close()
	transport := gossip.NewMeshTransport(testGateway{address: server.Listener.Addr().String()})
	msg := &entity.GossipMessage{Type: entity.GossipMsgPing}
	if _, err := transport.SendMessage(context.Background(), "peer", 19800, msg); err == nil {
		t.Fatal("unconfigured gossip transport accepted send")
	}
	if called {
		t.Fatal("unconfigured transport reached peer")
	}
	transport.SetAuthToken("shared-secret")
	reply, err := transport.SendMessage(context.Background(), "peer", 19800, msg)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Type != entity.GossipMsgAck || !called {
		t.Fatal("authenticated gossip message failed")
	}
}
