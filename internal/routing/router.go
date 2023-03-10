package routing

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/go-logr/logr"
	cid "github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/multiformats/go-multiaddr"
	mc "github.com/multiformats/go-multicodec"
	mh "github.com/multiformats/go-multihash"
)

const KeyTTL = 10 * time.Minute

type Router interface {
	Close() error
	Resolve(ctx context.Context, key string, allowSelf bool) (string, bool, error)
	Advertise(ctx context.Context, keys []string) error
}

type P2PRouter struct {
	host host.Host
	rd   *routing.RoutingDiscovery
}

func NewP2PRouter(ctx context.Context, addr string, b Bootstrapper) (Router, error) {
	log := logr.FromContextOrDiscard(ctx).WithName("p2p")

	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if h == "" {
		h = "0.0.0.0"
	}
	multiAddr, err := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%s", h, p))
	if err != nil {
		return nil, fmt.Errorf("could not create host multi address: %w", err)
	}
	host, err := libp2p.New(libp2p.ListenAddrs(multiAddr))
	if err != nil {
		return nil, fmt.Errorf("could not create host: %w", err)
	}
	self := fmt.Sprintf("%s/p2p/%s", host.Addrs()[0].String(), host.ID().Pretty())
	log.Info("starting p2p router", "id", self)

	err = b.Run(ctx, self)
	if err != nil {
		return nil, err
	}

	dhtOpts := []dht.Option{dht.Mode(dht.ModeServer), dht.ProtocolPrefix("/spegel"), dht.DisableValues(), dht.MaxRecordAge(KeyTTL)}
	bootstrapPeerOpt := dht.BootstrapPeersFunc(func() []peer.AddrInfo {
		addrInfo, err := b.GetAddress()
		if err != nil {
			log.Error(err, "could not get bootstrap addresses")
			return nil
		}
		if addrInfo.ID == host.ID() {
			log.Info("leader is self skipping connection to bootstrap node")
			return nil
		}
		return []peer.AddrInfo{*addrInfo}
	})
	dhtOpts = append(dhtOpts, bootstrapPeerOpt)
	kdht, err := dht.New(ctx, host, dhtOpts...)
	if err != nil {
		return nil, fmt.Errorf("could not create distributed hash table: %w", err)
	}
	if err = kdht.Bootstrap(ctx); err != nil {
		return nil, fmt.Errorf("could not boostrap distributed hash table: %w", err)
	}
	rd := routing.NewRoutingDiscovery(kdht)

	retry := 0
	for {
		log.Info("waiting for routing table to be populated with peers")
		if kdht.RoutingTable().Size() > 0 {
			break
		}
		retry = retry + 1
		if retry == 30 {
			return nil, fmt.Errorf("timed out waiting for routing table to populate with peers")
		}
		time.Sleep(1 * time.Second)
	}

	return &P2PRouter{
		host: host,
		rd:   rd,
	}, nil
}

func (r *P2PRouter) Close() error {
	return r.host.Close()
}

func (r *P2PRouter) Resolve(ctx context.Context, key string, allowSelf bool) (string, bool, error) {
	logr.FromContextOrDiscard(ctx).V(10).Info("resolving key", "host", r.host.ID().Pretty(), "key", key)
	c, err := createCid(key)
	if err != nil {
		return "", false, err
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := r.rd.FindProvidersAsync(cancelCtx, c, 0)
	for {
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case info, ok := <-ch:
			// Channel is closed means no provider is found.
			if !ok {
				return "", false, fmt.Errorf("key not found %s", key)
			}
			// Ignore responses that come from self if not allowed.
			if !allowSelf && info.ID == r.host.ID() {
				continue
			}
			for _, addr := range info.Addrs {
				v, err := addr.ValueForProtocol(multiaddr.P_IP4)
				if err != nil {
					return "", false, err
				}
				// Protect against empty values beeing returned.
				if v == "" {
					continue
				}
				// There are two IPs being advertised, one is localhost which is not useful.
				if v == "127.0.0.1" {
					continue
				}
				return v, true, nil
			}
		}
	}
}

func (r *P2PRouter) Advertise(ctx context.Context, keys []string) error {
	logr.FromContextOrDiscard(ctx).V(10).Info("advertising keys", "host", r.host.ID().Pretty(), "keys", keys)
	for _, key := range keys {
		c, err := createCid(key)
		if err != nil {
			return err
		}
		err = r.rd.Provide(ctx, c, false)
		if err != nil {
			return err
		}
	}
	return nil
}

func createCid(key string) (cid.Cid, error) {
	pref := cid.Prefix{
		Version:  1,
		Codec:    uint64(mc.Raw),
		MhType:   mh.SHA2_256,
		MhLength: -1,
	}
	c, err := pref.Sum([]byte(key))
	if err != nil {
		return cid.Cid{}, err
	}
	return c, nil
}
