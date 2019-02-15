// Copyright 2018 The dexon-consensus Authors
// This file is part of the dexon-consensus library.
//
// The dexon-consensus library is free software: you can redistribute it
// and/or modify it under the terms of the GNU Lesser General Public License as
// published by the Free Software Foundation, either version 3 of the License,
// or (at your option) any later version.
//
// The dexon-consensus library is distributed in the hope that it will be
// useful, but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser
// General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the dexon-consensus library. If not, see
// <http://www.gnu.org/licenses/>.

package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/dexon-foundation/dexon-consensus/common"
	"github.com/dexon-foundation/dexon-consensus/core/crypto"
	"github.com/dexon-foundation/dexon-consensus/core/types"
	typesDKG "github.com/dexon-foundation/dexon-consensus/core/types/dkg"
	"github.com/dexon-foundation/dexon-consensus/core/utils"
)

const (
	// Count of maximum count of peers to pull votes from.
	maxPullingPeerCount = 3
	maxBlockCache       = 1000
	maxVoteCache        = 128
)

// NetworkType is the simulation network type.
type NetworkType string

// NetworkType enums.
const (
	NetworkTypeTCP      NetworkType = "tcp"
	NetworkTypeTCPLocal NetworkType = "tcp-local"
	NetworkTypeFake     NetworkType = "fake"
)

// NetworkConfig is the configuration for Network module.
type NetworkConfig struct {
	Type          NetworkType
	PeerServer    string
	PeerPort      int
	DirectLatency LatencyModel
	GossipLatency LatencyModel
	Marshaller    Marshaller
}

// PullRequest is a generic request to pull everything (ex. vote, block...).
type PullRequest struct {
	Requester types.NodeID
	Type      string
	Identity  interface{}
}

// MarshalJSON implements json.Marshaller.
func (req *PullRequest) MarshalJSON() (b []byte, err error) {
	var idAsBytes []byte
	// Make sure caller prepare correct identity for pull requests.
	switch req.Type {
	case "block":
		idAsBytes, err = json.Marshal(req.Identity.(common.Hashes))
	case "vote":
		idAsBytes, err = json.Marshal(req.Identity.(types.Position))
	case "randomness":
		idAsBytes, err = json.Marshal(req.Identity.(common.Hashes))
	default:
		err = fmt.Errorf("unknown ID type for pull request: %v", req.Type)
	}
	if err != nil {
		return
	}
	b, err = json.Marshal(&struct {
		Requester types.NodeID `json:"req"`
		Type      string       `json:"type"`
		Identity  []byte       `json:"id"`
	}{req.Requester, req.Type, idAsBytes})
	return
}

// UnmarshalJSON iumplements json.Unmarshaller.
func (req *PullRequest) UnmarshalJSON(data []byte) (err error) {
	rawReq := &struct {
		Requester types.NodeID `json:"req"`
		Type      string       `json:"type"`
		Identity  []byte       `json:"id"`
	}{}
	if err = json.Unmarshal(data, rawReq); err != nil {
		return
	}
	var ID interface{}
	switch rawReq.Type {
	case "block":
		hashes := common.Hashes{}
		if err = json.Unmarshal(rawReq.Identity, &hashes); err != nil {
			break
		}
		ID = hashes
	case "vote":
		pos := types.Position{}
		if err = json.Unmarshal(rawReq.Identity, &pos); err != nil {
			break
		}
		ID = pos
	case "randomness":
		hashes := common.Hashes{}
		if err = json.Unmarshal(rawReq.Identity, &hashes); err != nil {
			break
		}
		ID = hashes
	default:
		err = fmt.Errorf("unknown pull request type: %v", rawReq.Type)
	}
	if err != nil {
		return
	}
	req.Requester = rawReq.Requester
	req.Type = rawReq.Type
	req.Identity = ID
	return
}

// Network implements core.Network interface based on TransportClient.
type Network struct {
	ID                       types.NodeID
	config                   NetworkConfig
	ctx                      context.Context
	ctxCancel                context.CancelFunc
	trans                    TransportClient
	dMoment                  time.Time
	fromTransport            <-chan *TransportEnvelope
	toConsensus              chan interface{}
	toNode                   chan interface{}
	sentRandomnessLock       sync.Mutex
	sentRandomness           map[common.Hash]struct{}
	sentAgreementLock        sync.Mutex
	sentAgreement            map[common.Hash]struct{}
	blockCacheLock           sync.RWMutex
	blockCache               map[common.Hash]*types.Block
	voteCacheLock            sync.RWMutex
	voteCache                map[types.Position]map[types.VoteHeader]*types.Vote
	voteCacheSize            int
	votePositions            []types.Position
	randomnessCacheLock      sync.RWMutex
	randomnessCache          map[common.Hash]*types.BlockRandomnessResult
	stateModule              *State
	peers                    map[types.NodeID]struct{}
	unreceivedBlocksLock     sync.RWMutex
	unreceivedBlocks         map[common.Hash]chan<- common.Hash
	unreceivedRandomnessLock sync.RWMutex
	unreceivedRandomness     map[common.Hash]chan<- common.Hash
	cache                    *utils.NodeSetCache
	notarySetCachesLock      sync.Mutex
	notarySetCaches          map[uint64]map[types.NodeID]struct{}
	dkgSetCachesLock         sync.Mutex
	dkgSetCaches             map[uint64]map[types.NodeID]struct{}
}

// NewNetwork setup network stuffs for nodes, which provides an
// implementation of core.Network based on TransportClient.
func NewNetwork(pubKey crypto.PublicKey, config NetworkConfig) (
	n *Network) {
	// Construct basic network instance.
	n = &Network{
		ID:                   types.NewNodeID(pubKey),
		config:               config,
		toConsensus:          make(chan interface{}, 1000),
		toNode:               make(chan interface{}, 1000),
		sentRandomness:       make(map[common.Hash]struct{}),
		sentAgreement:        make(map[common.Hash]struct{}),
		blockCache:           make(map[common.Hash]*types.Block, maxBlockCache),
		randomnessCache:      make(map[common.Hash]*types.BlockRandomnessResult),
		unreceivedBlocks:     make(map[common.Hash]chan<- common.Hash),
		unreceivedRandomness: make(map[common.Hash]chan<- common.Hash),
		peers:                make(map[types.NodeID]struct{}),
		notarySetCaches:      make(map[uint64]map[types.NodeID]struct{}),
		dkgSetCaches:         make(map[uint64]map[types.NodeID]struct{}),
		voteCache: make(
			map[types.Position]map[types.VoteHeader]*types.Vote),
	}
	n.ctx, n.ctxCancel = context.WithCancel(context.Background())
	// Construct transport layer.
	switch config.Type {
	case NetworkTypeTCPLocal:
		n.trans = NewTCPTransportClient(pubKey, config.Marshaller, true)
	case NetworkTypeTCP:
		n.trans = NewTCPTransportClient(pubKey, config.Marshaller, false)
	case NetworkTypeFake:
		n.trans = NewFakeTransportClient(pubKey)
	default:
		panic(fmt.Errorf("unknown network type: %v", config.Type))
	}
	return
}

// PullBlocks implements core.Network interface.
func (n *Network) PullBlocks(hashes common.Hashes) {
	go n.pullBlocksAsync(hashes)
}

// PullVotes implements core.Network interface.
func (n *Network) PullVotes(pos types.Position) {
	go n.pullVotesAsync(pos)
}

// PullRandomness implememnts core.Network interface.
func (n *Network) PullRandomness(hashes common.Hashes) {
	go n.pullRandomnessAsync(hashes)
}

// BroadcastVote implements core.Network interface.
func (n *Network) BroadcastVote(vote *types.Vote) {
	if err := n.trans.Broadcast(n.getNotarySet(vote.Position.Round),
		n.config.DirectLatency, vote); err != nil {
		panic(err)
	}
	n.addVoteToCache(vote)
}

// BroadcastBlock implements core.Network interface.
func (n *Network) BroadcastBlock(block *types.Block) {
	// Avoid data race in fake transport.
	block = n.cloneForFake(block).(*types.Block)
	notarySet := n.getNotarySet(block.Position.Round)
	if err := n.trans.Broadcast(
		notarySet, n.config.DirectLatency, block); err != nil {
		panic(err)
	}
	if err := n.trans.Broadcast(getComplementSet(n.peers, notarySet),
		n.config.GossipLatency, block); err != nil {
		panic(err)
	}
	n.addBlockToCache(block)
}

// BroadcastAgreementResult implements core.Network interface.
func (n *Network) BroadcastAgreementResult(
	result *types.AgreementResult) {
	if !n.markAgreementResultAsSent(result.BlockHash) {
		return
	}
	// Send to DKG set first.
	dkgSet := n.getDKGSet(result.Position.Round)
	if err := n.trans.Broadcast(
		dkgSet, n.config.DirectLatency, result); err != nil {
		panic(err)
	}
	// Gossip to other nodes.
	if err := n.trans.Broadcast(getComplementSet(n.peers, dkgSet),
		n.config.GossipLatency, result); err != nil {
		panic(err)
	}
}

// BroadcastRandomnessResult implements core.Network interface.
func (n *Network) BroadcastRandomnessResult(
	randResult *types.BlockRandomnessResult) {
	if !n.markRandomnessResultAsSent(randResult.BlockHash) {
		return
	}
	// Send to notary set first.
	notarySet := n.getNotarySet(randResult.Position.Round)
	if err := n.trans.Broadcast(
		notarySet, n.config.DirectLatency, randResult); err != nil {
		panic(err)
	}
	// Gossip to other nodes.
	if err := n.trans.Broadcast(getComplementSet(n.peers, notarySet),
		n.config.GossipLatency, randResult); err != nil {
		panic(err)
	}
	n.addRandomnessToCache(randResult)
}

// SendDKGPrivateShare implements core.Network interface.
func (n *Network) SendDKGPrivateShare(
	recv crypto.PublicKey, prvShare *typesDKG.PrivateShare) {
	n.send(types.NewNodeID(recv), prvShare)
}

// BroadcastDKGPrivateShare implements core.Network interface.
func (n *Network) BroadcastDKGPrivateShare(
	prvShare *typesDKG.PrivateShare) {
	if err := n.trans.Broadcast(n.getDKGSet(prvShare.Round),
		n.config.DirectLatency, prvShare); err != nil {
		panic(err)
	}
}

// BroadcastDKGPartialSignature implements core.Network interface.
func (n *Network) BroadcastDKGPartialSignature(
	psig *typesDKG.PartialSignature) {
	if err := n.trans.Broadcast(
		n.getDKGSet(psig.Round), n.config.DirectLatency, psig); err != nil {
		panic(err)
	}
}

// ReceiveChan implements core.Network interface.
func (n *Network) ReceiveChan() <-chan interface{} {
	return n.toConsensus
}

// Setup transport layer.
func (n *Network) Setup(serverEndpoint interface{}) (err error) {
	// Join the p2p network.
	switch n.config.Type {
	case NetworkTypeTCP, NetworkTypeTCPLocal:
		addr := net.JoinHostPort(
			n.config.PeerServer, strconv.Itoa(n.config.PeerPort))
		n.fromTransport, err = n.trans.Join(addr)
	case NetworkTypeFake:
		n.fromTransport, err = n.trans.Join(serverEndpoint)
	default:
		err = fmt.Errorf("unknown network type: %v", n.config.Type)
	}
	if err != nil {
		return
	}
	peerKeys := n.trans.Peers()
	for _, k := range peerKeys {
		n.peers[types.NewNodeID(k)] = struct{}{}
	}
	return
}

func (n *Network) dispatchMsg(e *TransportEnvelope) {
	msg := n.cloneForFake(e.Msg)
	switch v := msg.(type) {
	case *types.Block:
		n.addBlockToCache(v)
		// Notify pulling routine about the newly arrived block.
		func() {
			n.unreceivedBlocksLock.Lock()
			defer n.unreceivedBlocksLock.Unlock()
			if ch, exists := n.unreceivedBlocks[v.Hash]; exists {
				ch <- v.Hash
			}
			delete(n.unreceivedBlocks, v.Hash)
		}()
		n.toConsensus <- v
	case *types.Vote:
		// Add this vote to cache.
		n.addVoteToCache(v)
		n.toConsensus <- v
	case *types.AgreementResult, *types.BlockRandomnessResult,
		*typesDKG.PrivateShare, *typesDKG.PartialSignature:
		n.toConsensus <- v
	case packedStateChanges:
		if n.stateModule == nil {
			panic(errors.New(
				"receive packed state change request without state attached"))
		}
		if err := n.stateModule.AddRequestsFromOthers([]byte(v)); err != nil {
			panic(err)
		}
	case *PullRequest:
		go n.handlePullRequest(v)
	default:
		n.toNode <- v
	}
}

func (n *Network) handlePullRequest(req *PullRequest) {
	switch req.Type {
	case "block":
		hashes := req.Identity.(common.Hashes)
		func() {
			n.blockCacheLock.Lock()
			defer n.blockCacheLock.Unlock()
		All:
			for _, h := range hashes {
				b, exists := n.blockCache[h]
				if !exists {
					continue
				}
				select {
				case <-n.ctx.Done():
					break All
				default:
				}
				n.send(req.Requester, b)
			}
		}()
	case "vote":
		pos := req.Identity.(types.Position)
		func() {
			n.voteCacheLock.Lock()
			defer n.voteCacheLock.Unlock()
			if votes, exists := n.voteCache[pos]; exists {
				for _, v := range votes {
					n.send(req.Requester, v)
				}
			}
		}()
	case "randomness":
		hashes := req.Identity.(common.Hashes)
		func() {
			n.randomnessCacheLock.Lock()
			defer n.randomnessCacheLock.Unlock()
		All:
			for _, h := range hashes {
				r, exists := n.randomnessCache[h]
				if !exists {
					continue
				}
				select {
				case <-n.ctx.Done():
					break All
				default:
				}
				n.send(req.Requester, r)
			}
		}()
	default:
		panic(fmt.Errorf("unknown type of pull request: %v", req.Type))
	}
}

// Run the main loop.
func (n *Network) Run() {
Loop:
	for {
		select {
		case <-n.ctx.Done():
			break Loop
		default:
		}
		select {
		case <-n.ctx.Done():
			break Loop
		case e, ok := <-n.fromTransport:
			if !ok {
				break Loop
			}
			go n.dispatchMsg(e)
		}
	}
}

// Close stops the network.
func (n *Network) Close() (err error) {
	n.ctxCancel()
	close(n.toConsensus)
	n.toConsensus = nil
	close(n.toNode)
	n.toNode = nil
	if err = n.trans.Close(); err != nil {
		return
	}
	return
}

// Report exports 'Report' method of TransportClient.
func (n *Network) Report(msg interface{}) error {
	return n.trans.Report(msg)
}

// Broadcast a message to all peers.
func (n *Network) Broadcast(msg interface{}) error {
	return n.trans.Broadcast(n.peers, &FixedLatencyModel{}, msg)
}

// Peers exports 'Peers' method of Transport.
func (n *Network) Peers() []crypto.PublicKey {
	return n.trans.Peers()
}

// DMoment exports 'DMoment' method of Transport.
func (n *Network) DMoment() time.Time {
	return n.trans.DMoment()
}

// ReceiveChanForNode returns a channel for messages not handled by
// core.Consensus.
func (n *Network) ReceiveChanForNode() <-chan interface{} {
	return n.toNode
}

// addStateModule attaches a State instance to this network.
func (n *Network) addStateModule(s *State) {
	// This variable should be attached before run, no lock to protect it.
	n.stateModule = s
}

// AddNodeSetCache attaches an utils.NodeSetCache to this module. Once attached
// The behavior of Broadcast-X methods would be switched to broadcast to correct
// set of peers, instead of all peers.
func (n *Network) AddNodeSetCache(cache *utils.NodeSetCache) {
	// This variable should be attached before run, no lock to protect it.
	n.cache = cache
}

func (n *Network) pullBlocksAsync(hashes common.Hashes) {
	// Setup notification channels for each block hash.
	notYetReceived := make(map[common.Hash]struct{})
	ch := make(chan common.Hash, len(hashes))
	func() {
		n.unreceivedBlocksLock.Lock()
		defer n.unreceivedBlocksLock.Unlock()
		for _, h := range hashes {
			if _, exists := n.unreceivedBlocks[h]; exists {
				continue
			}
			n.unreceivedBlocks[h] = ch
			notYetReceived[h] = struct{}{}
		}
	}()
	req := &PullRequest{
		Requester: n.ID,
		Type:      "block",
		Identity:  hashes,
	}
	// Randomly pick peers to send pull requests.
Loop:
	for nID := range n.peers {
		if nID == n.ID {
			continue
		}
		n.send(nID, req)
		select {
		case <-n.ctx.Done():
			break Loop
		case <-time.After(2 * n.config.DirectLatency.Delay()):
			// Consume everything in the notification channel.
			for {
				select {
				case h, ok := <-ch:
					if !ok {
						// This network module is closed.
						break Loop
					}
					delete(notYetReceived, h)
					if len(notYetReceived) == 0 {
						break Loop
					}
				default:
					continue Loop
				}
			}
		}
	}
}

func (n *Network) pullVotesAsync(pos types.Position) {
	// Randomly pick several peers to pull votes from.
	req := &PullRequest{
		Requester: n.ID,
		Type:      "vote",
		Identity:  pos,
	}
	// Get corresponding notary set.
	notarySet := n.getNotarySet(pos.Round)
	// Randomly select one peer from notary set and send a pull request.
	sentCount := 0
	for nID := range notarySet {
		n.send(nID, req)
		sentCount++
		if sentCount >= maxPullingPeerCount {
			break
		}
	}
}

func (n *Network) pullRandomnessAsync(hashes common.Hashes) {
	// Setup notification channels for each block hash.
	notYetReceived := make(map[common.Hash]struct{})
	ch := make(chan common.Hash, len(hashes))
	func() {
		n.unreceivedRandomnessLock.Lock()
		defer n.unreceivedRandomnessLock.Unlock()
		for _, h := range hashes {
			if _, exists := n.unreceivedRandomness[h]; exists {
				continue
			}
			n.unreceivedRandomness[h] = ch
			notYetReceived[h] = struct{}{}
		}
	}()
	req := &PullRequest{
		Requester: n.ID,
		Type:      "randomness",
		Identity:  hashes,
	}
	// Randomly pick peers to send pull requests.
Loop:
	for nID := range n.peers {
		if nID == n.ID {
			continue
		}
		n.send(nID, req)
		select {
		case <-n.ctx.Done():
			break Loop
		case <-time.After(2 * n.config.DirectLatency.Delay()):
			// Consume everything in the notification channel.
			for {
				select {
				case h, ok := <-ch:
					if !ok {
						// This network module is closed.
						break Loop
					}
					delete(notYetReceived, h)
					if len(notYetReceived) == 0 {
						break Loop
					}
				default:
					continue Loop
				}
			}
		}
	}
}

func (n *Network) addBlockToCache(b *types.Block) {
	n.blockCacheLock.Lock()
	defer n.blockCacheLock.Unlock()
	if len(n.blockCache) > maxBlockCache {
		// Randomly purge one block from cache.
		for k := range n.blockCache {
			delete(n.blockCache, k)
			break
		}
	}
	n.blockCache[b.Hash] = b.Clone()
}

func (n *Network) addVoteToCache(v *types.Vote) {
	n.voteCacheLock.Lock()
	defer n.voteCacheLock.Unlock()
	if n.voteCacheSize >= maxVoteCache {
		pos := n.votePositions[0]
		n.voteCacheSize -= len(n.voteCache[pos])
		delete(n.voteCache, pos)
		n.votePositions = n.votePositions[1:]
	}
	if _, exists := n.voteCache[v.Position]; !exists {
		n.votePositions = append(n.votePositions, v.Position)
		n.voteCache[v.Position] =
			make(map[types.VoteHeader]*types.Vote)
	}
	if _, exists := n.voteCache[v.Position][v.VoteHeader]; exists {
		return
	}
	n.voteCache[v.Position][v.VoteHeader] = v
	n.voteCacheSize++
}

func (n *Network) addRandomnessToCache(rand *types.BlockRandomnessResult) {
	n.randomnessCacheLock.Lock()
	defer n.randomnessCacheLock.Unlock()
	if len(n.randomnessCache) > 1000 {
		// Randomly purge one randomness from cache.
		for k := range n.randomnessCache {
			delete(n.randomnessCache, k)
			break
		}
	}
	n.randomnessCache[rand.BlockHash] = rand
}

func (n *Network) markAgreementResultAsSent(blockHash common.Hash) bool {
	n.sentAgreementLock.Lock()
	defer n.sentAgreementLock.Unlock()
	if _, exist := n.sentAgreement[blockHash]; exist {
		return false
	}
	if len(n.sentAgreement) > 1000 {
		// Randomly drop one entry.
		for k := range n.sentAgreement {
			delete(n.sentAgreement, k)
			break
		}
	}
	n.sentAgreement[blockHash] = struct{}{}
	return true
}

func (n *Network) markRandomnessResultAsSent(blockHash common.Hash) bool {
	n.sentRandomnessLock.Lock()
	defer n.sentRandomnessLock.Unlock()
	if _, exist := n.sentRandomness[blockHash]; exist {
		return false
	}
	if len(n.sentRandomness) > 1000 {
		// Randomly drop one entry.
		for k := range n.sentRandomness {
			delete(n.sentRandomness, k)
			break
		}
	}
	n.sentRandomness[blockHash] = struct{}{}
	return true
}

func (n *Network) cloneForFake(v interface{}) interface{} {
	if n.config.Type != NetworkTypeFake {
		return v
	}
	switch val := v.(type) {
	case *types.Block:
		return val.Clone()
	case *types.BlockRandomnessResult:
		// Perform deep copy for randomness result.
		return cloneBlockRandomnessResult(val)
	}
	return v
}

// getNotarySet gets notary set for that (round, chain) from cache.
func (n *Network) getNotarySet(round uint64) map[types.NodeID]struct{} {
	if n.cache == nil {
		// Default behavior is to broadcast to all peers, which makes it easier
		// to be used in simple test cases.
		return n.peers
	}
	n.notarySetCachesLock.Lock()
	defer n.notarySetCachesLock.Unlock()
	set, exists := n.notarySetCaches[round]
	if !exists {
		var err error
		set, err = n.cache.GetNotarySet(round, 0)
		if err != nil {
			panic(err)
		}
		n.notarySetCaches[round] = set
	}
	return set
}

// getDKGSet gets DKG set for that round from cache.
func (n *Network) getDKGSet(round uint64) map[types.NodeID]struct{} {
	if n.cache == nil {
		// Default behavior is to broadcast to all peers, which makes it easier
		// to be used in simple test cases.
		return n.peers
	}
	n.dkgSetCachesLock.Lock()
	defer n.dkgSetCachesLock.Unlock()
	set, exists := n.dkgSetCaches[round]
	if !exists {
		var err error
		set, err = n.cache.GetDKGSet(round)
		if err != nil {
			panic(err)
		}
		n.dkgSetCaches[round] = set
	}
	return set
}

func (n *Network) send(endpoint types.NodeID, msg interface{}) {
	go func() {
		time.Sleep(n.config.DirectLatency.Delay())
		if err := n.trans.Send(endpoint, msg); err != nil {
			panic(err)
		}
	}()
}
