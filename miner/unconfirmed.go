// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package miner

import (
	"container/ring"
	"fmt"
	"sync"

	"github.com/BerithFoundation/berith-chain/common"
	"github.com/BerithFoundation/berith-chain/core/types"
	"github.com/BerithFoundation/berith-chain/log"
)

// chainRetriever is used by the unconfirmed block set to verify whether a previously
// mined block is part of the canonical chain or not.
type chainRetriever interface {
	// GetHeaderByNumber retrieves the canonical header associated with a block number.
	GetHeaderByNumber(number uint64) *types.Header

	// GetBlockByNumber retrieves the canonical block associated with a block number.
	GetBlockByNumber(number uint64) *types.Block
}

// unconfirmedBlock is a small collection of metadata about a locally mined block
// that is placed into a unconfirmed set for canonical chain inclusion tracking.
type unconfirmedBlock struct {
	index uint64
	hash  common.Hash
}

// unconfirmedBlocks implements a data structure to maintain locally mined blocks
// have not yet reached enough maturity to guarantee chain inclusion. It is
// used by the miner to provide logs to the user when a previously mined block
// has a high enough guarantee to not be reorged out of the canonical chain.
//
// unconfirmedBlocks는 체인 포함을 보장하기 위해 아직 충분한 성숙도에 도달하지 않은
// 로컬 마이닝 블록을 유지하기 위한 데이터 구조를 구현한다.
// 이전에 채굴된 블록이 정규 체인에서 재기록되지 않을 만큼 충분히 높은 개런티를 가질 때
// 마이너가 로그를 제공하기 위해 사용한다.
type unconfirmedBlocks struct {
	// Blockchain to verify canonical status through
	// 표준 상태를 확인할 수 있는 블록체인
	chain chainRetriever

	// Depth after which to discard previous blocks
	// 이전 블록을 폐기할 깊이 == 7
	depth uint

	// Block infos to allow canonical chain cross checks
	// 표준 체인 크로스체킹을 허용하기 위한 블록 정보
	blocks *ring.Ring

	lock sync.RWMutex // Protects the fields from concurrent access
}

// newUnconfirmedBlocks returns new data structure to track currently unconfirmed blocks.
func newUnconfirmedBlocks(chain chainRetriever, depth uint) *unconfirmedBlocks {
	return &unconfirmedBlocks{
		chain: chain,
		depth: depth,
	}
}

// Insert adds a new block to the set of unconfirmed ones.
func (set *unconfirmedBlocks) Insert(index uint64, hash common.Hash) {
	fmt.Println("unconfirmedBlocks.Insert() 호출")
	// If a new block was mined locally, shift out any old enough blocks
	set.Shift(index)

	// Create the new item as its own ring
	// 1칸짜리 Ring 자료구조 생성
	item := ring.New(1)
	item.Value = &unconfirmedBlock{
		index: index,
		hash:  hash,
	}
	// Set as the initial ring or append to the end
	set.lock.Lock()
	defer set.lock.Unlock()

	if set.blocks == nil {
		set.blocks = item
	} else {
		// ring 자료구조의 한칸 뒤로 포커스해서 새로운 item을 연결
		set.blocks.Move(-1).Link(item)
	}
	// Display a log for the user to notify of a new mined block unconfirmed
	log.Info("🔨 mined potential block", "number", index, "hash", hash, "Total blocks", set.blocks.Len())
}

// Shift drops all unconfirmed blocks from the set which exceed the unconfirmed sets depth
// allowance, checking them against the canonical chain for inclusion or staleness
// report.
//
// Shift는 확인되지 않은 설정 깊이 허용치를 초과하는 모든 미확인 블록을 세트에서 삭제한 다음
// 포함 또는 지연 보고서를 작성하기 위해 표준 체인과 대조한다.
func (set *unconfirmedBlocks) Shift(height uint64) {
	fmt.Println("unconfirmedBlocks.Shift () 호출 height : ", height)
	set.lock.Lock()
	defer set.lock.Unlock()

	for set.blocks != nil {
		// Retrieve the next unconfirmed block and abort if too fresh
		// 다음 미확인 블록을 검색하고 생성된 지 얼마 안됐다면 처리를 중단한다.
		next := set.blocks.Value.(*unconfirmedBlock)
		if next.index+uint64(set.depth) > height {
			fmt.Printf("unconfirmedBlocks.Shift () / Break ! \n idx+depth : %v , height : %v", next.index+uint64(set.depth), height)
			break
		}
		// Block seems to exceed depth allowance, check for canonical status
		// 블록이 depth 허용치를 초과해 보인다면 표준 status를 확인한다.
		header := set.chain.GetHeaderByNumber(next.index)
		switch {
		case header == nil:
			log.Warn("Failed to retrieve header of mined block", "number", next.index, "hash", next.hash)
		case header.Hash() == next.hash:
			log.Info("🔗 block reached canonical chain", "number", next.index, "hash", next.hash)
		default:
			// Block is not canonical, check whether we have an uncle or a lost block
			// 블록이 정본이 아니라면, 엉클블록으로 가져올지, 블록을 포기할지 확인한다.
			fmt.Println("unconfirmedBlocks.Shift () / block is not canonical")
			included := false
			for number := next.index; !included && number < next.index+uint64(set.depth) && number <= height; number++ {
				if block := set.chain.GetBlockByNumber(number); block != nil {
					for _, uncle := range block.Uncles() {
						if uncle.Hash() == next.hash {
							included = true
							break
						}
					}
				}
			}
			if included {
				log.Info("⑂ block became an uncle", "number", next.index, "hash", next.hash)
			} else {
				log.Info("😱 block lost", "number", next.index, "hash", next.hash)
			}
		}
		// Drop the block out of the ring
		if set.blocks.Value == set.blocks.Next().Value {
			set.blocks = nil
		} else {
			set.blocks = set.blocks.Move(-1)
			set.blocks.Unlink(1)
			set.blocks = set.blocks.Move(1)
		}
	}
}
