// Copyright 2017 The go-ethereum Authors
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

package core

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BerithFoundation/berith-chain/berithdb"
	"github.com/BerithFoundation/berith-chain/common"
	"github.com/BerithFoundation/berith-chain/core/rawdb"
	"github.com/BerithFoundation/berith-chain/core/types"
	"github.com/BerithFoundation/berith-chain/event"
	"github.com/BerithFoundation/berith-chain/log"
)

// ChainIndexerBackend defines the methods needed to process chain segments in
// the background and write the segment results into the database. These can be
// used to create filter blooms or CHTs.
type ChainIndexerBackend interface {
	// Reset initiates the processing of a new chain segment, potentially terminating
	// any partially completed operations (in case of a reorg).
	Reset(ctx context.Context, section uint64, prevHead common.Hash) error

	// Process crunches through the next header in the chain segment. The caller
	// will ensure a sequential order of headers.
	Process(ctx context.Context, header *types.Header) error

	// Commit finalizes the section metadata and stores it into the database.
	Commit() error
}

// ChainIndexerChain interface is used for connecting the indexer to a blockchain
// ChainIndexerChain interface는 인덱서를 블록체인에 연결하기 위해 사용된다.
type ChainIndexerChain interface {
	// CurrentHeader retrieves the latest locally known header.
	CurrentHeader() *types.Header

	// SubscribeChainHeadEvent subscribes to new head header notifications.
	SubscribeChainHeadEvent(ch chan<- ChainHeadEvent) event.Subscription
}

// ChainIndexer does a post-processing job for equally sized sections of the
// canonical chain (like BlooomBits and CHT structures). A ChainIndexer is
// connected to the blockchain through the event system by starting a
// ChainHeadEventLoop in a goroutine.
//
// ChainIndexer는 표준 체인의 동일한 크기의 섹션 (BloomBits, CHT 등)에 대한 후 처리 작업을 수행한다.
//
// Further child ChainIndexers can be added which use the output of the parent
// section indexer. These child indexers receive new head notifications only
// after an entire section has been finished or in case of rollbacks that might
// affect already finished sections.
//
// 상위 섹션 인덱서의 아웃풋을 사용하는 하위 체인 인덱서를 추가할 수 있다.
// 이러한 하위 인덱서는 전체 섹션을 완료한 후 또는 이미 완료된 섹션에
// 영향을 줄 수 있는 롤백의 경우에만 새 헤드 알림을 받는다.
type ChainIndexer struct {
	chainDb  berithdb.Database   // Chain database to index the data from
	indexDb  berithdb.Database   // Prefixed table-view of the db to write index metadata into
	backend  ChainIndexerBackend // Background processor generating the index data content
	children []*ChainIndexer     // Child indexers to cascade chain updates to

	// 루프가 시작되었는지에 대한 플래그
	active uint32 // Flag whether the event loop was started

	// 헤더가 작업되어야 하는지 알리는 채널
	update chan struct{} // Notification channel that headers should be processed

	quit      chan chan error // Quit channel to tear down running goroutines
	ctx       context.Context
	ctxCancel func()

	// 처리해야 할 단일 체인 세그먼트의 블록 수 4096으로 초기화
	sectionSize uint64 // Number of blocks in a single chain segment to process

	// 완료된 세그먼트를 처리하기 전의 확인 수
	confirmsReq uint64 // Number of confirmations before processing a completed segment

	storedSections uint64 // Number of sections successfully indexed into the database
	knownSections  uint64 // Number of sections known to be complete (block wise)
	cascadedHead   uint64 // Block number of the last completed section cascaded to subindexers

	// 체크포인트가 적용되는 세션 수
	checkpointSections uint64      // Number of sections covered by the checkpoint
	checkpointHead     common.Hash // Section head belonging to the checkpoint

	throttling time.Duration // Disk throttling to prevent a heavy upgrade from hogging resources

	log  log.Logger
	lock sync.RWMutex
}

// NewChainIndexer creates a new chain indexer to do background processing on
// chain segments of a given size after certain number of confirmations passed.
// The throttling parameter might be used to prevent database thrashing.
func NewChainIndexer(chainDb, indexDb berithdb.Database, backend ChainIndexerBackend, section, confirm uint64, throttling time.Duration, kind string) *ChainIndexer {
	c := &ChainIndexer{
		chainDb:     chainDb,
		indexDb:     indexDb,
		backend:     backend,
		update:      make(chan struct{}, 1),
		quit:        make(chan chan error),
		sectionSize: section,
		confirmsReq: confirm,
		throttling:  throttling,
		log:         log.New("type", kind),
	}
	// Initialize database dependent fields and start the updater
	c.loadValidSections()
	c.ctx, c.ctxCancel = context.WithCancel(context.Background())

	go c.updateLoop()

	return c
}

// AddCheckpoint adds a checkpoint. Sections are never processed and the chain
// is not expected to be available before this point. The indexer assumes that
// the backend has sufficient information available to process subsequent sections.
//
// Note: knownSections == 0 and storedSections == checkpointSections until
// syncing reaches the checkpoint
func (c *ChainIndexer) AddCheckpoint(section uint64, shead common.Hash) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.checkpointSections = section + 1
	c.checkpointHead = shead

	if section < c.storedSections {
		return
	}
	c.setSectionHead(section, shead)
	c.setValidSections(section + 1)
}

// Start creates a goroutine to feed chain head events into the indexer for
// cascading background processing. Children do not need to be started, they
// are notified about new events by their parents.
// ChainIndexer.Start()는 백그라운드 처리에 종속되어있는 (Bloom)인덱서에게
// 체인헤드 이벤트를 공급하기 위한 고루틴을 생성한다.
func (c *ChainIndexer) Start(chain ChainIndexerChain) {
	events := make(chan ChainHeadEvent, 10)
	sub := chain.SubscribeChainHeadEvent(events)

	go c.eventLoop(chain.CurrentHeader(), events, sub)
}

// Close tears down all goroutines belonging to the indexer and returns any error
// that might have occurred internally.
func (c *ChainIndexer) Close() error {
	var errs []error

	c.ctxCancel()

	// Tear down the primary update loop
	errc := make(chan error)
	c.quit <- errc
	if err := <-errc; err != nil {
		errs = append(errs, err)
	}
	// If needed, tear down the secondary event loop
	if atomic.LoadUint32(&c.active) != 0 {
		c.quit <- errc
		if err := <-errc; err != nil {
			errs = append(errs, err)
		}
	}
	// Close all children
	for _, child := range c.children {
		if err := child.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	// Return any failures
	switch {
	case len(errs) == 0:
		return nil

	case len(errs) == 1:
		return errs[0]

	default:
		return fmt.Errorf("%v", errs)
	}
}

// eventLoop is a secondary - optional - event loop of the indexer which is only
// started for the outermost indexer to push chain head events into a processing
// queue.
//
// eventLoop는 가장 바깥쪽 인덱서가 체인 헤드 이벤트를 프로세싱 큐로
// 밀어넣기 위해 시작되는 인덱서의 옵셔널한 이벤트 루프이다.
func (c *ChainIndexer) eventLoop(currentHeader *types.Header, events chan ChainHeadEvent, sub event.Subscription) {
	// Mark the chain indexer as active, requiring an additional teardown
	atomic.StoreUint32(&c.active, 1)

	defer sub.Unsubscribe()

	// Fire the initial new head event to start any outstanding processing
	// 초기 새 헤드 이벤트를 발생시켜 미처리 작업을 시작한다.
	c.newHead(currentHeader.Number.Uint64(), false)

	var (
		prevHeader = currentHeader
		prevHash   = currentHeader.Hash()
	)
	for {
		select {
		case errc := <-c.quit:
			// Chain indexer terminating, report no failure and abort
			errc <- nil
			return

		case ev, ok := <-events:
			// Received a new event, ensure it's not nil (closing) and update
			if !ok {
				errc := <-c.quit
				errc <- nil
				return
			}
			header := ev.Block.Header()
			fmt.Println("ChainIndexer.evetLoop() ChainHeadEvents 수신 : ", header.Number.Int64())
			if header.ParentHash != prevHash {
				// Reorg to the common ancestor if needed (might not exist in light sync mode, skip reorg then)
				// TODO(karalabe, zsfelfoldi): This seems a bit brittle, can we detect this case explicitly?
				// 새로 수신된 ChainHeader 이벤트의 블록과 기존 블록이 공통으로 가지는 상위 블록을 찾는다.
				if rawdb.ReadCanonicalHash(c.chainDb, prevHeader.Number.Uint64()) != prevHash {
					if h := rawdb.FindCommonAncestor(c.chainDb, prevHeader, header); h != nil {
						c.newHead(h.Number.Uint64(), true)
					}
				}
			}
			c.newHead(header.Number.Uint64(), false)

			prevHeader, prevHash = header, header.Hash()
		}
	}
}

// newHead notifies the indexer about new chain heads and/or reorgs.
func (c *ChainIndexer) newHead(head uint64, reorg bool) {
	fmt.Println("ChainIndexer.newHead() 호출, Reorg : ", reorg)
	c.lock.Lock()
	defer c.lock.Unlock()

	// If a reorg happened, invalidate all sections until that point
	// 재편성되면, 해당 시점까지 모든 섹션을 무효화한다.
	if reorg {
		// Revert the known section number to the reorg point
		known := head / c.sectionSize // 현재 블록이 몇 번 째 섹션인가?
		stored := known
		if known < c.checkpointSections {
			known = 0
		}
		if stored < c.checkpointSections {
			stored = c.checkpointSections
		}
		if known < c.knownSections {
			c.knownSections = known
		}
		// Revert the stored sections from the database to the reorg point
		if stored < c.storedSections {
			c.setValidSections(stored)
		}
		// Update the new head number to the finalized section end and notify children
		head = known * c.sectionSize

		if head < c.cascadedHead {
			c.cascadedHead = head
			for _, child := range c.children {
				child.newHead(c.cascadedHead, true)
			}
		}
		return
	}
	// No reorg, calculate the number of newly known sections and update if high enough
	// 재편성하지 않는다면, 새로 알려진 섹션들의 수만 계산하고 체인이 충분히 높다면 업데이트한다.
	var sections uint64
	if head >= c.confirmsReq {
		sections = (head + 1 - c.confirmsReq) / c.sectionSize
		if sections < c.checkpointSections {
			sections = 0
		}
		if sections > c.knownSections {
			if c.knownSections < c.checkpointSections {
				// syncing reached the checkpoint, verify section head
				syncedHead := rawdb.ReadCanonicalHash(c.chainDb, c.checkpointSections*c.sectionSize-1)
				if syncedHead != c.checkpointHead {
					c.log.Error("Synced chain does not match checkpoint", "number", c.checkpointSections*c.sectionSize-1, "expected", c.checkpointHead, "synced", syncedHead)
					return
				}
			}
			c.knownSections = sections

			select {
			case c.update <- struct{}{}:
				fmt.Println("ChainIndexer.newHead() / c.update 데이터 전달")
			default:
			}
		}
	}
}

// updateLoop is the main event loop of the indexer which pushes chain segments
// down into the processing backend.
func (c *ChainIndexer) updateLoop() {
	var (
		updating bool
		updated  time.Time
	)

	for {
		select {
		case errc := <-c.quit:
			// Chain indexer terminating, report no failure and abort
			errc <- nil
			return

		case <-c.update:
			fmt.Println("ChainIndexer.updateLoop() / c.updata 채널 데이터 수신 ")
			// Section headers completed (or rolled back), update the index
			c.lock.Lock()
			if c.knownSections > c.storedSections {
				// Periodically print an upgrade log message to the user
				if time.Since(updated) > 8*time.Second {
					if c.knownSections > c.storedSections+1 {
						updating = true
						c.log.Info("Upgrading chain index", "percentage", c.storedSections*100/c.knownSections)
					}
					updated = time.Now()
				}
				// Cache the current section count and head to allow unlocking the mutex
				section := c.storedSections
				var oldHead common.Hash
				if section > 0 {
					oldHead = c.SectionHead(section - 1)
				}
				// Process the newly defined section in the background
				c.lock.Unlock()
				newHead, err := c.processSection(section, oldHead)
				if err != nil {
					select {
					case <-c.ctx.Done():
						<-c.quit <- nil
						return
					default:
					}
					c.log.Error("Section processing failed", "error", err)
				}
				c.lock.Lock()

				// If processing succeeded and no reorgs occcurred, mark the section completed
				if err == nil && oldHead == c.SectionHead(section-1) {
					c.setSectionHead(section, newHead)
					c.setValidSections(section + 1)
					if c.storedSections == c.knownSections && updating {
						updating = false
						c.log.Info("Finished upgrading chain index")
					}
					c.cascadedHead = c.storedSections*c.sectionSize - 1
					for _, child := range c.children {
						c.log.Trace("Cascading chain index update", "head", c.cascadedHead)
						child.newHead(c.cascadedHead, false)
					}
				} else {
					// If processing failed, don't retry until further notification
					c.log.Debug("Chain index processing failed", "section", section, "err", err)
					c.knownSections = c.storedSections
				}
			}
			// If there are still further sections to process, reschedule
			if c.knownSections > c.storedSections {
				time.AfterFunc(c.throttling, func() {
					select {
					case c.update <- struct{}{}:
					default:
					}
				})
			}
			c.lock.Unlock()
		}
	}
}

// processSection processes an entire section by calling backend functions while
// ensuring the continuity of the passed headers. Since the chain mutex is not
// held while processing, the continuity can be broken by a long reorg, in which
// case the function returns with an error.
//
// processSection은 전달된 헤더들의 연속성을 보장하면서 백엔드 함수들을 호출하여 전체 섹션을 처리한다.
// 처리하는 동안 체인 뮤텍스가 유지되지 않기 때문에 재편성이 길어지면 연속성이 깨질수 있다.
func (c *ChainIndexer) processSection(section uint64, lastHead common.Hash) (common.Hash, error) {
	c.log.Trace("Processing new chain section", "section", section)

	// Reset and partial processing

	if err := c.backend.Reset(c.ctx, section, lastHead); err != nil {
		c.setValidSections(0)
		return common.Hash{}, err
	}

	for number := section * c.sectionSize; number < (section+1)*c.sectionSize; number++ {
		hash := rawdb.ReadCanonicalHash(c.chainDb, number)
		if hash == (common.Hash{}) {
			return common.Hash{}, fmt.Errorf("canonical block #%d unknown", number)
		}
		header := rawdb.ReadHeader(c.chainDb, hash, number)
		if header == nil {
			return common.Hash{}, fmt.Errorf("block #%d [%x…] not found", number, hash[:4])
		} else if header.ParentHash != lastHead {
			return common.Hash{}, fmt.Errorf("chain reorged during section processing")
		}
		if err := c.backend.Process(c.ctx, header); err != nil {
			return common.Hash{}, err
		}
		lastHead = header.Hash()
	}
	if err := c.backend.Commit(); err != nil {
		return common.Hash{}, err
	}
	return lastHead, nil
}

// Sections returns the number of processed sections maintained by the indexer
// and also the information about the last header indexed for potential canonical
// verifications.
func (c *ChainIndexer) Sections() (uint64, uint64, common.Hash) {
	c.lock.Lock()
	defer c.lock.Unlock()

	return c.storedSections, c.storedSections*c.sectionSize - 1, c.SectionHead(c.storedSections - 1)
}

// AddChildIndexer adds a child ChainIndexer that can use the output of this one
func (c *ChainIndexer) AddChildIndexer(indexer *ChainIndexer) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.children = append(c.children, indexer)

	// Cascade any pending updates to new children too
	sections := c.storedSections
	if c.knownSections < sections {
		// if a section is "stored" but not "known" then it is a checkpoint without
		// available chain data so we should not cascade it yet
		sections = c.knownSections
	}
	if sections > 0 {
		indexer.newHead(sections*c.sectionSize-1, false)
	}
}

// loadValidSections reads the number of valid sections from the index database
// and caches is into the local state.
func (c *ChainIndexer) loadValidSections() {
	data, _ := c.indexDb.Get([]byte("count"))
	if len(data) == 8 {
		c.storedSections = binary.BigEndian.Uint64(data)
	}
}

// setValidSections writes the number of valid sections to the index database
func (c *ChainIndexer) setValidSections(sections uint64) {
	// Set the current number of valid sections in the database
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], sections)
	c.indexDb.Put([]byte("count"), data[:])

	// Remove any reorged sections, caching the valids in the mean time
	for c.storedSections > sections {
		c.storedSections--
		c.removeSectionHead(c.storedSections)
	}
	c.storedSections = sections // needed if new > old
}

// SectionHead retrieves the last block hash of a processed section from the
// index database.
func (c *ChainIndexer) SectionHead(section uint64) common.Hash {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], section)

	hash, _ := c.indexDb.Get(append([]byte("shead"), data[:]...))
	if len(hash) == len(common.Hash{}) {
		return common.BytesToHash(hash)
	}
	return common.Hash{}
}

// setSectionHead writes the last block hash of a processed section to the index
// database.
func (c *ChainIndexer) setSectionHead(section uint64, hash common.Hash) {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], section)

	c.indexDb.Put(append([]byte("shead"), data[:]...), hash.Bytes())
}

// removeSectionHead removes the reference to a processed section from the index
// database.
func (c *ChainIndexer) removeSectionHead(section uint64) {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], section)

	c.indexDb.Delete(append([]byte("shead"), data[:]...))
}
