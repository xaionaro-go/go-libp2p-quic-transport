package libp2pquic

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"

	ic "github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"
	tpt "github.com/libp2p/go-libp2p-core/transport"

	quic "github.com/lucas-clemente/quic-go"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr-net"
	"github.com/whyrusleeping/mafmt"
)

var quicConfig = &quic.Config{
	MaxIncomingStreams:                    1000,
	MaxIncomingUniStreams:                 -1,              // disable unidirectional streams
	MaxReceiveStreamFlowControlWindow:     3 * (1 << 20),   // 3 MB
	MaxReceiveConnectionFlowControlWindow: 4.5 * (1 << 20), // 4.5 MB
	AcceptToken: func(clientAddr net.Addr, token *quic.Token) bool {
		// TODO(#6): require source address validation when under load
		return true
	},
	KeepAlive: true,
}

type connManager struct {
	mutex sync.Mutex

	connIPv4 net.PacketConn
	connIPv6 net.PacketConn
}

func (c *connManager) GetConnForAddr(network string) (net.PacketConn, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	switch network {
	case "udp4":
		if c.connIPv4 != nil {
			return c.connIPv4, nil
		}
		var err error
		c.connIPv4, err = c.createConn(network, "0.0.0.0:0")
		return c.connIPv4, err
	case "udp6":
		if c.connIPv6 != nil {
			return c.connIPv6, nil
		}
		var err error
		c.connIPv6, err = c.createConn(network, ":0")
		return c.connIPv6, err
	default:
		return nil, fmt.Errorf("unsupported network: %s", network)
	}
}

func (c *connManager) createConn(network, host string) (net.PacketConn, error) {
	addr, err := net.ResolveUDPAddr(network, host)
	if err != nil {
		return nil, err
	}
	return net.ListenUDP(network, addr)
}

// The Transport implements the tpt.Transport interface for QUIC connections.
type transport struct {
	privKey     ic.PrivKey
	localPeer   peer.ID
	tlsConf     *tls.Config
	connManager *connManager
}

var _ tpt.Transport = &transport{}

// NewTransport creates a new QUIC transport
func NewTransport(key ic.PrivKey) (tpt.Transport, error) {
	localPeer, err := peer.IDFromPrivateKey(key)
	if err != nil {
		return nil, err
	}
	tlsConf, err := generateConfig(key)
	if err != nil {
		return nil, err
	}

	return &transport{
		privKey:     key,
		localPeer:   localPeer,
		tlsConf:     tlsConf,
		connManager: &connManager{},
	}, nil
}

// Dial dials a new QUIC connection
func (t *transport) Dial(ctx context.Context, raddr ma.Multiaddr, p peer.ID) (tpt.CapableConn, error) {
	network, host, err := manet.DialArgs(raddr)
	if err != nil {
		return nil, err
	}
	pconn, err := t.connManager.GetConnForAddr(network)
	if err != nil {
		return nil, err
	}
	addr, err := fromQuicMultiaddr(raddr)
	if err != nil {
		return nil, err
	}
	var remotePubKey ic.PubKey
	tlsConf := t.tlsConf.Clone()
	// We need to check the peer ID in the VerifyPeerCertificate callback.
	// The tls.Config it is also used for listening, and we might also have concurrent dials.
	// Clone it so we can check for the specific peer ID we're dialing here.
	tlsConf.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		chain := make([]*x509.Certificate, len(rawCerts))
		for i := 0; i < len(rawCerts); i++ {
			cert, err := x509.ParseCertificate(rawCerts[i])
			if err != nil {
				return err
			}
			chain[i] = cert
		}
		var err error
		remotePubKey, err = getRemotePubKey(chain)
		if err != nil {
			return err
		}
		if !p.MatchesPublicKey(remotePubKey) {
			return errors.New("peer IDs don't match")
		}
		return nil
	}
	sess, err := quic.DialContext(ctx, pconn, addr, host, tlsConf, quicConfig)
	if err != nil {
		return nil, err
	}
	localMultiaddr, err := toQuicMultiaddr(sess.LocalAddr())
	if err != nil {
		return nil, err
	}
	return &conn{
		sess:            sess,
		transport:       t,
		privKey:         t.privKey,
		localPeer:       t.localPeer,
		localMultiaddr:  localMultiaddr,
		remotePubKey:    remotePubKey,
		remotePeerID:    p,
		remoteMultiaddr: raddr,
	}, nil
}

// CanDial determines if we can dial to an address
func (t *transport) CanDial(addr ma.Multiaddr) bool {
	return mafmt.QUIC.Matches(addr)
}

// Listen listens for new QUIC connections on the passed multiaddr.
func (t *transport) Listen(addr ma.Multiaddr) (tpt.Listener, error) {
	return newListener(addr, t, t.localPeer, t.privKey, t.tlsConf)
}

// Proxy returns true if this transport proxies.
func (t *transport) Proxy() bool {
	return false
}

// Protocols returns the set of protocols handled by this transport.
func (t *transport) Protocols() []int {
	return []int{ma.P_QUIC}
}

func (t *transport) String() string {
	return "QUIC"
}
