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

package light

import (
	"bytes"
	"context"
	"errors"
	"math/big"

	"github.com/BerithFoundation/berith-chain/common"
	"github.com/BerithFoundation/berith-chain/core"
	"github.com/BerithFoundation/berith-chain/core/rawdb"
	"github.com/BerithFoundation/berith-chain/core/types"
	"github.com/BerithFoundation/berith-chain/crypto"
	"github.com/BerithFoundation/berith-chain/rlp"
)

var sha3_nil = crypto.Keccak256Hash(nil)

// errNonCanonicalHash is returned if the requested chain data doesn't belong
// to the canonical chain. ODR can only retrieve the canonical chain data covered
// by the CHT or Bloom trie for verification.
var errNonCanonicalHash = errors.New("hash is not currently canonical")

func GetHeaderByNumber(ctx context.Context, odr OdrBackend, number uint64) (*types.Header, error) {
	db := odr.Database()
	hash := rawdb.ReadCanonicalHash(db, number)
	if (hash != common.Hash{}) {
		// if there is a canonical hash, there is a header too
		header := rawdb.ReadHeader(db, hash, number)
		if header == nil {
			panic("Canonical hash present but header not found")
		}
		return header, nil
	}

	var (
		chtCount, sectionHeadNum uint64
		sectionHead              common.Hash
	)
	if odr.ChtIndexer() != nil {
		chtCount, sectionHeadNum, sectionHead = odr.ChtIndexer().Sections()
		canonicalHash := rawdb.ReadCanonicalHash(db, sectionHeadNum)
		// if the CHT was injected as a trusted checkpoint, we have no canonical hash yet so we accept zero hash too
		for chtCount > 0 && canonicalHash != sectionHead && canonicalHash != (common.Hash{}) {
			chtCount--
			if chtCount > 0 {
				sectionHeadNum = chtCount*odr.IndexerConfig().ChtSize - 1
				sectionHead = odr.ChtIndexer().SectionHead(chtCount - 1)
				canonicalHash = rawdb.ReadCanonicalHash(db, sectionHeadNum)
			}
		}
	}
	if number >= chtCount*odr.IndexerConfig().ChtSize {
		return nil, errNoTrustedCht
	}
	r := &ChtRequest{ChtRoot: GetChtRoot(db, chtCount-1, sectionHead), ChtNum: chtCount - 1, BlockNum: number, Config: odr.IndexerConfig()}
	if err := odr.Retrieve(ctx, r); err != nil {
		return nil, err
	}
	return r.Header, nil
}

func GetCanonicalHash(ctx context.Context, odr OdrBackend, number uint64) (common.Hash, error) {
	hash := rawdb.ReadCanonicalHash(odr.Database(), number)
	if (hash != common.Hash{}) {
		return hash, nil
	}
	header, err := GetHeaderByNumber(ctx, odr, number)
	if header != nil {
		return header.Hash(), nil
	}
	return common.Hash{}, err
}

// GetTd retrieves the total difficulty corresponding to the number and hash.
func GetTd(ctx context.Context, odr OdrBackend, hash common.Hash, number uint64) (*big.Int, error) {
	td := rawdb.ReadTd(odr.Database(), hash, number)
	if td != nil {
		return td, nil
	}
	header, err := GetHeaderByNumber(ctx, odr, number)
	if err != nil {
		return nil, err
	}
	if header.Hash() != hash {
		return nil, errNonCanonicalHash
	}
	// <hash, number> -> td mapping already be stored in db, get it.
	return rawdb.ReadTd(odr.Database(), hash, number), nil
}

// GetBodyRLP retrieves the block body (transactions and uncles) in RLP encoding.
func GetBodyRLP(ctx context.Context, odr OdrBackend, hash common.Hash, number uint64) (rlp.RawValue, error) {
	if data := rawdb.ReadBodyRLP(odr.Database(), hash, number); data != nil {
		return data, nil
	}
	r := &BlockRequest{Hash: hash, Number: number}
	if err := odr.Retrieve(ctx, r); err != nil {
		return nil, err
	} else {
		return r.Rlp, nil
	}
}

// GetBody retrieves the block body (transactons, uncles) corresponding to the
// hash.
func GetBody(ctx context.Context, odr OdrBackend, hash common.Hash, number uint64) (*types.Body, error) {
	data, err := GetBodyRLP(ctx, odr, hash, number)
	if err != nil {
		return nil, err
	}
	body := new(types.Body)
	if err := rlp.Decode(bytes.NewReader(data), body); err != nil {
		return nil, err
	}
	return body, nil
}

// GetBlock retrieves an entire block corresponding to the hash, assembling it
// back from the stored header and body.
func GetBlock(ctx context.Context, odr OdrBackend, hash common.Hash, number uint64) (*types.Block, error) {
	// Retrieve the block header and body contents
	header := rawdb.ReadHeader(odr.Database(), hash, number)
	if header == nil {
		return nil, errNoHeader
	}
	body, err := GetBody(ctx, odr, hash, number)
	if err != nil {
		return nil, err
	}
	// Reassemble the block and return
	return types.NewBlockWithHeader(header).WithBody(body.Transactions, body.Uncles), nil
}

// GetBlockReceipts retrieves the receipts generated by the transactions included
// in a block given by its hash.
func GetBlockReceipts(ctx context.Context, odr OdrBackend, hash common.Hash, number uint64) (types.Receipts, error) {
	// Retrieve the potentially incomplete receipts from disk or network
	receipts := rawdb.ReadReceipts(odr.Database(), hash, number)
	if receipts == nil {
		header, err := GetHeaderByNumber(ctx, odr, number)
		if err != nil {
			return nil, errNoHeader
		}
		if header.Hash() != hash {
			return nil, errNonCanonicalHash
		}
		r := &ReceiptsRequest{Hash: hash, Number: number, Header: header}
		if err := odr.Retrieve(ctx, r); err != nil {
			return nil, err
		}
		receipts = r.Receipts
	}
	// If the receipts are incomplete, fill the derived fields
	if len(receipts) > 0 && receipts[0].TxHash == (common.Hash{}) {
		block, err := GetBlock(ctx, odr, hash, number)
		if err != nil {
			return nil, err
		}
		genesis := rawdb.ReadCanonicalHash(odr.Database(), 0)
		config := rawdb.ReadChainConfig(odr.Database(), genesis)

		if err := receipts.DeriveFields(config, block.Hash(), block.NumberU64(), block.Transactions()); err != nil {
			return nil, err
		}
		rawdb.WriteReceipts(odr.Database(), hash, number, receipts)
	}
	return receipts, nil
}

// GetBlockLogs retrieves the logs generated by the transactions included in a
// block given by its hash.
func GetBlockLogs(ctx context.Context, odr OdrBackend, hash common.Hash, number uint64) ([][]*types.Log, error) {
	// Retrieve the potentially incomplete receipts from disk or network
	receipts := rawdb.ReadReceipts(odr.Database(), hash, number)
	if receipts == nil {
		r := &ReceiptsRequest{Hash: hash, Number: number}
		if err := odr.Retrieve(ctx, r); err != nil {
			return nil, err
		}
		receipts = r.Receipts
	}
	// Return the logs without deriving any computed fields on the receipts
	logs := make([][]*types.Log, len(receipts))
	for i, receipt := range receipts {
		logs[i] = receipt.Logs
	}
	return logs, nil
}

// GetBloomBits retrieves a batch of compressed bloomBits vectors belonging to the given bit index and section indexes
func GetBloomBits(ctx context.Context, odr OdrBackend, bitIdx uint, sectionIdxList []uint64) ([][]byte, error) {
	var (
		db      = odr.Database()
		result  = make([][]byte, len(sectionIdxList))
		reqList []uint64
		reqIdx  []int
	)

	var (
		bloomTrieCount, sectionHeadNum uint64
		sectionHead                    common.Hash
	)
	if odr.BloomTrieIndexer() != nil {
		bloomTrieCount, sectionHeadNum, sectionHead = odr.BloomTrieIndexer().Sections()
		canonicalHash := rawdb.ReadCanonicalHash(db, sectionHeadNum)
		// if the BloomTrie was injected as a trusted checkpoint, we have no canonical hash yet so we accept zero hash too
		for bloomTrieCount > 0 && canonicalHash != sectionHead && canonicalHash != (common.Hash{}) {
			bloomTrieCount--
			if bloomTrieCount > 0 {
				sectionHeadNum = bloomTrieCount*odr.IndexerConfig().BloomTrieSize - 1
				sectionHead = odr.BloomTrieIndexer().SectionHead(bloomTrieCount - 1)
				canonicalHash = rawdb.ReadCanonicalHash(db, sectionHeadNum)
			}
		}
	}

	for i, sectionIdx := range sectionIdxList {
		sectionHead := rawdb.ReadCanonicalHash(db, (sectionIdx+1)*odr.IndexerConfig().BloomSize-1)
		// if we don't have the canonical hash stored for this section head number, we'll still look for
		// an entry with a zero sectionHead (we store it with zero section head too if we don't know it
		// at the time of the retrieval)
		bloomBits, err := rawdb.ReadBloomBits(db, bitIdx, sectionIdx, sectionHead)
		if err == nil {
			result[i] = bloomBits
		} else {
			// TODO(rjl493456442) Convert sectionIndex to BloomTrie relative index
			if sectionIdx >= bloomTrieCount {
				return nil, errNoTrustedBloomTrie
			}
			reqList = append(reqList, sectionIdx)
			reqIdx = append(reqIdx, i)
		}
	}
	if reqList == nil {
		return result, nil
	}

	r := &BloomRequest{BloomTrieRoot: GetBloomTrieRoot(db, bloomTrieCount-1, sectionHead), BloomTrieNum: bloomTrieCount - 1,
		BitIdx: bitIdx, SectionIndexList: reqList, Config: odr.IndexerConfig()}
	if err := odr.Retrieve(ctx, r); err != nil {
		return nil, err
	} else {
		for i, idx := range reqIdx {
			result[idx] = r.BloomBits[i]
		}
		return result, nil
	}
}

// GetTransaction retrieves a canonical transaction by hash and also returns
// its position in the chain. There is no guarantee in the LES protocol that
// the mined transaction will be retrieved back for sure because of different
// reasons(the transaction is unindexed, the malicous server doesn't reply it
// deliberately, etc). Therefore, unretrieved transactions will receive a certain
// number of retrys, thus giving a weak guarantee.
func GetTransaction(ctx context.Context, odr OdrBackend, txHash common.Hash) (*types.Transaction, common.Hash, uint64, uint64, error) {
	r := &TxStatusRequest{Hashes: []common.Hash{txHash}}
	if err := odr.RetrieveTxStatus(ctx, r); err != nil || r.Status[0].Status != core.TxStatusIncluded {
		return nil, common.Hash{}, 0, 0, err
	}
	pos := r.Status[0].Lookup
	// first ensure that we have the header, otherwise block body retrieval will fail
	// also verify if this is a canonical block by getting the header by number and checking its hash
	if header, err := GetHeaderByNumber(ctx, odr, pos.BlockIndex); err != nil || header.Hash() != pos.BlockHash {
		return nil, common.Hash{}, 0, 0, err
	}
	body, err := GetBody(ctx, odr, pos.BlockHash, pos.BlockIndex)
	if err != nil || uint64(len(body.Transactions)) <= pos.Index || body.Transactions[pos.Index].Hash() != txHash {
		return nil, common.Hash{}, 0, 0, err
	}
	return body.Transactions[pos.Index], pos.BlockHash, pos.BlockIndex, pos.Index, nil
}
