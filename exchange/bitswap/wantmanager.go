package bitswap

import (
	"sync"
	"time"

	context "github.com/ipfs/go-ipfs/Godeps/_workspace/src/golang.org/x/net/context"
	engine "github.com/ipfs/go-ipfs/exchange/bitswap/decision"
	bsmsg "github.com/ipfs/go-ipfs/exchange/bitswap/message"
	bsnet "github.com/ipfs/go-ipfs/exchange/bitswap/network"
	wantlist "github.com/ipfs/go-ipfs/exchange/bitswap/wantlist"
	peer "github.com/ipfs/go-ipfs/p2p/peer"
	u "github.com/ipfs/go-ipfs/util"
)

type WantManager struct {
	// sync channels for Run loop
	incoming   chan []*bsmsg.Entry
	connect    chan peer.ID // notification channel for new peers connecting
	disconnect chan peer.ID // notification channel for peers disconnecting

	// synchronized by Run loop, only touch inside there
	peers map[peer.ID]*msgQueue
	wl    *wantlist.Wantlist

	network bsnet.BitSwapNetwork
	ctx     context.Context
}

func NewWantManager(ctx context.Context, network bsnet.BitSwapNetwork) *WantManager {
	return &WantManager{
		incoming:   make(chan []*bsmsg.Entry, 10),
		connect:    make(chan peer.ID, 10),
		disconnect: make(chan peer.ID, 10),
		peers:      make(map[peer.ID]*msgQueue),
		wl:         wantlist.New(),
		network:    network,
		ctx:        ctx,
	}
}

type msgPair struct {
	to  peer.ID
	msg bsmsg.BitSwapMessage
}

type cancellation struct {
	who peer.ID
	blk u.Key
}

type msgQueue struct {
	p peer.ID

	outlk   sync.Mutex
	out     bsmsg.BitSwapMessage
	network bsnet.BitSwapNetwork

	work chan struct{}
	done chan struct{}
}

func (pm *WantManager) WantBlocks(ks []u.Key) {
	log.Infof("want blocks: %s", ks)
	pm.addEntries(ks, false)
}

func (pm *WantManager) CancelWants(ks []u.Key) {
	pm.addEntries(ks, true)
}

func (pm *WantManager) addEntries(ks []u.Key, cancel bool) {
	var entries []*bsmsg.Entry
	for i, k := range ks {
		entries = append(entries, &bsmsg.Entry{
			Cancel: cancel,
			Entry: wantlist.Entry{
				Key:      k,
				Priority: kMaxPriority - i,
			},
		})
	}
	select {
	case pm.incoming <- entries:
	case <-pm.ctx.Done():
	}
}

func (pm *WantManager) SendBlock(ctx context.Context, env *engine.Envelope) {
	// Blocks need to be sent synchronously to maintain proper backpressure
	// throughout the network stack
	defer env.Sent()

	msg := bsmsg.NewPartial()
	msg.AddBlock(env.Block)
	log.Infof("Sending block %s to %s", env.Peer, env.Block)
	err := pm.network.SendMessage(ctx, env.Peer, msg)
	if err != nil {
		log.Noticef("sendblock error: %s", err)
	}
}

func (pm *WantManager) startPeerHandler(p peer.ID) *msgQueue {
	_, ok := pm.peers[p]
	if ok {
		// TODO: log an error?
		return nil
	}

	mq := pm.newMsgQueue(p)

	// new peer, we will want to give them our full wantlist
	fullwantlist := bsmsg.NewFull()
	for _, e := range pm.wl.Entries() {
		fullwantlist.AddEntry(e.Key, e.Priority)
	}
	mq.out = fullwantlist
	mq.work <- struct{}{}

	pm.peers[p] = mq
	go mq.runQueue(pm.ctx)
	return mq
}

func (pm *WantManager) stopPeerHandler(p peer.ID) {
	pq, ok := pm.peers[p]
	if !ok {
		// TODO: log error?
		return
	}

	close(pq.done)
	delete(pm.peers, p)
}

func (mq *msgQueue) runQueue(ctx context.Context) {
	for {
		select {
		case <-mq.work: // there is work to be done

			err := mq.network.ConnectTo(ctx, mq.p)
			if err != nil {
				log.Noticef("cant connect to peer %s: %s", mq.p, err)
				// TODO: cant connect, what now?
				continue
			}

			// grab outgoing message
			mq.outlk.Lock()
			wlm := mq.out
			if wlm == nil || wlm.Empty() {
				mq.outlk.Unlock()
				continue
			}
			mq.out = nil
			mq.outlk.Unlock()

			// send wantlist updates
			err = mq.network.SendMessage(ctx, mq.p, wlm)
			if err != nil {
				log.Noticef("bitswap send error: %s", err)
				// TODO: what do we do if this fails?
			}
		case <-mq.done:
			return
		}
	}
}

func (pm *WantManager) Connected(p peer.ID) {
	pm.connect <- p
}

func (pm *WantManager) Disconnected(p peer.ID) {
	pm.disconnect <- p
}

// TODO: use goprocess here once i trust it
func (pm *WantManager) Run() {
	tock := time.NewTicker(rebroadcastDelay.Get())
	for {
		select {
		case entries := <-pm.incoming:

			// add changes to our wantlist
			for _, e := range entries {
				if e.Cancel {
					pm.wl.Remove(e.Key)
				} else {
					pm.wl.Add(e.Key, e.Priority)
				}
			}

			// broadcast those wantlist changes
			for _, p := range pm.peers {
				p.addMessage(entries)
			}

		case <-tock.C:
			// resend entire wantlist every so often (REALLY SHOULDNT BE NECESSARY)
			var es []*bsmsg.Entry
			for _, e := range pm.wl.Entries() {
				es = append(es, &bsmsg.Entry{Entry: e})
			}
			for _, p := range pm.peers {
				p.outlk.Lock()
				p.out = bsmsg.NewFull()
				p.outlk.Unlock()

				p.addMessage(es)
			}
		case p := <-pm.connect:
			pm.startPeerHandler(p)
		case p := <-pm.disconnect:
			pm.stopPeerHandler(p)
		case <-pm.ctx.Done():
			return
		}
	}
}

func (wm *WantManager) newMsgQueue(p peer.ID) *msgQueue {
	mq := new(msgQueue)
	mq.done = make(chan struct{})
	mq.work = make(chan struct{}, 1)
	mq.network = wm.network
	mq.p = p

	return mq
}

func (mq *msgQueue) addMessage(entries []*bsmsg.Entry) {
	mq.outlk.Lock()
	defer func() {
		mq.outlk.Unlock()
		select {
		case mq.work <- struct{}{}:
		default:
		}
	}()

	// if we have no message held overwrite the one we are holding
	if mq.out == nil {
		mq.out = bsmsg.NewPartial()
	}

	// TODO: add a msg.Combine(...) method
	// otherwise, combine the one we are holding with the
	// one passed in
	for _, e := range entries {
		if e.Cancel {
			mq.out.Cancel(e.Key)
		} else {
			mq.out.AddEntry(e.Key, e.Priority)
		}
	}
}
