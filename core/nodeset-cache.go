// Copyright 2018 The dexon-consensus-core Authors
// This file is part of the dexon-consensus-core library.
//
// The dexon-consensus-core library is free software: you can redistribute it
// and/or modify it under the terms of the GNU Lesser General Public License as
// published by the Free Software Foundation, either version 3 of the License,
// or (at your option) any later version.
//
// The dexon-consensus-core library is distributed in the hope that it will be
// useful, but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser
// General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the dexon-consensus-core library. If not, see
// <http://www.gnu.org/licenses/>.

package core

import (
	"errors"
	"sync"

	"github.com/dexon-foundation/dexon-consensus-core/core/crypto"
	"github.com/dexon-foundation/dexon-consensus-core/core/types"
)

var (
	// ErrRoundNotReady means we got nil config from governance contract.
	ErrRoundNotReady = errors.New("round is not ready")
)

type sets struct {
	nodeSet   *types.NodeSet
	notarySet []map[types.NodeID]struct{}
	dkgSet    map[types.NodeID]struct{}
}

// NodeSetCache caches node set information from governance contract.
type NodeSetCache struct {
	lock    sync.RWMutex
	gov     Governance
	rounds  map[uint64]*sets
	keyPool map[types.NodeID]*struct {
		pubKey crypto.PublicKey
		refCnt int
	}
}

// NewNodeSetCache constructs an NodeSetCache instance.
func NewNodeSetCache(gov Governance) *NodeSetCache {
	return &NodeSetCache{
		gov:    gov,
		rounds: make(map[uint64]*sets),
		keyPool: make(map[types.NodeID]*struct {
			pubKey crypto.PublicKey
			refCnt int
		}),
	}
}

// Exists checks if a node is in node set of that round.
func (cache *NodeSetCache) Exists(
	round uint64, nodeID types.NodeID) (exists bool, err error) {

	nIDs, exists := cache.get(round)
	if !exists {
		if nIDs, err = cache.update(round); err != nil {
			return
		}
	}
	_, exists = nIDs.nodeSet.IDs[nodeID]
	return
}

// GetPublicKey return public key for that node:
func (cache *NodeSetCache) GetPublicKey(
	nodeID types.NodeID) (key crypto.PublicKey, exists bool) {

	cache.lock.RLock()
	defer cache.lock.RUnlock()

	rec, exists := cache.keyPool[nodeID]
	if exists {
		key = rec.pubKey
	}
	return
}

// GetNodeSet returns IDs of nodes set of this round as map.
func (cache *NodeSetCache) GetNodeSet(
	round uint64) (nIDs *types.NodeSet, err error) {

	IDs, exists := cache.get(round)
	if !exists {
		if IDs, err = cache.update(round); err != nil {
			return
		}
	}
	nIDs = IDs.nodeSet.Clone()
	return
}

// GetNotarySet returns of notary set of this round.
func (cache *NodeSetCache) GetNotarySet(
	round uint64, chainID uint32) (map[types.NodeID]struct{}, error) {
	IDs, err := cache.getOrUpdate(round)
	if err != nil {
		return nil, err
	}
	if chainID >= uint32(len(IDs.notarySet)) {
		return nil, ErrInvalidChainID
	}
	return cache.cloneMap(IDs.notarySet[chainID]), nil
}

// GetDKGSet returns of DKG set of this round.
func (cache *NodeSetCache) GetDKGSet(
	round uint64) (map[types.NodeID]struct{}, error) {
	IDs, err := cache.getOrUpdate(round)
	if err != nil {
		return nil, err
	}
	return cache.cloneMap(IDs.dkgSet), nil
}

func (cache *NodeSetCache) cloneMap(
	nIDs map[types.NodeID]struct{}) map[types.NodeID]struct{} {
	nIDsCopy := make(map[types.NodeID]struct{}, len(nIDs))
	for k := range nIDs {
		nIDsCopy[k] = struct{}{}
	}
	return nIDsCopy
}

func (cache *NodeSetCache) getOrUpdate(round uint64) (nIDs *sets, err error) {
	s, exists := cache.get(round)
	if !exists {
		if s, err = cache.update(round); err != nil {
			return
		}
	}
	nIDs = s
	return
}

// update node set for that round.
//
// This cache would maintain 10 rounds before the updated round and purge
// rounds not in this range.
func (cache *NodeSetCache) update(
	round uint64) (nIDs *sets, err error) {

	cache.lock.Lock()
	defer cache.lock.Unlock()

	// Get the requested round from governance contract.
	keySet := cache.gov.NodeSet(round)
	if keySet == nil {
		// That round is not ready yet.
		err = ErrRoundNotReady
		return
	}
	// Cache new round.
	nodeSet := types.NewNodeSet()
	for _, key := range keySet {
		nID := types.NewNodeID(key)
		nodeSet.Add(nID)
		if rec, exists := cache.keyPool[nID]; exists {
			rec.refCnt++
		} else {
			cache.keyPool[nID] = &struct {
				pubKey crypto.PublicKey
				refCnt int
			}{key, 1}
		}
	}
	cfg := cache.gov.Configuration(round)
	crs := cache.gov.CRS(round)
	nIDs = &sets{
		nodeSet:   nodeSet,
		notarySet: make([]map[types.NodeID]struct{}, cfg.NumChains),
		dkgSet: nodeSet.GetSubSet(
			cfg.NumDKGSet, types.NewDKGSetTarget(crs)),
	}
	for i := range nIDs.notarySet {
		nIDs.notarySet[i] = nodeSet.GetSubSet(
			cfg.NumNotarySet, types.NewNotarySetTarget(crs, uint32(i)))
	}

	cache.rounds[round] = nIDs
	// Purge older rounds.
	for rID, nIDs := range cache.rounds {
		nodeSet := nIDs.nodeSet
		if round-rID <= 5 {
			continue
		}
		for nID := range nodeSet.IDs {
			rec := cache.keyPool[nID]
			if rec.refCnt--; rec.refCnt == 0 {
				delete(cache.keyPool, nID)
			}
		}
		delete(cache.rounds, rID)
	}
	return
}

func (cache *NodeSetCache) get(
	round uint64) (nIDs *sets, exists bool) {

	cache.lock.RLock()
	defer cache.lock.RUnlock()

	nIDs, exists = cache.rounds[round]
	return
}
