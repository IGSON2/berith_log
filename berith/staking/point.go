/**
[BERITH]
- To calculate Selection Point
- Formula when block creation time is 10 seconds
- When generating a block every 10 seconds, 3600000 blocks are generated per year.
**/

package staking

import (
	"math/big"

	"github.com/BerithFoundation/berith-chain/common"
)

const (
	BlockYear = 3600000 // When generating a block every 10 seconds, 3600000 blocks are generated per year.
)

func CalcPointBigint(prevStake, addStake, nowBlock, stakeBlock *big.Int, period uint64) *big.Int {
	//공식이 10초 단위 이기때문에 맞추기 위함 (perioid 를 제네시스로 변경하면 자동으로 변경되기 위함)
	correctionValue := float64(period) / common.DefaultBlockCreationSec // Value for correction when block creation time is different from the standard
	referenceBlock := int64(BlockYear / correctionValue)

	//ratio := (b * 100)  / (bb + s) <- 100은 소수점 처리
	// 현재 블록 높이 / {이전 스테이킹 블록 높이 + (1년 채굴 블록량 / 10)}
	// [genesis]....[prevStk]..............[corVal].....[curStk]			 <- 스테이킹 간격이 넓어 corVal을 더해도 ratio가 100이 안됨
	// [genesis].........................[prevStk]......[curStk]....[corVal] <- ratio가 100이 넘는 경우
	ratio := new(big.Int).Mul(nowBlock, big.NewInt(100))
	ratio.Div(ratio, new(big.Int).Add(big.NewInt(referenceBlock), stakeBlock))

	if ratio.Cmp(big.NewInt(100)) == 1 {
		ratio = big.NewInt(100)
	}

	//advantage := prevStake * (prevStake / (prevStake + addStake)) * ratio / 100
	temp1 := new(big.Int).Div(prevStake, new(big.Int).Add(prevStake, addStake))
	temp2 := new(big.Int).Mul(prevStake, temp1)
	temp3 := new(big.Int).Mul(temp2, ratio)
	advantage := new(big.Int).Div(temp3, big.NewInt(100))

	//selectionPoint := prevStake + advantage + addStake
	temp1 = new(big.Int).Add(prevStake, advantage)
	selectionPoint := new(big.Int).Add(temp1, addStake)

	return selectionPoint
}
