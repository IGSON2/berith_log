/*
d8888b. d88888b d8888b. d888888b d888888b db   db
88  `8D 88'     88  `8D   `88'   `~~88~~' 88   88
88oooY' 88ooooo 88oobY'    88       88    88ooo88
88~~~b. 88~~~~~ 88`8b      88       88    88~~~88
88   8D 88.     88 `88.   .88.      88    88   88
Y8888P' Y88888P 88   YD Y888888P    YP    YP   YP

	  copyrights by ibizsoftware 2018 - 2019
*/

/**
[BERITH]
- The consensus algorithm interface implementation handles the Berith consensus process here.
- Header verification and body data verification
- Body data verification
  BC check, group check, priority verification
- Check the Tx of the body and record it in the Staking DB and select it
**/

package bsrr

import (
	"berith-chain/trie"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"time"

	"github.com/BerithFoundation/berith-chain/rpc"

	"github.com/BerithFoundation/berith-chain/accounts"
	"github.com/BerithFoundation/berith-chain/berith/selection"
	"github.com/BerithFoundation/berith-chain/berith/staking"
	"github.com/BerithFoundation/berith-chain/berithdb"
	"github.com/BerithFoundation/berith-chain/common"
	"github.com/BerithFoundation/berith-chain/consensus"
	"github.com/BerithFoundation/berith-chain/consensus/misc"
	"github.com/BerithFoundation/berith-chain/core/state"
	"github.com/BerithFoundation/berith-chain/core/types"
	"github.com/BerithFoundation/berith-chain/crypto"
	"github.com/BerithFoundation/berith-chain/crypto/sha3"
	"github.com/BerithFoundation/berith-chain/log"
	"github.com/BerithFoundation/berith-chain/params"
	"github.com/BerithFoundation/berith-chain/rlp"
	lru "github.com/hashicorp/golang-lru"
)

const (
	inmemorySnapshots  = 128     // Number of recent vote snapshots to keep in memory
	inmemorySigners    = 128 * 3 // Number of recent vote snapshots to keep in memory
	inmemorySignatures = 4096    // Number of recent block signatures to keep in memory

	termDelay  = 100 * time.Millisecond // Delay per signer in the same group
	groupDelay = 1 * time.Second        // Delay per groups

	commonDiff = 3 // A constant that specifies the maximum number of people in a group when dividing a signer's candidates into multiple groups
)

var (
	RewardBlock          = big.NewInt(500)
	StakeMinimum, _      = new(big.Int).SetString(params.StakeMinimum, 0)
	LimitStakeBalance, _ = new(big.Int).SetString(params.LimitStakeBalance, 0)
	SlashRound           = uint64(2)
	ForkFactor           = 1.0

	epochLength = uint64(360) // Default number of blocks after which to checkpoint and reset the pending votes

	extraVanity = 32 // Fixed number of extra-data prefix bytes reserved for signer vanity
	extraSeal   = 65 // Fixed number of extra-data suffix bytes reserved for signer seal

	uncleHash = types.CalcUncleHash(nil) // Always Keccak256(RLP([])) as uncles are meaningless outside of PoW.

	diffWithoutStaker = int64(1234)
)

// Various error messages to mark blocks invalid. These should be private to
// prevent engine specific errors from being referenced in the remainder of the
// codebase, inherently breaking if the engine is swapped out. Please put common
// error types into the consensus package.
var (
	// errUnknownBlock is returned when the list of signers is requested for a block
	// that is not part of the local blockchain.
	errUnknownBlock = errors.New("unknown block")

	// errMissingVanity is returned if a block's extra-data section is shorter than
	// 32 bytes, which is required to store the signer vanity.
	errMissingVanity = errors.New("extra-data 32 byte vanity prefix missing")

	// errMissingSignature is returned if a block's extra-data section doesn't seem
	// to contain a 65 byte secp256k1 signature.
	errMissingSignature = errors.New("extra-data 65 byte signature suffix missing")

	// errExtraSigners is returned if non-checkpoint block contain signer data in
	// their extra-data fields.
	// 체크포인트 블록이 아닌데 서명자 목록을 포함하고 있을 경우
	errExtraSigners = errors.New("non-checkpoint block contains extra signer list")

	// errInvalidCheckpointSigners is returned if a checkpoint block contains an
	// invalid list of signers (i.e. non divisible by 20 bytes).
	// 체크포인트 블록이 유효하지 않은 서명자 목록을 포함하고 있을경우 (주소 길이인 20byte로 나누어 떨어져야 함)
	errInvalidCheckpointSigners = errors.New("invalid signer list on checkpoint block")

	// errInvalidMixDigest is returned if a block's mix digest is non-zero.
	errInvalidMixDigest = errors.New("non-zero mix digest")

	// errInvalidUncleHash is returned if a block contains an non-empty uncle list.
	errInvalidUncleHash = errors.New("non empty uncle hash")

	// errInvalidDifficulty is returned if the difficulty of a block neither 1 or 2.
	errInvalidDifficulty = errors.New("invalid difficulty")

	// ErrInvalidTimestamp is returned if the timestamp of a block is lower than
	// the previous block's timestamp + the minimum block period.
	ErrInvalidTimestamp = errors.New("invalid timestamp")

	// errInvalidVotingChain is returned if an authorization list is attempted to
	// be modified via out-of-range or non-contiguous headers.
	errInvalidVotingChain = errors.New("invalid voting chain")

	// errUnauthorizedSigner is returned if a header is signed by a non-authorized entity.
	errUnauthorizedSigner = errors.New("unauthorized signer")

	errNoData = errors.New("no data")

	// errInvalidNonce is returned if a nonce is less than or equals to 0.
	errInvalidNonce = errors.New("invalid nonce")

	errStakingList = errors.New("not found staking list")

	errMissingState = errors.New("state missing")

	errCleanStakingDB = errors.New("fail to clean stakingDB")

	errBIP1 = errors.New("error when fork network to BIP1")
)

// SignerFn is a signer callback function to request a hash to be signed by a
// backing account.
type SignerFn func(accounts.Account, []byte) ([]byte, error)

// sigHash returns the hash which is used as input for the proof-of-authority
// signing. It is the hash of the entire header apart from the 65 byte signature
// contained at the end of the extra data.
//
// Note, the method requires the extra data to be at least 65 bytes, otherwise it
// panics. This is done to avoid accidentally using both forms (signature present
// or not), which could be abused to produce different hashes for the same header.
//
// sigHash는 권한 증명 서명을 위한 입력으로 사용되는 해시를 반환한다.
// extra data 끝에 포함된 65바이트 시그니처를 제외한 전체 헤더의 해시이다.
func sigHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewKeccak256()

	_ = rlp.Encode(hasher, []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra[:len(header.Extra)-65], // Yes, this will panic if extra is too short
		header.MixDigest,
		header.Nonce,
	})
	hasher.Sum(hash[:0])
	return hash
}

// ecrecover extracts the Berith account address from a signed header.
func ecrecover(header *types.Header, sigcache *lru.ARCCache) (common.Address, error) {
	// If the signature's already cached, return that
	hash := header.Hash()
	if address, known := sigcache.Get(hash); known {
		return address.(common.Address), nil
	}
	// Retrieve the signature from the header extra-data
	if len(header.Extra) < extraSeal {
		return common.Address{}, errMissingSignature
	}
	signature := header.Extra[len(header.Extra)-extraSeal:]

	// Recover the public key and the Berith address
	pubkey, err := crypto.Ecrecover(sigHash(header).Bytes(), signature)
	if err != nil {
		return common.Address{}, err
	}
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])

	sigcache.Add(hash, signer)
	return signer, nil
}

type BSRR struct {
	config *params.BSRRConfig // Consensus engine configuration parameters
	db     berithdb.Database  // Database to store and retrieve snapshot checkpoints
	//[BERITH] add to stakingDB clique structure
	stakingDB staking.DataBase // DB storing stakingList
	cache     *lru.ARCCache    // cache to store stakingList

	recents    *lru.ARCCache // Snapshots for recent block to speed up reorgs
	signatures *lru.ARCCache // Signatures of recent blocks to speed up mining

	signer common.Address // Berith address of the signing key
	signFn SignerFn       // Signer function to authorize hashes with
	lock   sync.RWMutex   // Protects the signer fields

	proposals map[common.Address]bool // Current list of proposals we are pushing

	// The fields below are for testing only
	rankGroup common.SequenceGroup // grouped by rank
}

/*
[BERITH]
Function to create a new BSRR structure
*/
func New(config *params.BSRRConfig, db berithdb.Database) *BSRR {
	conf := config
	if conf.Epoch == 0 {
		conf.Epoch = epochLength
	}

	if conf.Rewards != nil {
		if conf.Rewards.Cmp(big.NewInt(0)) == 0 {
			conf.Rewards = RewardBlock
		}
	} else {
		conf.Rewards = RewardBlock
	}

	if conf.StakeMinimum == nil || conf.StakeMinimum.Cmp(big.NewInt(0)) == 0 {
		conf.StakeMinimum = StakeMinimum
	}

	if conf.LimitStakeBalance == nil || conf.LimitStakeBalance.Cmp(big.NewInt(0)) == 0 {
		conf.LimitStakeBalance = LimitStakeBalance
	}

	if conf.SlashRound != 0 {
		if conf.SlashRound == 0 {
			conf.SlashRound = 1
		}
	} else {
		conf.SlashRound = SlashRound
	}

	if conf.ForkFactor <= 0.0 || conf.ForkFactor > 1.0 {
		conf.ForkFactor = ForkFactor
	}

	recents, _ := lru.NewARC(inmemorySnapshots)
	signatures, _ := lru.NewARC(inmemorySignatures)
	//[BERITH] Cache instance creation and sizing
	cache, _ := lru.NewARC(inmemorySigners)

	return &BSRR{
		config:     conf,
		db:         db,
		recents:    recents,
		signatures: signatures,
		cache:      cache,
		proposals:  make(map[common.Address]bool),
		rankGroup:  &common.ArithmeticGroup{CommonDiff: commonDiff},
	}
}

/*
[BERITH]
Function that receives StakingDB and creates new BSRR structure
*/
func NewCliqueWithStakingDB(stakingDB staking.DataBase, config *params.BSRRConfig, db berithdb.Database) *BSRR {
	engine := New(config, db)
	engine.stakingDB = stakingDB
	// Synchronize the engine.config and chainConfig.
	return engine
}

// Author implements consensus.Engine, returning the Berith address recovered
// from the signature in the header's extra-data section.
func (c *BSRR) Author(header *types.Header) (common.Address, error) {
	return ecrecover(header, c.signatures)
}

// VerifyHeader checks whether a header conforms to the consensus rules.
func (c *BSRR) VerifyHeader(chain consensus.ChainReader, header *types.Header, seal bool) error {
	return c.verifyHeader(chain, header, nil)
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers. The
// method returns a quit channel to abort the operations and a results channel to
// retrieve the async verifications (the order is that of the input slice).
func (c *BSRR) VerifyHeaders(chain consensus.ChainReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	go func() {
		for i, header := range headers {
			err := c.verifyHeader(chain, header, headers[:i])

			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

// verifyHeader checks whether a header conforms to the consensus rules.The
// caller may optionally pass in a batch of parents (ascending order) to avoid
// looking those up from the database. This is useful for concurrently verifying
// a batch of new headers.
//
// verifyHeader는 헤더가 합의 규칙을 따르는지 확인한다.
// 호출자는 선택적으로 데이터베이스로부터 상위 항목을 조회하지 않도록 상위 그룹(오름차순)을 전달할 수 있다.
// 이 기능은 새 헤더 묶음을 검증하는데 유용하다.
func (c *BSRR) verifyHeader(chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {
	if header.Number == nil {
		return errUnknownBlock
	}
	number := header.Number.Uint64()

	// Don't waste time checking blocks from the future
	// Future block 검증 안함
	if header.Time.Cmp(big.NewInt(time.Now().Unix())) > 0 {
		return consensus.ErrFutureBlock
	}
	// Checkpoint blocks need to enforce zero beneficiary
	// 체크포이트 블록은 수혜자가 0명이어야 한다
	checkpoint := (number % c.config.Epoch) == 0

	// Check that the extra-data contains both the vanity and signature
	// extra-data가 vanity와 서명을 포함하는지 검증
	if len(header.Extra) < extraVanity {
		return errMissingVanity
	}
	if len(header.Extra) < extraVanity+extraSeal {
		return errMissingSignature
	}
	// Ensure that the extra-data contains a signer list on checkpoint, but none otherwise
	signersBytes := len(header.Extra) - extraVanity - extraSeal
	if !checkpoint && signersBytes != 0 {
		return errExtraSigners
	}
	if checkpoint && signersBytes%common.AddressLength != 0 {
		return errInvalidCheckpointSigners
	}
	// Ensure that the mix digest is zero as we don't have fork protection currently
	//
	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}
	// Ensure that the block doesn't contain any uncles which are meaningless in PoA
	if header.UncleHash != uncleHash {
		return errInvalidUncleHash
	}
	// Ensure that the block nonce is greater than 0
	if number > 0 && header.Nonce.Uint64() < 1 {
		return errInvalidNonce
	}

	// If all checks passed, validate any special fields for hard forks
	if err := misc.VerifyForkHashes(chain.Config(), header, false); err != nil {
		return err
	}
	// All basic checks passed, verify cascading fields
	return c.verifyCascadingFields(chain, header, parents)
}

// verifyCascadingFields verifies all the header fields that are not standalone,
// rather depend on a batch of previous headers. The caller may optionally pass
// in a batch of parents (ascending order) to avoid looking those up from the
// database. This is useful for concurrently verifying a batch of new headers.
//
// verifyCascadeingFields는 독립 실행형태가 아닌 이전 헤더 배치에 종속되어있는 모든 헤더 필드를 검증한다.
func (c *BSRR) verifyCascadingFields(chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {
	// The genesis block is the always valid dead-end
	number := header.Number.Uint64()
	if number == 0 {
		return nil
	}
	// Ensure that the block's timestamp isn't too close to it's parent
	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}
	if parent == nil || parent.Number.Uint64() != number-1 || parent.Hash() != header.ParentHash {
		return consensus.ErrUnknownAncestor
	}
	if parent.Time.Uint64()+c.config.Period > header.Time.Uint64() {
		return ErrInvalidTimestamp
	}

	// All basic checks passed, verify the seal and return
	return c.verifySeal(chain, header, parents)
}

// VerifyUncles implements consensus.Engine, always returning an error for any
// uncles as this consensus mechanism doesn't permit uncles.
func (c *BSRR) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errors.New("uncles not allowed")
	}
	return nil
}

// VerifySeal implements consensus.Engine, checking whether the signature contained
// in the header satisfies the consensus protocol requirements.
func (c *BSRR) VerifySeal(chain consensus.ChainReader, header *types.Header) error {
	return c.verifySeal(chain, header, nil)
}

// verifySeal checks whether the signature contained in the header satisfies the
// consensus protocol requirements. The method accepts an optional list of parent
// headers that aren't yet part of the local blockchain to generate the snapshots
// from.
/*
	[Berith]
	verifySeal method is necessary to implement Engine interface but not used.
	The logic that verifies the signature contained in the header is in the Finalize method.

	verifySeal은 Engine을 구현하기 위해 필요한 메서드 이지만 사용하지는 않는다.
	헤더의 서명을 검증하는 로직은 Finalize 메서드에 있다.
*/
func (c *BSRR) verifySeal(chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {
	// Verifying the genesis block is not supported
	number := header.Number
	if number.Uint64() == 0 {
		return errUnknownBlock
	}

	return nil
}

// Prepare implements consensus.Engine, preparing all the consensus fields of the
// header for running the transactions on top.
// 트랜잭션을 실행시키기 위해 헤더의 모든 합의 필드를 준비한다.
// commitNewWork에서 먼저 일부 필드가 초기화 된 블록의 헤더를 인자로 받는다.
func (c *BSRR) Prepare(chain consensus.ChainReader, header *types.Header) error {
	fmt.Println("BSRR.Prepare() 호출 Header : ", header.Number)
	header.Nonce = types.BlockNonce{}
	number := header.Number.Uint64()

	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}

	target, exist := c.getStakeTargetBlock(chain, parent)
	if !exist {
		return consensus.ErrUnknownAncestor
	}

	// Set the correct difficulty and nonce
	// 타겟블록에서 berithBase의 스코어와 순위를 반환.
	// berithBase는 노드에서 지정한 채굴자이다. 여러 노드들 중 현재 노드의 채굴자는
	// 몇위인지, 스코어는 몇점인지 알아내는 것이다.
	diff, rank := c.calcDifficultyAndRank(c.signer, chain, 0, target)
	if rank < 1 {
		return errUnauthorizedSigner
	}
	header.Difficulty = diff
	// nonce is used to check order of staking list
	header.Nonce = types.EncodeNonce(uint64(rank))

	// Ensure the extra data has all it's components
	if len(header.Extra) < extraVanity {
		header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, extraVanity-len(header.Extra))...)
	}
	header.Extra = header.Extra[:extraVanity]

	header.Extra = append(header.Extra, make([]byte, extraSeal)...)

	// Mix digest is reserved for now, set to empty
	header.MixDigest = common.Hash{}

	// 블록 생성시 먼저 현재시간에 Period만큼 더해서 Time이 초기화됨.
	header.Time = new(big.Int).Add(parent.Time, new(big.Int).SetUint64(c.config.Period))

	if header.Time.Int64() < time.Now().Unix() {
		fmt.Println("header time init. BlockNumber : ", header.Number)
		header.Time = big.NewInt(time.Now().Unix())
	}
	return nil
}

// Finalize implements consensus.Engine, ensuring no uncles are set, nor block
// rewards given, and returns the final block.
//
// Finalize는 엉클 블록이 정해지지 않았는지 확인하고,
// 블록 보상이 주어지지 않았는지 확인한 뒤, 최종 블록을 반환한다.
// 헤더의 루트를 완성하고 주어진 헤더 + 트렌젝션 + 엉클정보 + 영수증으로
// 블록을 만든다.
func (c *BSRR) Finalize(chain consensus.ChainReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {
	fmt.Println("BSRR.Finalize() 호출")
	// [Berith] Retrieves the parent block's StakingList.
	var stks staking.Stakers
	stks, err := c.getStakers(chain, header.Number.Uint64()-1, header.ParentHash)
	if err != nil {
		return nil, errStakingList
	}

	if header.Coinbase != common.HexToAddress("0") {
		var signers signers

		parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
		if parent == nil {
			log.Warn("unknown ancestor", "parent", "nil")
		}

		if chain.Config().IsBIP1Block(header.Number) { // Berith는 ETH와 달리POS니까
			stks, err = c.supportBIP1(chain, parent, stks)
			if err != nil {
				return nil, errBIP1
			}
		}
		target, exist := c.getStakeTargetBlock(chain, parent)
		if !exist {
			return nil, consensus.ErrUnknownAncestor
		}

		signers, err := c.getSigners(chain, target)
		if err != nil {
			return nil, errUnauthorizedSigner
		}

		signerMap := signers.signersMap()
		if _, ok := signerMap[header.Coinbase]; !ok {
			return nil, errUnauthorizedSigner
		}

		predicted, rank := c.calcDifficultyAndRank(header.Coinbase, chain, 0, target)
		if rank < 1 {
			return nil, errUnauthorizedSigner
		}

		if predicted.Cmp(header.Difficulty) != 0 {
			return nil, errInvalidDifficulty
		}
		if header.Nonce.Uint64() != uint64(rank) {
			return nil, errInvalidNonce
		}

		/*
			[Berith]
			To reduce disk usage, Staker information is periodically deleted.
		*/
		if new(big.Int).Mod(header.Number, big.NewInt(common.CleanCycle)).Cmp(common.Big0) == 0 {
			if err = c.stakingDB.Clean(chain, target); err != nil {
				return nil, errCleanStakingDB
			}
		}
	}

	// [BERITH] Modify the data of StateDB based on the transaction information of the received block.
	if err = c.setStakersWithTxs(state, chain, stks, txs, header); err != nil {
		return nil, errStakingList
	}

	// Reward
	c.accumulateRewards(chain, state, header)

	//[BERITH] Commit the modified StateDB data.
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	header.UncleHash = types.CalcUncleHash(nil)

	// Assemble and return the final block for sealing
	return types.NewBlock(header, txs, nil, receipts, trie.NewStackTrie(nil)), nil
}

// Authorize injects a private key into the consensus engine to mint new blocks
// with.
//
// Authorize는 새 블록을 생성하기 위해 개인키를 합의엔진에 추가한다.
// StartMining 에서 berithBase의 주소와 서명을 받아 인증한다.
func (c *BSRR) Authorize(signer common.Address, signFn SignerFn) {
	fmt.Println("signer : ", signer.Hex())
	c.lock.Lock()
	defer c.lock.Unlock()

	c.signer = signer
	c.signFn = signFn
}

// Seal implements consensus.Engine, attempting to create a sealed block using
// the local signing credentials.
//
//
func (c *BSRR) Seal(chain consensus.ChainReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	fmt.Println("BSRR.Seal() 호출")
	header := block.Header()

	// Sealing the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}

	// Don't hold the signer fields for the entire sealing procedure
	c.lock.RLock()
	signer, signFn := c.signer, c.signFn
	c.lock.RUnlock()

	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}

	// Checks target block and signers
	target, exist := c.getStakeTargetBlock(chain, parent)
	if !exist {
		return consensus.ErrUnknownAncestor
	}

	signers, err := c.getSigners(chain, target)
	if err != nil {
		return err
	}
	if _, authorized := signers.signersMap()[signer]; !authorized {
		return errUnauthorizedSigner
	}

	// Prepare에서 header.Time에 미리 period만큼 시간을 더해 놓았다.
	// 그러나 1번 블록은 제네시스 JSON 파일을 생성하고 Period 안에 채굴될 일이 거의 없기 때문에
	// 제네시스 + Period 가 웬만하면 현재 UnixTime보다 작을 것이다.
	// 때문에 1번 블록의 첫 commit 시 header.Time은 현재 시간으로 초기화되어
	// 딜레이가 음수가 되는 것이다. 이런 이유로 처리해야 할 트랜잭션이 쌓이게되면
	// 도중에 블록이 resultCh로 제출되어 새로운 commitNewWork을 호출하며
	// interrupt에 1을 치환해 버리기 때문에 commitTransactions가 return 되는 것이다.
	//
	// Sweet, the protocol permits us to sign the block, wait for our time
	delay := time.Unix(header.Time.Int64(), 0).Sub(time.Now()) // nolint: gosimple
	_, rank := c.calcDifficultyAndRank(header.Coinbase, chain, 0, target)
	fmt.Printf("BSRR.Seal() / rank : %v, delay : %v\n", rank, delay.Milliseconds())
	if rank == -1 {
		return errUnauthorizedSigner
	}

	//delay += c.getDelay(rank)
	temp, err := c.getDelay(rank)
	if err != nil {
		return err
	}
	delay += temp
	fmt.Println("Seal() / delay + temp : ", delay)

	// Sign all the things!
	sighash, err := signFn(accounts.Account{Address: signer}, sigHash(header).Bytes())
	if err != nil {
		return err
	}
	copy(header.Extra[len(header.Extra)-extraSeal:], sighash)
	// Wait until sealing is terminated or delay timeout.
	log.Trace("Waiting for slot to sign and propagate", "delay", common.PrettyDuration(delay))
	go func() {
		select {
		case <-stop:
			return
			// rank만큼 딜레이 시간이 늘어난다.
		case <-time.After(delay):
		}

		select {
		case results <- block.WithSeal(header):
			fmt.Println("resultCh로 데이터 삽입")
		default:
			log.Warn("Sealing result is not read by miner", "sealhash", c.SealHash(header))
		}
	}()
	return nil
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have ( based on the previous blocks in the chain and the
// current signer. )
// CalcDifficulty는 난이도 조정 알고리즘이다. 이 함수는 체인의 이전 블록들과 현재 서명자를 기준으로
// 새 블록이 가져야 하는 난이도를 반환한다.
//
// 그러나 아직 이 함수가 실행될 로직은 구현되어 있지 않음.
func (c *BSRR) CalcDifficulty(chain consensus.ChainReader, time uint64, parent *types.Header) *big.Int {
	target, exist := c.getStakeTargetBlock(chain, parent)
	if !exist {
		return big.NewInt(0)
	}
	diff, _ := c.calcDifficultyAndRank(c.signer, chain, time, target)
	return diff
}

func (c *BSRR) getAncestor(chain consensus.ChainReader, n int64, header *types.Header) (*types.Header, bool) {
	target := header
	targetNumber := new(big.Int).Sub(header.Number, big.NewInt(n))
	for target != nil && target.Number.Cmp(big.NewInt(0)) > 0 && target.Number.Cmp(targetNumber) > 0 {
		// target이 targetNumber보다 같아지면 break, 즉 targetNumber 높이의 헤더를 구하기 위함
		target = chain.GetHeader(target.ParentHash, target.Number.Uint64()-1)
	}

	if target == nil {
		fmt.Println("BSRR.getAncestor() / target is nil !")
		return &types.Header{}, false
	}

	return target, chain.HasBlockAndState(target.Hash(), target.Number.Uint64())
}

/*
[BERITH]
Returns the target block to determine the miner for a given parent header.
1) [0 ~ epoch-1]      : target == block number 0(genesis block)
2) [epoch ~ 2epoch-1] : target == epoch block number
3) [2epoch ~ ...)     : target == block number - epoch

주어진 부모 헤더의 마이너를 결정하기 위한 타겟 블록을 반환한다.
*/
func (c *BSRR) getStakeTargetBlock(chain consensus.ChainReader, parent *types.Header) (*types.Header, bool) {
	if parent == nil {
		return &types.Header{}, false
	}

	var targetNumber uint64
	blockNumber := parent.Number.Uint64()
	d := blockNumber / c.config.Epoch
	fmt.Println("BSRR.getStakeTargetBlock / d : ", blockNumber/c.config.Epoch, "Epoch : ", c.config.Epoch)
	// 블록 높이가 360 이상이면 여기로
	if d > 1 {
		// 부모 블록의 1에포크 전 블록헤더의 블록과 State 존재 유무을 구한다. (current - (1+epoch))
		return c.getAncestor(chain, int64(c.config.Epoch), parent)
	}

	switch d {
	case 0:
		targetNumber = 0
	case 1:
		targetNumber = c.config.Epoch
	}

	target := chain.GetHeaderByNumber(targetNumber)
	if target != nil {
		return target, chain.HasBlockAndState(target.Hash(), targetNumber)
	}
	return target, false
}

// SealHash returns the hash of a block prior to it being sealed.
// SealHash는 블록이 sealing되기 전에 블록의 해시를 반환한다.
func (c *BSRR) SealHash(header *types.Header) common.Hash {
	return sigHash(header)
}

/*
[BERITH]
Method to return the difficulty and rank when creating a block for a given address
1) [0, epoch] -> After extraction from extra data of genesis block ==> return (1234,1) or (0, -1)
2) [epoch+1, ~) -> Returns the staking list based on the target block (diff, rank) ==> return (diff,rank) or (0, -1)

블록을 생성할 때 주어진 주소의 난이도와 랭크를 반환하는 함수이다.
[0, epoch] ->  제네시스 블록의 엑스트라 데이터로부터의 추출(1234,1)
[epoch+1, ~] -> 타겟블록의 스테이킹 리스트 반환
*/
func (c *BSRR) calcDifficultyAndRank(signer common.Address, chain consensus.ChainReader, time uint64, target *types.Header) (*big.Int, int) {
	fmt.Println("CalcDifficultyAndRank / Target : ", target.Number.Int64())
	// extract diff and rank from genesis's extra data
	if target.Number.Cmp(big.NewInt(0)) == 0 {
		log.Info("default difficulty and rank", "diff", diffWithoutStaker, "rank", 1)
		return big.NewInt(diffWithoutStaker), 1
	}

	stks, err := c.getStakers(chain, target.Number.Uint64(), target.Hash())
	if err != nil {
		log.Error("failed to get stakers", "err", err.Error())
		return big.NewInt(0), -1
	}

	stateDB, err := chain.StateAt(target.Root)
	if err != nil {
		log.Error("failed to get state", "err", err.Error())
		return big.NewInt(0), -1
	}

	results := selection.SelectBlockCreator(chain.Config(), target.Number.Uint64(), target.Hash(), stks, stateDB)

	//후보자가 10000명 이하라면, ForkFactor가 1.0이기 때문에 그대로 반환됨
	max := c.getMaxMiningCandidates(len(results))

	if results[signer].Rank > max {
		log.Warn("out of rank", "hash", target.Hash().Hex(), "rank", results[signer].Rank, "max", max)
		return big.NewInt(0), -1
	}
	fmt.Printf("%v's Rank : %v\n", signer.Hex(), results[signer].Rank)
	return results[signer].Score, results[signer].Rank
}

/*
[Berith]
Returns the delay time for block sealing according to the given rank.
Always returns a value greater than or equal to 0
*/
func (c *BSRR) getDelay(rank int) (time.Duration, error) {
	if rank <= 1 {
		fmt.Println("getDelay / return 0s")
		return time.Duration(0), nil
	}

	// Delay time for each group
	groupOrder, err := c.rankGroup.GetGroupOrder(rank)
	if err != nil {
		return time.Duration(0), err
	}
	delay := time.Duration(groupOrder-1) * groupDelay

	// Delay time in group
	startRank, _, err := c.rankGroup.GetGroupRange(groupOrder)
	if err != nil {
		return time.Duration(0), err
	}
	delay += time.Duration(rank-startRank) * termDelay
	fmt.Printf("GetDelay / Rank : %v , Delay : %v\n", rank, delay)
	return delay, nil
}

// Close implements consensus.Engine. It's a noop for clique as there are no background threads.
func (c *BSRR) Close() error {
	return nil
}

func getReward(config *params.ChainConfig, header *types.Header) *big.Int {
	const (
		blockNumberAt1Year         = 3150000 // If a block is created every 10 seconds, this number of the block created at the time of 1 year.
		defaultReward              = 26      // The basic reward is 26 tokens.
		additionalReward           = 5       // Additional rewards are paid for one year.
		blockSectionDivisionNumber = 7370000 // Reference value for dividing a block into 50 sections
		groupingValue              = 0.5     // Constant for grouping two groups to have the same Reward Subtract
	)

	number := header.Number.Uint64()
	// Reward after a specific block
	if number < config.Bsrr.Rewards.Uint64() {
		return big.NewInt(0)
	}

	// Value to correct Reward when block creation time is changed.
	correctionValue := float64(config.Bsrr.Period) / common.DefaultBlockCreationSec
	correctedBlockNumber := float64(number) * correctionValue

	var addtional float64 = 0
	if correctedBlockNumber <= blockNumberAt1Year {
		addtional = additionalReward
	}

	/*
		[Berith]
		The reward payment decreases as the time increases, and for this purpose, the block is divided into 50 sections.
		The same amount is deducted for every two sections.
	*/
	reward := (defaultReward - math.Round(correctedBlockNumber/blockSectionDivisionNumber)*groupingValue + addtional) * correctionValue
	if reward <= 0 {
		return big.NewInt(0)
	}
	fmt.Println("getReward() / Reward : ", reward)
	temp := reward * 1e+10
	return new(big.Int).Mul(big.NewInt(int64(temp)), big.NewInt(1e+8))
}

// AccumulateRewards credits the coinbase of the given block with the mining
// reward.
func (c *BSRR) accumulateRewards(chain consensus.ChainReader, state *state.StateDB, header *types.Header) {
	fmt.Println("BSRR.accumulateRewards() 호출")
	config := chain.Config()
	state.AddBehindBalance(header.Coinbase, header.Number, getReward(config, header))

	// Get the block constructor of the past point.
	target, exist := c.getAncestor(chain, int64(config.Bsrr.Epoch), header)
	if !exist {
		return
	}

	signers, err := c.getSigners(chain, target)
	if err != nil {
		return
	}

	//all node block result
	for _, addr := range signers {
		behind, err := state.GetFirstBehindBalance(addr)
		fmt.Printf("%v's Behind balance : %v\n", addr.Hex(), behind.Balance)
		if err != nil {
			continue
		}

		target := new(big.Int).Add(behind.Number, new(big.Int).SetUint64(config.Bsrr.Epoch))
		if header.Number.Cmp(target) == -1 {
			continue
		}

		if behind.Balance.Cmp(new(big.Int).SetInt64(int64(0))) != 1 {
			continue
		}

		//bihind --> main
		state.AddBalance(addr, behind.Balance)

		state.RemoveFirstBehindBalance(addr)
	}
}

func (c *BSRR) supportBIP1(chain consensus.ChainReader, parent *types.Header, stks staking.Stakers) (staking.Stakers, error) {
	st, err := chain.StateAt(parent.Root)
	if err != nil {
		return nil, err
	}

	for _, addr := range stks.AsList() {
		if st.GetStakeBalance(addr).Cmp(c.config.StakeMinimum) < 0 {
			stks.Remove(addr)
		}
	}

	bytes, err := json.Marshal(stks)
	if err != nil {
		return nil, err
	}
	c.cache.Add(parent.Hash(), bytes)
	err = c.stakingDB.Commit(parent.Hash().Hex(), stks)
	if err != nil {
		return nil, err
	}

	return stks, nil
}

//[BERITH] Method to call stakingList from cache or db
func (c *BSRR) getStakers(chain consensus.ChainReader, number uint64, hash common.Hash) (staking.Stakers, error) {
	var (
		list   staking.Stakers
		blocks []*types.Block
	)

	prevNum := number
	prevHash := hash

	//[BERITH] Find the nearest StakingList in the input block.
	for list == nil {
		//[BERITH] When StakingList stored in cache is found
		if val, ok := c.cache.Get(prevHash); ok {
			bytes := val.([]byte)

			if err := json.Unmarshal(bytes, &list); err == nil {
				break
			}
			list = nil
			c.cache.Remove(prevHash)
		}

		//[BERITH] StakingList is not saved
		if prevNum == 0 {
			list = c.stakingDB.NewStakers()
			break
		}

		//[BERITH] When finding StakingList stored in DB
		var err error
		list, err = c.stakingDB.GetStakers(prevHash.Hex())
		if err == nil {
			break
		}
		list = nil

		block := chain.GetBlock(prevHash, prevNum)
		if block == nil {
			return nil, errors.New("unknown anccesstor")
		}

		blocks = append(blocks, block)
		prevNum--
		prevHash = block.ParentHash()
	}
	if list != nil {
		fmt.Println("Stakers : ", list.AsList())
	} else {
		fmt.Println("Stakers : Nil")
	}

	if len(blocks) == 0 {
		return list, nil
	}

	for i := 0; i < len(blocks)/2; i++ {
		blocks[i], blocks[len(blocks)-1-i] = blocks[len(blocks)-1-i], blocks[i]
	}

	err := c.checkBlocks(chain, list, blocks)
	if err != nil {
		return nil, err
	}

	bytes, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}
	c.cache.Add(hash, bytes)
	err = c.stakingDB.Commit(hash.Hex(), list)
	if err != nil {
		return nil, err
	}

	return list, nil
}

//[BERITH] Method to check the block and set the value in stakingList
func (c *BSRR) checkBlocks(chain consensus.ChainReader, stks staking.Stakers, blocks []*types.Block) error {
	if len(blocks) == 0 {
		return nil
	}

	for _, block := range blocks {
		if err := c.setStakersWithTxs(nil, chain, stks, block.Transactions(), block.Header()); err != nil {
			return err
		}
	}

	return nil
}

//[BERITH] Method to examine transaction array and set value in stakingList
func (c *BSRR) setStakersWithTxs(state *state.StateDB, chain consensus.ChainReader, stks staking.Stakers, txs []*types.Transaction, header *types.Header) error {
	number := header.Number

	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)

	if parent == nil {
		return consensus.ErrUnknownAncestor
	}

	prevState, err := chain.StateAt(parent.Root)

	if err != nil {
		return errMissingState
	}

	stkChanged := make(map[common.Address]bool)

	for _, tx := range txs {
		msg, err := tx.AsMessage(types.MakeSigner(chain.Config(), number))
		if err != nil {
			return err
		}

		// General Transaction
		if msg.Base() == types.Main && msg.Target() == types.Main {
			continue
		}

		//[BERITH] 2019-09-03
		// Fix to save the last staking block number
		// Stake or Unstake in case of not normal Tx
		if chain.Config().IsBIP1(number) && msg.Base() == types.Stake && msg.Target() == types.Main {
			stkChanged[msg.From()] = false
		} else if msg.Base() == types.Main && msg.Target() == types.Stake {
			stkChanged[msg.From()] = true
		}
	}

	for addr, isAdd := range stkChanged {
		if state != nil {
			point := big.NewInt(0)
			currentStkBal := state.GetStakeBalance(addr)
			if currentStkBal.Cmp(big.NewInt(0)) == 1 {
				currentStkBal = new(big.Int).Div(currentStkBal, common.UnitForBer)
				prevStkBal := new(big.Int).Div(prevState.GetStakeBalance(addr), common.UnitForBer)
				additionalStkBal := new(big.Int).Sub(currentStkBal, prevStkBal)
				currentBlock := header.Number
				lastStkBlock := new(big.Int).Set(state.GetStakeUpdated(addr))
				period := c.config.Period
				point = staking.CalcPointBigint(prevStkBal, additionalStkBal, currentBlock, lastStkBlock, period)
			}
			state.SetPoint(addr, point)
		}

		if isAdd {
			stks.Put(addr)
		} else {
			stks.Remove(addr)
		}

	}
	return nil
}

type signers []common.Address

func (s signers) signersMap() map[common.Address]struct{} {
	result := make(map[common.Address]struct{})
	for _, signer := range s {
		result[signer] = struct{}{}
	}
	return result
}

//[BERITH] Method that returns a list of accounts that can create a block of the received block number
// 1) [0, epoch number) -> Return signers extracted from extra data of genesis
// 2) [epoch nunber ~ ) -> Return signers extracted from staking list
func (c *BSRR) getSigners(chain consensus.ChainReader, target *types.Header) (signers, error) {
	// extract signers from genesis block's extra data if block number equals to 0
	if target.Number.Cmp(big.NewInt(0)) == 0 {
		return c.getSignersFromExtraData(target)
	}

	// extract signers from genesis block's extra data if block number is less than epoch
	if target.Number.Cmp(big.NewInt(int64(c.config.Epoch))) < 0 {
		return c.getSignersFromExtraData(chain.GetHeaderByNumber(0))
	}

	// extract signers from staking list if block number is greater than or equals to epoch
	list, err := c.getStakers(chain, target.Number.Uint64(), target.Hash())
	if err != nil {
		return nil, errors.New("failed to get staking list")
	}

	result := list.AsList()
	if len(result) == 0 {
		return make([]common.Address, 0), nil
	}
	return result, nil
}

//[BERITH] Returns signers from the extra data field.
func (c *BSRR) getSignersFromExtraData(header *types.Header) (signers, error) {
	n := (len(header.Extra) - extraVanity - extraSeal) / common.AddressLength
	if n < 1 {
		return nil, errExtraSigners
	}

	signers := make([]common.Address, n)
	for i := 0; i < len(signers); i++ {
		copy(signers[i][:], header.Extra[extraVanity+i*common.AddressLength:])
	}
	return signers, nil
}

// [BERITH] Returns the number of candidates who can create a block at a given number of stakers.
func (c *BSRR) getMaxMiningCandidates(holders int) int {
	if holders == 0 {
		return 0
	}

	// (0,1) 범위는 모두 1
	t := int(math.Round(c.config.ForkFactor * float64(holders)))
	if t == 0 {
		t = 1
	}

	if t > selection.MaxMiner {
		t = selection.MaxMiner
	}
	return t
}

/*
[BERITH]
Elected probability return function
*/
func (c *BSRR) getJoinRatio(stks staking.Stakers, address common.Address, hash common.Hash, blockNumber uint64, states *state.StateDB) (float64, error) {
	var total float64
	var n float64

	for _, stk := range stks.AsList() {
		point := float64(states.GetPoint(stk).Int64())
		if address == stk {
			n = point
		}
		total += point
	}

	if total == 0 {
		return 0, nil
	}

	return n / total, nil
}

// APIs implements consensus.Engine, returning the user facing RPC API to allow
// controlling the signer voting.
func (c *BSRR) APIs(chain consensus.ChainReader) []rpc.API {
	return []rpc.API{{
		Namespace: "bsrr",
		Version:   "1.0",
		Service:   &API{chain: chain, bsrr: c},
		Public:    false,
	}}
}
