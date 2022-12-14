package selection

import (
	"math/rand"

	"github.com/BerithFoundation/berith-chain/common"
)

type Range struct {
	min   uint64
	max   uint64
	start int
	end   int
}

/**
[BERITH]
BinarySearch the Random value in width units.
*/
func (r Range) binarySearch(q *Queue, cs *Candidates) common.Address {
	if r.end-r.start <= 1 { //이전 레인지의 결과 중 start와 end 값의 차이가 1 이하라는 뜻은 탐색이 필요 없다는 것
		return cs.selections[r.start].address
	}

	random := uint64(rand.Int63n(int64(r.max-r.min))) + r.min
	start := r.start
	end := r.end
	for {
		target := (start + end) / 2
		a := r.min
		if target > 0 {
			a = cs.selections[target-1].val
		}
		b := cs.selections[target].val

		if random >= a && random <= b {
			if r.start != target {
				q.enqueue(Range{
					min:   r.min,
					max:   a - 1,
					start: r.start,
					end:   target,
				})
			}
			if target+1 != r.end {
				q.enqueue(Range{
					min:   b + 1,
					max:   r.max,
					start: target + 1,
					end:   r.end,
				})
			}
			return cs.selections[target].address
		}

		if random < a {
			end = target
		} else {
			start = target + 1
		}
	}
}
