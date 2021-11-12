package peering

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/migalabs/armiarma/src/base"
	"github.com/migalabs/armiarma/src/db"
	"github.com/migalabs/armiarma/src/db/utils"
	"github.com/migalabs/armiarma/src/hosts"

	log "github.com/sirupsen/logrus"
)

var (
	PruningStrategyName = "PRUNING"
	DefaultDelay        = 24 * time.Hour   // hours of dealy after each negative attempt with delay
	MinIterTime         = 10 * time.Second // Minimum time that has to pass before iterating again
	ConnEventBuffSize   = 10
)

type PruningOpts struct {
	AggregatedDelay time.Duration
	LogOpts         base.LogOpts
}

// Pruning Strategy is a Peering Strategy that applies penalties to peers that haven't show activity when attempting to connect them.
// Combined with the Deprecated flag in the db.Peer struct, it produces more accourated metrics when exporting, pruning peers that are no longer active.
type PruningStrategy struct {
	*base.Base
	strategyType string
	PeerStore    *db.PeerStore
	// Delay unit time that gets applied to those slashed peers when reporting inactivity errors when activly connecting
	AggregatedDelay time.Duration
	// Peer Stream and Return ConnectionStatus channels (communication between modules)
	// both empty by default (need for initialization)

	peerStreamChan chan db.Peer
	nextPeerChan   chan struct{}
	connAttemptNot chan ConnectionAttemptStatus
	connNot        chan hosts.ConnectionStatus
	disconnNot     chan hosts.DisconnectionStatus

	// List of peers sorted by the amount of time thatwe have to wait
	PeerQueue PeerQueue
	/*
		// TODO: Choose the necessary parameters for the pruning
		FilterDigest beacon.ForkDigest `ask:"--filter-digest" help:"Only connect when the peer is known to have the given fork digest in ENR. Or connect to any if not specified."`
		FilterPort   int               `ask:"--filter-port" help:"Only connect to peers that has the given port advertised on the ENR."`
		Filtering    bool              `changed:"filter-digest"`
	*/
}

// NewPruningStrategy
// * Pruning strategy constructor, that will offer a db.Peer stream for the
// * peering service. The povided db.Peer stream are ready to connect.
// @param ctx: parent context
// @param peerstore: db.PeerStore
// @param opts: base and logging option
// @return peering strategy interface with the prunning service:
// @return error:
func NewPruningStrategy(ctx context.Context, peerstore *db.PeerStore, opts PruningOpts) (PruningStrategy, error) {
	// TODO: cancel is still not implemented in the BaseCreation
	pruningCtx, _ := context.WithCancel(ctx)
	opts.LogOpts.ModName = PruningStrategyName
	b, err := base.NewBase(
		base.WithContext(pruningCtx),
		base.WithLogger(opts.LogOpts),
	)
	if err != nil {
		return PruningStrategy{}, err
	}
	// Generate the ConnStatus notification channel
	// TODO: consider making the ConnStatus channel larger
	pr := PruningStrategy{
		Base:           b,
		strategyType:   PruningStrategyName,
		PeerStore:      peerstore,
		PeerQueue:      NewPeerQueue(),
		peerStreamChan: make(chan db.Peer, ConnEventBuffSize),
		nextPeerChan:   make(chan struct{}, ConnEventBuffSize),
		connAttemptNot: make(chan ConnectionAttemptStatus, ConnEventBuffSize),
		connNot:        make(chan hosts.ConnectionStatus, ConnEventBuffSize),
		disconnNot:     make(chan hosts.DisconnectionStatus, ConnEventBuffSize),
	}
	return pr, nil
}

// Type
// * Returns the strategy type that has been set
// @return string with the name of the pruning strategy
func (c PruningStrategy) Type() string {
	return c.strategyType
}

// Run
// * initializes the db.Peer stream on the returning db.Peer chan
// * stores locally an auxiliary map wuth an array that will keep
// * track of the next connection time.
// @return db.Peer channel with the next peer to connect
func (c *PruningStrategy) Run() chan db.Peer {
	// start go routine that will notify of the full peerstore iteration and notifies it to the main strategy loop
	go c.peerstoreIterator()
	return c.peerStreamChan
}

// peerstoreIterator
// * Private function that is in charge of iterating through the peerstore,
// * receive connections/disconnectios, and fetch info comming from the peering service into the db
// * Main interaction of the Peering Service with the DB
func (c *PruningStrategy) peerstoreIterator() {
	// get Ctx of the pruning module
	modCtx := c.Ctx()
	// get the peer list from the peerstore
	err := c.PeerQueue.UpdatePeerListFromPeerStore(c.PeerStore)
	if err != nil {
		c.Log.Error(err)
	}
	peerCounter := 0
	peerListLen := c.PeerQueue.Len()
	validIterTimer := time.NewTimer(MinIterTime)
	iterStartTime := time.Now()
	nextIterFlag := false
	for {
		select {
		// Receive the notification of sending the next peer
		case <-c.nextPeerChan:
			if peerListLen > 0 {
				c.Log.Debug("prepare next peer for pushing it into peer stream")
				// read info about next peer
				nextPeer := c.PeerQueue.PeerList[peerCounter]
				// check if the node is ready for connection
				if nextPeer.IsReadyForConnection() {
					pinfo, err := c.PeerStore.GetPeerData(nextPeer.PeerID)
					if err != nil {
						log.Warn(err)
						pinfo = db.NewPeer(nextPeer.PeerID)
					}
					// compose all the detailed info for the given peer
					// Generating New peer to fetch info
					npeer := db.NewPeer(nextPeer.PeerID)
					peerEnr := pinfo.GetBlockchainNode()
					if peerEnr != nil {
						npeer.NodeId = peerEnr.ID().String()
						// TODO:
						npeer.Ip = peerEnr.IP().String()
					}
					pID, _ := peer.Decode(nextPeer.PeerID)
					if err != nil {
						c.Log.Errorf("error decoding PeerID string into peer.ID %s", err.Error())
					}
					npeer.PeerId = pID.String()
					k, _ := pID.ExtractPublicKey()
					pubk, _ := k.Raw()
					npeer.Pubkey = hex.EncodeToString(pubk)
					npeer.MAddrs = pinfo.MAddrs
					// Update metadata of peer
					c.PeerStore.StoreOrUpdatePeer(npeer)

					// Send next peer to the peering service
					c.Log.Debugf("pushing next peer %s into peer stream", pinfo.PeerId)
					c.peerStreamChan <- pinfo

					// increment peerCounter to see if we finished iterating the peerstore
					peerCounter++
				} else {
					c.Log.Debug("next peers has to wait to be connected")
					c.NextPeer()
					nextIterFlag = true
				}
			} else {
				c.Log.Warn("empty peerstore")
				// Recreate the call of the nextPeer that the iterator just used
				c.NextPeer()

			}
			if nextIterFlag || peerCounter >= peerListLen {
				// time to update the PeerList
				iterTime := time.Since(iterStartTime)
				c.Log.Debug("peerstore iteration of ", peerCounter, " peers, done in ", iterTime)
				c.PeerStore.NewPeerstoreIteration(iterTime)
				// check if the minIterTime has been
				<-validIterTimer.C

				// reset values
				// get the peer list from the peerstore
				err := c.PeerQueue.UpdatePeerListFromPeerStore(c.PeerStore)
				if err != nil {
					c.Log.Error(err)
				}
				peerListLen = c.PeerQueue.Len()
				c.Log.Debugf("got new peer list with %d", peerListLen)
				validIterTimer = time.NewTimer(MinIterTime)
				peerCounter = 0
				nextIterFlag = false
			}

		// Receive the status of the peer that got connected to the crawler
		case connAttemtpStatus := <-c.connAttemptNot:
			c.Log.Debugf("new connection attempt has been received from peer %s", connAttemtpStatus.Peer.PeerId)
			c.PeerStore.StoreOrUpdatePeer(connAttemtpStatus.Peer)
			if connAttemtpStatus.Successful {
				c.Log.Debugf("adding success connection to peer %s", connAttemtpStatus.Peer.PeerId)
				c.PeerStore.AddNewPosConnectionAttempt(connAttemtpStatus.Peer.PeerId)
				// Update Pruning Metadata
				var p *PrunedPeer
				p, ok := c.PeerQueue.GetPeer(connAttemtpStatus.Peer.PeerId)
				if !ok {
					p := NewPrunedPeer(connAttemtpStatus.Peer.PeerId, time.Now())
					c.PeerQueue.AddPeer(p)
				} else {
					p.NextConnection = time.Now().Unix()
					p.DeprecationTime = (time.Time{}).Unix()
				}
			} else {
				c.Log.Debugf("adding negative connection to peer %s", connAttemtpStatus.Peer.PeerId)
				// Update Pruning Metadata
				var p *PrunedPeer
				p, ok := c.PeerQueue.GetPeer(connAttemtpStatus.Peer.PeerId)
				if !ok {
					p = NewPrunedPeer(connAttemtpStatus.Peer.PeerId, time.Now())
				}
				c.RecErrorHandler(p, connAttemtpStatus.RecError.Error())
			}

		// Receive the notification of a that got disconnected from the crawler
		case connStat := <-c.connNot:
			c.Log.Debugf("new connection has been received from peer %s", connStat.Peer.PeerId)
			c.PeerStore.StoreOrUpdatePeer(connStat.Peer)

		// Receive the notification of a that got disconnected from the crawler
		case disconnStat := <-c.disconnNot:
			c.Log.Debugf("new disconnection has been received from peer %s", disconnStat.Peer.PeerId)
			c.PeerStore.StoreOrUpdatePeer(disconnStat.Peer)

		// detect if the context has been shut down to end the go routine
		case <-modCtx.Done():
			c.Log.Debug("closing peerstore iterator")
		}
	}
}

// ClosePeerStream
// * Closes in a controled secuence the module related go routines and channels
// * Ending with the Base.Ctx cancelation
func (c *PruningStrategy) Close() {
	c.Log.Infof("closing pruning strategy")
	// close the involved channels
	close(c.peerStreamChan)
	close(c.nextPeerChan)
	close(c.connNot)
	close(c.disconnNot)
	// shutdown the base context of the pruning
	c.Cancel()
}

// NextPeer
// * Notifies the peerstore iterator that a new peer has been requested
// * After it, the peerstore iteratow will put the new peer in the PeerStreamChan
func (c *PruningStrategy) NextPeer() {
	c.Log.Debug("next peer has been requested")
	c.nextPeerChan <- struct{}{}
}

// NewConnectionAttempt
// * Notifies the peerstore iterator that a new ConnStatus has been received
// * After it, the peerstore iteratow will aggregate the extra info
func (c *PruningStrategy) NewConnectionAttempt(connAttStat ConnectionAttemptStatus) {
	c.Log.Debug("next connection has been received")
	c.connAttemptNot <- connAttStat
}

// NewConnection
// * Notifies the peerstore iterator that a new Connection has been received
// * I puts the connection metadata in the connNot channel to let the select
// * loop all the metadata of the received connection
func (c *PruningStrategy) NewConnection(connStat hosts.ConnectionStatus) {
	c.Log.Debug("next connection has been received")
	c.connNot <- connStat
}

// NewDisconnection
// * Notifies the peerstore iterator that a new disconnection has been received
// * I puts the disconnection metadata in the disconnNot channel to let the select
// * loop all the metadata of the received disconnection
func (c *PruningStrategy) NewDisconnection(disconnStat hosts.DisconnectionStatus) {
	c.Log.Debug("next connection has been received")
	c.disconnNot <- disconnStat
}

// peeringWorker
// @params
// @return
// TODO: Still not sure if we need workers for iterating the peerstore
func peeringWorker(ctx context.Context, ps *db.PeerStore, peerChan chan string) {

}

// RecErrorHandler
// * function that selects actuation method for each of the possible errors while actively dialing peers
// @params peerID in string format, recorded error in string format
func (c *PruningStrategy) RecErrorHandler(pe *PrunedPeer, rec_err string) {
	var fn func(p *db.Peer)
	// current time
	t := time.Now()
	var depTime time.Time
	switch utils.FilterError(rec_err) {
	case "Connection reset by peer":
		fn = func(p *db.Peer) {
			p.AddNegConnAtt(false) // hardcoded to no Peer is still there, alive
		}
	case "i/o timeout":
		deprecable := pe.Deprecable(t)
		if !deprecable {
			depTime = t.Add(DefaultDelay)
		}
		pe.AggregateDelay()
		fn = func(p *db.Peer) {
			p.AddNegConnAtt(deprecable)
		}
	case "dial to self attempted":
		pe.AggregateDelay()
		// we tried to peer ourselfs! deprecate the peer
		fn = func(p *db.Peer) {
			p.AddNegConnAtt(true) // Deprecate directly
		}
	case "dial backoff":
		fn = func(p *db.Peer) {
			p.AddNegConnAtt(false) // hardcoded to no Peer is still there, alive
		}
	case "connection refused":
		fn = func(p *db.Peer) {
			p.AddNegConnAtt(false) // hardcoded to no Peer is still there, alive
		}
	case "no route to host":
		pe.AggregateDelay()
		fn = func(p *db.Peer) {
			p.AddNegConnAtt(true) // Deprecate directly
		}
	case "unreachable network":
		pe.AggregateDelay()
		fn = func(p *db.Peer) {
			p.AddNegConnAtt(true) // Deprecate directly
		}
	case "peer id mismatch, peer dissmissed":
		// TODO: try to recover the peers from the peerID using Decode
		pe.AggregateDelay()
		// deprecate old peer and generate a new one
		// trim new peerID from error message
		np := strings.Split(rec_err, "key matches ")[1]
		np = strings.Replace(np, ")", "", -1)
		//newPeerID := peer.Decode(np)
		//f.WriteString(fmt.Sprintf("%s shifted to %s\n", pe.String(), newPeerID))
		// Generate a new Peer with the addrs of the previous one and the ID suggested at the
		log.Infof("deprecating peer %s, but adding possible new peer %s", pe, np)
		/*
			pubkey, err := newPeerID.ExtractPublicKey()
			if err != nil {
				fmt.Println("error obtainign pubkey from peerid", err)
			} else {
				fmt.Println("pubkey success, obtained")
			}
			TODO: -Fix empty pubkey originated from adding the new PeerID to the Peerstore
					-Apparently the len of the new peer doesn't have the same one as the previous one
			// Generate new Addrs for the possible new discovered peer
			addrs := c.Store.Addrs(pe)
			enr := c.Store.LatestENR(pe)
			fmt.Println("len old", len(pe.String()), "len new", len(newPeerID.String()))
			fmt.Println(rec_err)
			// Info about the peer should be added to the metrics
			// *** Carefull - problems with reading the pubkey ***
			//newP := db.NewPeer(newPeerID.String())
			//c.PeerStore.Store(newPeerID.String(), newP)
			_, _ = c.Store.UpdateENRMaybe(newPeerID, enr)
			c.Store.AddAddrs(newPeerID, addrs, time.Duration(48)*time.Hour)
		*/
		fn = func(p *db.Peer) {
			p.AddNegConnAtt(true)
		}
	default:
		deprecable := pe.Deprecable(t)
		if !deprecable {
			depTime = t.Add(DefaultDelay)
		}
		pe.AggregateDelay()
		fn = func(p *db.Peer) {
			p.AddNegConnAtt(deprecable)
		}
	}
	// Add the deprecation time for the Puned Peer
	pe.SetDeprecationDate(depTime)
	c.PeerStore.AddNewNegConnectionAttempt(pe.PeerID, rec_err, fn)
}

// Extra Prunning methods

// PeerQueue
// * Auxiliar peer array and map list to keep the list of peers sorted
// * by cooner connection time, and still able to modify in a short time
// * the values of each peer
type PeerQueue struct {
	PeerList []*PrunedPeer
	PeerMap  map[string]*PrunedPeer
}

// NewPeerQueue
// * Constructor of a NewPeerQueue
// @return new PeerQueue
func NewPeerQueue() PeerQueue {
	pq := PeerQueue{
		PeerList: make([]*PrunedPeer, 0),
		PeerMap:  make(map[string]*PrunedPeer),
	}
	return pq
}

// IsPeerAlready
// * Check whether a peer is already in the Queue
// @params peerID: string of the peerID that we want to find
// @return true is peer is already, false if not
func (c *PeerQueue) IsPeerAlready(peerID string) bool {
	_, ok := c.PeerMap[peerID]
	return ok
}

// IsPeerAlready
// * Check whether a peer is already in the Queue
// @params peerID: string of the peerID that we want to find
// @return true is peer is already, false if not
func (c *PeerQueue) AddPeer(pPeer *PrunedPeer) {
	// append new item at the begining of the array
	c.PeerList = append([]*PrunedPeer{pPeer}, c.PeerList...)
	c.PeerMap[pPeer.PeerID] = pPeer
}

// GetPeer
// * retrieves the info of the peer requested from args
// @params peerID: string of the peerID that we want to find
// @return pointer to prunned peer, bool, true if exists, false if doesn't
func (c *PeerQueue) GetPeer(peerID string) (*PrunedPeer, bool) {
	p, ok := c.PeerMap[peerID]
	return p, ok
}

// SortPeerList
// * Sort the PeerQueue array leaving at the begining the peers
// * with the shorter next peer connection
func (c *PeerQueue) SortPeerList() {
	sort.Sort(c)
}

// SORTING METHODS FOR PeerQueue

// Swap is part of sort.Interface.
func (c *PeerQueue) Swap(i, j int) {
	c.PeerList[i], c.PeerList[j] = c.PeerList[j], c.PeerList[i]
}

// Less is part of sort.Interface. We use c.PeerList.NextConnection as the value to sort by
func (c PeerQueue) Less(i, j int) bool {
	return c.PeerList[i].NextConnection < c.PeerList[j].NextConnection
}

// Len is part of sort.Interface. We use the peer list to get the length of the array
func (c PeerQueue) Len() int {
	return len(c.PeerList)
}

//
func (c *PeerQueue) UpdatePeerListFromPeerStore(peerstore *db.PeerStore) error {
	// Get the list of peers from the peerstore
	peerList := peerstore.GetPeerList()
	totcnt := 0
	new := 0
	// Fill the PeerQueue.PeerList with the missing peers from the
	for _, peerID := range peerList {
		totcnt++
		if !c.IsPeerAlready(peerID.String()) {
			new++
			// Peer was not in the list of peers
			pInfo, err := peerstore.GetPeerData(peerID.String())
			if err != nil {
				return fmt.Errorf("unable import peer to PeerQueue. %s", err.Error())
			}
			// check the last connAttempt of the peer
			lattempt, err := pInfo.LastNegAttempt()
			var nextConn int64
			if err != nil {
				// there arent negative connections, add current time in Unix() for NextConnection
				nextConn = time.Now().Unix()
			} else {
				// there is a lastNegAtt
				nextConn = lattempt.Add(DefaultDelay).Unix()
			}
			newPeer := &PrunedPeer{
				PeerID:         peerID.String(),
				NextConnection: nextConn,
			}
			// add the new item to the list
			c.AddPeer(newPeer)
		}
	}
	c.SortPeerList()
	return nil
}

// TODO: think about includint a sync.RWMutex in case we upgrade to workers
type PrunedPeer struct {
	PeerID          string
	NextConnection  int64
	DeprecationTime int64
}

func NewPrunedPeer(peerID string, lastConnAtt time.Time) *PrunedPeer {
	var nextConn int64
	if lastConnAtt != (time.Time{}) {
		nextConn = time.Now().Unix()
	} else {
		nextConn = lastConnAtt.Add(DefaultDelay).Unix()
	}
	pp := PrunedPeer{
		PeerID:          peerID,
		NextConnection:  nextConn,
		DeprecationTime: (time.Time{}).Unix(),
	}
	return &pp
}

// IsReadyForConnection
// * This method evaluates if the given peer is ready to be connected.
// * This means that the current time has exceeded the
// * lastAttempt + waiting time, so we have already waited enough
// @return True of False if we are in position to connect or not
func (c *PrunedPeer) IsReadyForConnection() bool {
	now := time.Now().Unix()
	return now >= c.NextConnection
}

func (c *PrunedPeer) AggregateDelay() {
	c.NextConnection += int64(DefaultDelay.Seconds())
}

func (c *PrunedPeer) SetDeprecationDate(t time.Time) {
	c.DeprecationTime = t.Unix()
}

// only return true if the Deprecation time is different than 0 and current time is same or bigger than the specified time
func (c *PrunedPeer) Deprecable(t time.Time) bool {
	if c.DeprecationTime != 0 {
		return t.Unix() >= c.DeprecationTime
	}
	return false
}
