package pex

import (
	"errors"
	"fmt"
	"sync"
	"time"

	tmp2p "github.com/cometbft/cometbft/api/cometbft/p2p/v1"
	"github.com/cometbft/cometbft/v2/internal/cmap"
	cmtrand "github.com/cometbft/cometbft/v2/internal/rand"
	cmtmath "github.com/cometbft/cometbft/v2/libs/math"
	"github.com/cometbft/cometbft/v2/libs/service"
	"github.com/cometbft/cometbft/v2/p2p"
	"github.com/cometbft/cometbft/v2/p2p/internal/nodekey"
	na "github.com/cometbft/cometbft/v2/p2p/netaddr"
	"github.com/cometbft/cometbft/v2/p2p/transport"
	tcpconn "github.com/cometbft/cometbft/v2/p2p/transport/tcp/conn"
)

type Peer = p2p.Peer

const (
	// PexChannel is a channel for PEX messages.
	PexChannel = byte(0x00)

	// over-estimate of max na.NetAddr size
	// hexID (40) + IP (16) + Port (2) + Name (100) ...
	// NOTE: dont use massive DNS name ..
	maxAddressSize = 256

	// NOTE: amplificaiton factor!
	// small request results in up to maxMsgSize response.
	maxMsgSize = maxAddressSize * maxGetSelection

	// ensure we have enough peers.
	defaultEnsurePeersPeriod = 30 * time.Second

	// Seed/Crawler constants.

	// minTimeBetweenCrawls is a minimum time between attempts to crawl a peer.
	minTimeBetweenCrawls = 2 * time.Minute

	// check some peers every this.
	crawlPeerPeriod = 30 * time.Second

	maxAttemptsToDial = 16 // ~ 35h in total (last attempt - 18h)

	// if node connects to seed, it does not have any trusted peers.
	// Especially in the beginning, node should have more trusted peers than
	// untrusted.
	biasToSelectNewPeers = 30 // 70 to select good peers

	// if a peer is marked bad, it will be banned for at least this time period.
	defaultBanTime = 24 * time.Hour
)

// Reactor handles PEX (peer exchange) and ensures that an
// adequate number of peers are connected to the switch.
//
// It uses `AddrBook` (address book) to store `na.NetAddr`es of the peers.
//
// ## Preventing abuse
//
// Only accept pexAddrsMsg from peers we sent a corresponding pexRequestMsg too.
// Only accept one pexRequestMsg every ~defaultEnsurePeersPeriod.
type Reactor struct {
	p2p.BaseReactor

	book           AddrBook
	config         *ReactorConfig
	ensurePeersCh  chan struct{} // Wakes up ensurePeersRoutine()
	peersRoutineWg sync.WaitGroup

	// maps to prevent abuse
	requestsSent         *cmap.CMap // ID->struct{}: unanswered send requests
	lastReceivedRequests *cmap.CMap // ID->time.Time: last time peer requested from us

	seedAddrs []*na.NetAddr

	attemptsToDial sync.Map // address (string) -> {number of attempts (int), last time dialed (time.Time)}

	// seed/crawled mode fields
	crawlPeerInfos map[nodekey.ID]crawlPeerInfo
}

func (r *Reactor) minReceiveRequestInterval() time.Duration {
	// NOTE: must be less than ensurePeersPeriod, otherwise we'll request
	// peers too quickly from others and they'll think we're bad!
	return r.config.EnsurePeersPeriod / 3
}

// ReactorConfig holds reactor specific configuration data.
type ReactorConfig struct {
	// Seed/Crawler mode
	SeedMode bool

	// We want seeds to only advertise good peers. Therefore they should wait at
	// least as long as we expect it to take for a peer to become good before
	// disconnecting.
	SeedDisconnectWaitPeriod time.Duration

	// Maximum pause when redialing a persistent peer (if zero, exponential backoff is used)
	PersistentPeersMaxDialPeriod time.Duration

	// Period to ensure sufficient peers are connected
	EnsurePeersPeriod time.Duration

	// Seeds is a list of addresses reactor may use
	// if it can't connect to peers in the addrbook.
	Seeds []string
}

type _attemptsToDial struct {
	number     int
	lastDialed time.Time
}

// NewReactor creates new PEX reactor.
func NewReactor(b AddrBook, config *ReactorConfig) *Reactor {
	if config.EnsurePeersPeriod == 0 {
		config.EnsurePeersPeriod = defaultEnsurePeersPeriod
	}

	r := &Reactor{
		book:                 b,
		config:               config,
		ensurePeersCh:        make(chan struct{}),
		requestsSent:         cmap.NewCMap(),
		lastReceivedRequests: cmap.NewCMap(),
		crawlPeerInfos:       make(map[nodekey.ID]crawlPeerInfo),
	}
	r.BaseReactor = *p2p.NewBaseReactor("PEX", r)
	return r
}

// OnStart implements BaseService.
func (r *Reactor) OnStart() error {
	err := r.book.Start()
	if err != nil && !errors.Is(err, service.ErrAlreadyStarted) {
		return err
	}

	numOnline, seedAddrs, err := r.checkSeeds()
	if err != nil {
		return err
	} else if numOnline == 0 && r.book.Empty() {
		return ErrEmptyAddressBook
	}

	r.seedAddrs = seedAddrs

	r.peersRoutineWg.Add(1)
	// Check if this node should run
	// in seed/crawler mode
	if r.config.SeedMode {
		go r.crawlPeersRoutine()
	} else {
		go r.ensurePeersRoutine()
	}
	return nil
}

// Stop overrides `Service.Stop()`.
func (r *Reactor) Stop() error {
	if err := r.BaseReactor.Stop(); err != nil {
		return err
	}
	if err := r.book.Stop(); err != nil {
		return fmt.Errorf("can't stop address book: %w", err)
	}
	r.peersRoutineWg.Wait()
	return nil
}

// StreamDescriptors implements Reactor.
func (*Reactor) StreamDescriptors() []transport.StreamDescriptor {
	return []transport.StreamDescriptor{
		tcpconn.StreamDescriptor{
			ID:                  PexChannel,
			Priority:            1,
			SendQueueCapacity:   10,
			RecvMessageCapacity: maxMsgSize,
			MessageTypeI:        &tmp2p.Message{},
		},
	}
}

// AddPeer implements Reactor by adding peer to the address book (if inbound)
// or by requesting more addresses (if outbound).
func (r *Reactor) AddPeer(p Peer) {
	if p.IsOutbound() {
		// For outbound peers, the address is already in the books -
		// either via DialPeersAsync or r.Receive.
		// Ask it for more peers if we need.
		if r.book.NeedMoreAddrs() {
			r.RequestAddrs(p)
		}
	} else {
		// inbound peer is its own source
		addr, err := p.NodeInfo().NetAddr()
		if err != nil {
			r.Logger.Error("Failed to get peer NetAddr", "err", err, "peer", p)
			return
		}

		// Make it explicit that addr and src are the same for an inbound peer.
		src := addr

		// add to book. dont RequestAddrs right away because
		// we don't trust inbound as much - let ensurePeersRoutine handle it.
		err = r.book.AddAddress(addr, src)
		r.logErrAddrBook(err)
	}
}

// RemovePeer implements Reactor by resetting peer's requests info.
func (r *Reactor) RemovePeer(p Peer, _ any) {
	id := p.ID()
	r.requestsSent.Delete(id)
	r.lastReceivedRequests.Delete(id)
}

func (r *Reactor) logErrAddrBook(err error) {
	if err != nil {
		switch err.(type) {
		case ErrAddrBookNilAddr:
			r.Logger.Error("Failed to add new address", "err", err)
		default:
			// non-routable, self, full book, private, etc.
			r.Logger.Debug("Failed to add new address", "err", err)
		}
	}
}

// Receive implements Reactor by handling incoming PEX messages.
func (r *Reactor) Receive(e p2p.Envelope) {
	r.Logger.Debug("Received message", "src", e.Src, "chId", e.ChannelID, "msg", e.Message)

	switch msg := e.Message.(type) {
	case *tmp2p.PexRequest:

		// NOTE: this is a prime candidate for amplification attacks,
		// so it's important we
		// 1) restrict how frequently peers can request
		// 2) limit the output size

		// If we're a seed and this is an inbound peer,
		// respond once and disconnect.
		if r.config.SeedMode && !e.Src.IsOutbound() {
			id := e.Src.ID()
			v := r.lastReceivedRequests.Get(id)
			if v != nil {
				// FlushStop/StopPeer are already
				// running in a go-routine.
				return
			}
			r.lastReceivedRequests.Set(id, time.Now())

			// Send addrs and disconnect
			r.SendAddrs(e.Src, r.book.GetSelectionWithBias(biasToSelectNewPeers))
			go func() {
				// In a go-routine so it doesn't block .Receive.
				e.Src.FlushStop()
				r.Switch.StopPeerGracefully(e.Src)
			}()
		} else {
			// Check we're not receiving requests too frequently.
			if err := r.receiveRequest(e.Src); err != nil {
				r.Switch.StopPeerForError(e.Src, err)
				r.book.MarkBad(e.Src.SocketAddr(), defaultBanTime)
				return
			}
			r.SendAddrs(e.Src, r.book.GetSelection())
		}

	case *tmp2p.PexAddrs:
		// If we asked for addresses, add them to the book
		addrs, err := na.AddrsFromProtos(msg.Addrs)
		if err != nil {
			r.Switch.StopPeerForError(e.Src, err)
			r.book.MarkBad(e.Src.SocketAddr(), defaultBanTime)
			return
		}
		err = r.ReceiveAddrs(addrs, e.Src)
		if err != nil {
			r.Switch.StopPeerForError(e.Src, err)
			if errors.Is(err, ErrUnsolicitedList) {
				r.book.MarkBad(e.Src.SocketAddr(), defaultBanTime)
			}
			return
		}

	default:
		r.Logger.Error(fmt.Sprintf("Unknown message type %T", msg))
	}
}

// enforces a minimum amount of time between requests.
func (r *Reactor) receiveRequest(src Peer) error {
	id := src.ID()
	v := r.lastReceivedRequests.Get(id)
	if v == nil {
		// initialize with empty time
		lastReceived := time.Time{}
		r.lastReceivedRequests.Set(id, lastReceived)
		return nil
	}

	lastReceived := v.(time.Time)
	if lastReceived.Equal(time.Time{}) {
		// first time gets a free pass. then we start tracking the time
		lastReceived = time.Now()
		r.lastReceivedRequests.Set(id, lastReceived)
		return nil
	}

	now := time.Now()
	minInterval := r.minReceiveRequestInterval()
	if now.Sub(lastReceived) < minInterval {
		return ErrReceivedPEXRequestTooSoon{
			Peer:         src.ID(),
			LastReceived: lastReceived,
			Now:          now,
			MinInterval:  minInterval,
		}
	}
	r.lastReceivedRequests.Set(id, now)
	return nil
}

// RequestAddrs asks peer for more addresses if we do not already have a
// request out for this peer.
func (r *Reactor) RequestAddrs(p Peer) {
	id := p.ID()
	if r.requestsSent.Has(id) {
		return
	}
	r.Logger.Debug("Request addrs", "from", p)
	r.requestsSent.Set(id, struct{}{})
	_ = p.Send(p2p.Envelope{
		ChannelID: PexChannel,
		Message:   &tmp2p.PexRequest{},
	})
}

// ReceiveAddrs adds the given addrs to the addrbook if there's an open
// request for this peer and deletes the open request.
// If there's no open request for the src peer, it returns an error.
func (r *Reactor) ReceiveAddrs(addrs []*na.NetAddr, src Peer) error {
	id := src.ID()
	if !r.requestsSent.Has(id) {
		return ErrUnsolicitedList
	}
	r.requestsSent.Delete(id)

	srcAddr, err := src.NodeInfo().NetAddr()
	if err != nil {
		return err
	}

	for _, netAddr := range addrs {
		// NOTE: we check netAddr validity and routability in book#AddAddress.
		err = r.book.AddAddress(netAddr, srcAddr)
		if err != nil {
			r.logErrAddrBook(err)
			// XXX: should we be strict about incoming data and disconnect from a
			// peer here too?
			continue
		}
	}

	// Try to connect to addresses coming from a seed node without waiting (#2093)
	for _, seedAddr := range r.seedAddrs {
		if seedAddr.Equals(srcAddr) {
			select {
			case r.ensurePeersCh <- struct{}{}:
			default:
			}
			break
		}
	}

	return nil
}

// SendAddrs sends addrs to the peer.
func (*Reactor) SendAddrs(p Peer, netAddrs []*na.NetAddr) {
	e := p2p.Envelope{
		ChannelID: PexChannel,
		Message:   &tmp2p.PexAddrs{Addrs: na.AddrsToProtos(netAddrs)},
	}
	_ = p.Send(e)
}

// Ensures that sufficient peers are connected. (continuous).
func (r *Reactor) ensurePeersRoutine() {
	defer r.peersRoutineWg.Done()

	var (
		seed   = cmtrand.NewRand()
		jitter = seed.Int63n(r.config.EnsurePeersPeriod.Nanoseconds())
	)

	// Randomize first round of communication to avoid thundering herd.
	// If no peers are present directly start connecting so we guarantee swift
	// setup with the help of configured seeds.
	if r.nodeHasSomePeersOrDialingAny() {
		time.Sleep(time.Duration(jitter))
	}

	// fire once immediately.
	// ensures we dial the seeds right away if the book is empty
	r.ensurePeers(true)

	// fire periodically
	ticker := time.NewTicker(r.config.EnsurePeersPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.ensurePeers(true)
		case <-r.ensurePeersCh:
			r.ensurePeers(false)
		case <-r.book.Quit():
			return
		case <-r.Quit():
			return
		}
	}
}

// ensurePeers ensures that sufficient peers are connected. (once)
//
// heuristic that we haven't perfected yet, or, perhaps is manually edited by
// the node operator. It should not be used to compute what addresses are
// already connected or not.
func (r *Reactor) ensurePeers(ensurePeersPeriodElapsed bool) {
	var (
		out, in, dial = r.Switch.NumPeers()
		numToDial     = r.Switch.MaxNumOutboundPeers() - (out + dial)
	)
	r.Logger.Info(
		"Ensure peers",
		"numOutPeers", out,
		"numInPeers", in,
		"numDialing", dial,
		"numToDial", numToDial,
	)

	if numToDial <= 0 {
		return
	}

	// bias to prefer more vetted peers when we have fewer connections.
	// not perfect, but somewhate ensures that we prioritize connecting to more-vetted
	// NOTE: range here is [10, 90]. Too high ?
	newBias := cmtmath.MinInt(out, 8)*10 + 10

	toDial := make(map[nodekey.ID]*na.NetAddr)
	// Try maxAttempts times to pick numToDial addresses to dial
	maxAttempts := numToDial * 3

	for i := 0; i < maxAttempts && len(toDial) < numToDial; i++ {
		if !r.IsRunning() || !r.book.IsRunning() {
			return
		}

		try := r.book.PickAddress(newBias)
		if try == nil {
			continue
		}
		if _, selected := toDial[try.ID]; selected {
			continue
		}
		if r.Switch.IsDialingOrExistingAddress(try) {
			continue
		}
		// TODO: consider moving some checks from toDial into here
		// so we don't even consider dialing peers that we want to wait
		// before dialing again, or have dialed too many times already
		toDial[try.ID] = try
	}

	// Dial picked addresses
	for _, addr := range toDial {
		go func(addr *na.NetAddr) {
			err := r.dialPeer(addr)
			if err != nil {
				switch err.(type) {
				case ErrMaxAttemptsToDial, ErrTooEarlyToDial:
					r.Logger.Debug(err.Error(), "addr", addr)
				default:
					r.Logger.Debug(err.Error(), "addr", addr)
				}
			}
		}(addr)
	}

	if r.book.NeedMoreAddrs() {
		// Check if banned nodes can be reinstated
		r.book.ReinstateBadPeers()
	}

	if r.book.NeedMoreAddrs() {
		// 1) Pick a random peer and ask for more.
		peer := r.Switch.Peers().Random()
		if peer != nil && ensurePeersPeriodElapsed {
			r.Logger.Info("We need more addresses. Sending pexRequest to random peer", "peer", peer)
			r.RequestAddrs(peer)
		}

		// 2) Dial seeds if we are not dialing anyone.
		// This is done in addition to asking a peer for addresses to work-around
		// peers not participating in PEX.
		if len(toDial) == 0 {
			r.Logger.Info("No addresses to dial. Falling back to seeds")
			r.dialSeeds()
		}
	}
}

func (r *Reactor) dialAttemptsInfo(addr *na.NetAddr) (attempts int, lastDialed time.Time) {
	_attempts, ok := r.attemptsToDial.Load(addr.DialString())
	if !ok {
		return 0, time.Time{}
	}
	atd := _attempts.(_attemptsToDial)
	return atd.number, atd.lastDialed
}

func (r *Reactor) dialPeer(addr *na.NetAddr) error {
	attempts, lastDialed := r.dialAttemptsInfo(addr)
	if !r.Switch.IsPeerPersistent(addr) && attempts > maxAttemptsToDial {
		r.book.MarkBad(addr, defaultBanTime)
		return ErrMaxAttemptsToDial{Max: maxAttemptsToDial}
	}

	// exponential backoff if it's not our first attempt to dial given address
	if attempts > 0 {
		jitter := time.Duration(cmtrand.Float64() * float64(time.Second)) // 1s == (1e9 ns)
		backoffDuration := jitter + ((1 << uint(attempts)) * time.Second)
		backoffDuration = r.maxBackoffDurationForPeer(addr, backoffDuration)
		sinceLastDialed := time.Since(lastDialed)
		if sinceLastDialed < backoffDuration {
			return ErrTooEarlyToDial{backoffDuration, lastDialed}
		}
	}

	err := r.Switch.DialPeerWithAddress(addr)
	if err != nil {
		if _, ok := err.(p2p.ErrCurrentlyDialingOrExistingAddress); ok {
			return err
		}

		markAddrInBookBasedOnErr(addr, r.book, err)
		switch err.(type) {
		case p2p.ErrSwitchAuthenticationFailure:
			// NOTE: addr is removed from addrbook in markAddrInBookBasedOnErr
			r.attemptsToDial.Delete(addr.DialString())
		default:
			r.attemptsToDial.Store(addr.DialString(), _attemptsToDial{attempts + 1, time.Now()})
		}
		return ErrFailedToDial{attempts + 1, err}
	}

	// cleanup any history
	r.attemptsToDial.Delete(addr.DialString())
	return nil
}

// maxBackoffDurationForPeer caps the backoff duration for persistent peers.
func (r *Reactor) maxBackoffDurationForPeer(addr *na.NetAddr, planned time.Duration) time.Duration {
	if r.config.PersistentPeersMaxDialPeriod > 0 &&
		planned > r.config.PersistentPeersMaxDialPeriod &&
		r.Switch.IsPeerPersistent(addr) {
		return r.config.PersistentPeersMaxDialPeriod
	}
	return planned
}

// checkSeeds checks that addresses are well formed.
// Returns number of seeds we can connect to, along with all seeds addrs.
// return err if user provided any badly formatted seed addresses.
// Doesn't error if the seed node can't be reached.
// numOnline returns -1 if no seed nodes were in the initial configuration.
func (r *Reactor) checkSeeds() (numOnline int, netAddrs []*na.NetAddr, err error) {
	lSeeds := len(r.config.Seeds)
	if lSeeds == 0 {
		return -1, nil, nil
	}
	netAddrs, errs := na.NewFromStrings(r.config.Seeds)
	numOnline = lSeeds - len(errs)
	for _, err := range errs {
		switch e := err.(type) {
		case na.ErrLookup:
			r.Logger.Error("Connecting to seed failed", "err", e)
		default:
			return 0, nil, ErrSeedNodeConfig{Err: err}
		}
	}
	return numOnline, netAddrs, nil
}

// randomly dial seeds until we connect to one or exhaust them.
func (r *Reactor) dialSeeds() {
	perm := cmtrand.Perm(len(r.seedAddrs))
	// perm := r.Switch.rng.Perm(lSeeds)
	for _, i := range perm {
		// dial a random seed
		seedAddr := r.seedAddrs[i]
		err := r.Switch.DialPeerWithAddress(seedAddr)

		switch err.(type) {
		case nil, p2p.ErrCurrentlyDialingOrExistingAddress:
			return
		}
		r.Switch.Logger.Error("Error dialing seed", "err", err, "seed", seedAddr)
	}
	// do not write error message if there were no seeds specified in config
	if len(r.seedAddrs) > 0 {
		r.Switch.Logger.Error("Couldn't connect to any seeds")
	}
}

// AttemptsToDial returns the number of attempts to dial specific address. It
// returns 0 if never attempted or successfully connected.
func (r *Reactor) AttemptsToDial(addr *na.NetAddr) int {
	lAttempts, attempted := r.attemptsToDial.Load(addr.DialString())
	if attempted {
		return lAttempts.(_attemptsToDial).number
	}
	return 0
}

// ----------------------------------------------------------

// Explores the network searching for more peers. (continuous)
// Seed/Crawler Mode causes this node to quickly disconnect
// from peers, except other seed nodes.
func (r *Reactor) crawlPeersRoutine() {
	defer r.peersRoutineWg.Done()

	// If we have any seed nodes, consult them first
	if len(r.seedAddrs) > 0 {
		r.dialSeeds()
	} else {
		// Do an initial crawl
		r.crawlPeers(r.book.GetSelection())
	}

	// Fire periodically
	ticker := time.NewTicker(crawlPeerPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.attemptDisconnects()
			r.crawlPeers(r.book.GetSelection())
			r.cleanupCrawlPeerInfos()
		case <-r.book.Quit():
			return
		case <-r.Quit():
			return
		}
	}
}

// nodeHasSomePeersOrDialingAny returns true if the node is connected to some
// peers or dialing them currently.
func (r *Reactor) nodeHasSomePeersOrDialingAny() bool {
	out, in, dial := r.Switch.NumPeers()
	return out+in+dial > 0
}

// crawlPeerInfo handles temporary data needed for the network crawling
// performed during seed/crawler mode.
type crawlPeerInfo struct {
	Addr *na.NetAddr `json:"addr"`
	// The last time we crawled the peer or attempted to do so.
	LastCrawled time.Time `json:"last_crawled"`
}

// crawlPeers will crawl the network looking for new peer addresses.
func (r *Reactor) crawlPeers(addrs []*na.NetAddr) {
	now := time.Now()

	for _, addr := range addrs {
		peerInfo, ok := r.crawlPeerInfos[addr.ID]

		// Do not attempt to connect with peers we recently crawled.
		if ok && now.Sub(peerInfo.LastCrawled) < minTimeBetweenCrawls {
			continue
		}

		// Record crawling attempt.
		r.crawlPeerInfos[addr.ID] = crawlPeerInfo{
			Addr:        addr,
			LastCrawled: now,
		}

		err := r.dialPeer(addr)
		if err != nil {
			switch err.(type) {
			case ErrMaxAttemptsToDial, ErrTooEarlyToDial, p2p.ErrCurrentlyDialingOrExistingAddress:
				r.Logger.Debug(err.Error(), "addr", addr)
			default:
				r.Logger.Debug(err.Error(), "addr", addr)
			}
			continue
		}

		peer := r.Switch.Peers().Get(addr.ID)
		if peer != nil {
			r.RequestAddrs(peer)
		}
	}
}

func (r *Reactor) cleanupCrawlPeerInfos() {
	for id, info := range r.crawlPeerInfos {
		// If we did not crawl a peer for 24 hours, it means the peer was removed
		// from the addrbook => remove
		//
		// 10000 addresses / maxGetSelection = 40 cycles to get all addresses in
		// the ideal case,
		// 40 * crawlPeerPeriod ~ 20 minutes
		if time.Since(info.LastCrawled) > 24*time.Hour {
			delete(r.crawlPeerInfos, id)
		}
	}
}

// attemptDisconnects checks if we've been with each peer long enough to disconnect.
func (r *Reactor) attemptDisconnects() {
	for _, peer := range r.Switch.Peers().Copy() {
		state := peer.ConnState()
		if state.ConnectedFor < r.config.SeedDisconnectWaitPeriod {
			continue
		}
		if peer.IsPersistent() {
			continue
		}
		r.Switch.StopPeerGracefully(peer)
	}
}

func markAddrInBookBasedOnErr(addr *na.NetAddr, book AddrBook, err error) {
	// TODO: detect more "bad peer" scenarios
	switch err.(type) {
	case p2p.ErrSwitchAuthenticationFailure:
		book.MarkBad(addr, defaultBanTime)
	default:
		book.MarkAttempt(addr)
	}
}
