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
	var testInt = []int{0, 1000, -999}
	for _, myI := range testInt {
		add_stake := big.NewInt(int64(myI))
		prev_stake := big.NewInt(1000)
		new_block := big.NewInt(7200021)
		stake_block := big.NewInt(1)
		perioid := uint64(360)
		result := CalcPointBigint(prev_stake, add_stake, new_block, stake_block, perioid)
		fmt.Printf("addStake : %v, Result : %v\n", add_stake, result)
	}
}
