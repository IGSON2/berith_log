package selection

import (
	"math/big"

	"github.com/BerithFoundation/berith-chain/common"
)

/*
[Berith]
Object that stores voting results for election
*/
type VoteResults map[common.Address]VoteResult

type VoteResult struct {
	Score *big.Int `json:"score"`
	Rank  int      `json:"rank"`
}
