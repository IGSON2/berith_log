package selection

import (
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"testing"

	"github.com/BerithFoundation/berith-chain/common"
	"github.com/BerithFoundation/berith-chain/params"
)

func TestQueue(t *testing.T) {

	var tempCandidates []Candidate
	for i := 0; i < 5; i++ {
		tempCandidates = append(tempCandidates, Candidate{address: [20]byte{uint8(i + 1)}, point: uint64((i + 1) * 5)})
		tempCandidates[i].val += tempCandidates[i].val + tempCandidates[i].point
	}
	var total = tempCandidates[len(tempCandidates)-1].val
	var cs = &Candidates{total: total, selections: tempCandidates}
	candidateCount := len(cs.selections)
	queue := new(Queue).setQueueAsCandidates(candidateCount)
	result := make(VoteResults)

	currentElectScore := maxElectScore
	electScoreGap := (maxElectScore - minElectScore) / int64(candidateCount)

	// Block number is used as a seed so that all nodes have the same random value
	rand.Seed(cs.GetSeed(params.MainnetChainConfig, 1000000))

	err := queue.enqueue(Range{
		min:   0,
		max:   cs.total,
		start: 0,
		end:   candidateCount,
	})
	fmt.Println("Enqueue\t", queue.storage)
	if err != nil {
		fmt.Println(err)
		t.Errorf(err.Error())
	}

	for count := 1; count <= MaxMiner && queue.front != queue.rear; count++ {
		r, err := queue.dequeue()
		fmt.Println("Dequeue\t", queue.storage)
		if err != nil {
			fmt.Println(err)
			t.Errorf(err.Error())
		}
		account := r.testbinarySearch(queue, cs)
		result[account] = VoteResult{
			Score: big.NewInt(currentElectScore + int64(cs.ts)),
			Rank:  count,
		}
		currentElectScore -= electScoreGap
	}
	fmt.Println(result)
}

func (r Range) testbinarySearch(q *Queue, cs *Candidates) common.Address {
	if r.end-r.start <= 1 {
		return cs.selections[r.start].address
	}

	random := uint64(rand.Int63n(int64(r.max-r.min))) + r.min
	start := r.start
	end := r.end
	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf("Random : %d\tstart : %d\tend : %d\n", random, start, end)
	fmt.Println(strings.Repeat("=", 80))
	for {
		target := (start + end) / 2
		a := r.min
		if target > 0 {
			a = cs.selections[target-1].val
		}
		b := cs.selections[target].val
		fmt.Printf("Target : %d\tstart : %d\tend : %d\t\ta : %d\tb : %d\n", target, start, end, a, b)

		if random >= a && random <= b {
			if r.start != target {
				q.enqueue(Range{
					min:   r.min,
					max:   a - 1,
					start: r.start,
					end:   target,
				})
				fmt.Printf("Enqueue\tstart (%d) != target (%d),\t storage : %v\n", start, target, q.storage)

			}
			if target+1 != r.end {
				q.enqueue(Range{
					min:   b + 1,
					max:   r.max,
					start: target + 1,
					end:   r.end,
				})
				fmt.Printf("Enqueue\tend (%d) != target+1 (%d),\t storage : %v\n", end, target+1, q.storage)

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
