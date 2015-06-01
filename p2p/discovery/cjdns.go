package discovery

// mdns introduced in https://github.com/ipfs/go-ipfs/pull/1117

import (
	"sync"
	"time"

	"github.com/ipfs/go-ipfs/p2p/host"

	"github.com/ehmry/go-cjdns/admin"
	"github.com/ehmry/go-cjdns/key"
)

type cjdnsService struct {
	admin    *admin.Conn
	host     host.Host
	lk       sync.Mutex
	notifees []Notifee
	interval time.Duration
}

func NewCjdnsService(host host.Host, interval time.Duration) (Service, error) {
	admin, err := admin.Connect(nil)
	if err != nil {
		log.Error("cjdns connect error: ", err)
	}

	log.Debug("cjdns connect")

	service := &cjdnsService{
		admin:    admin,
		host:     host,
		interval: interval,
	}

	go service.pollPeerStats()

	return service, nil
}

func (cjdns *cjdnsService) Close() error {
	return nil
}

func (cjdns *cjdnsService) pollPeerStats() {
	ticker := time.NewTicker(cjdns.interval)
	for {
		select {
		case <-ticker.C:
			results, err := cjdns.admin.InterfaceController_peerStats()
			if err != nil {
				log.Error("cjdns peerstats error: ", err)
			}

			for _, peer := range results {
				k, err := key.DecodePublic(peer.PublicKey.String())
				if err != nil {
					log.Error("malformed key: %s", peer.PublicKey.String())
				}
				log.Debugf("cjdns peer: %s", k.IP())
			}
		}
	}
}

func (c *cjdnsService) RegisterNotifee(n Notifee) {
	c.lk.Lock()
	c.notifees = append(c.notifees, n)
	c.lk.Unlock()
}

func (c *cjdnsService) UnregisterNotifee(n Notifee) {
	c.lk.Lock()
	found := -1
	for i, notif := range c.notifees {
		if notif == n {
			found = i
			break
		}
	}
	if found != -1 {
		c.notifees = append(c.notifees[:found], c.notifees[found+1:]...)
	}
	c.lk.Unlock()
}
