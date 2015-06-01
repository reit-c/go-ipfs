package discovery

import (
	"io"

	"github.com/ipfs/go-ipfs/p2p/peer"
	"github.com/ipfs/go-ipfs/util"
)

var log = util.Logger("discovery")

type Service interface {
	io.Closer
	RegisterNotifee(Notifee)
	UnregisterNotifee(Notifee)
}

type Notifee interface {
	HandlePeerFound(peer.PeerInfo)
}
