package staking

import (
	"fmt"
	"math/big"
	"testing"
)

/*
[BERITH]
Election point calculation test
*/
func TestCalcPoint(t *testing.T) {
	add_stake := big.NewInt(80)
	prev_stake := big.NewInt(1000)
	new_block := big.NewInt(7200021)
	stake_block := big.NewInt(5000)
	perioid := uint64(360)
	result := CalcPointBigint(prev_stake, add_stake, new_block, stake_block, perioid)

	fmt.Println(result)
}
