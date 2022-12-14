// Copyright 2014 The go-ethereum Authors
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

package vm

import (
	"fmt"
	"hash"
	"sync/atomic"

	"github.com/BerithFoundation/berith-chain/common"
	"github.com/BerithFoundation/berith-chain/common/math"
	"github.com/BerithFoundation/berith-chain/log"
	"github.com/BerithFoundation/berith-chain/params"
)

// Config are the configuration options for the Interpreter
type Config struct {
	// Debug enabled debugging Interpreter options
	Debug bool
	// Tracer is the op code logger
	Tracer Tracer
	// NoRecursion disabled Interpreter call, callcode,
	// delegate call and create.
	NoRecursion bool
	// Enable recording of SHA3/keccak preimages
	EnablePreimageRecording bool
	// JumpTable contains the EVM instruction table. This
	// may be left uninitialised and will be set to the default
	// table.
	JumpTable [256]operation

	// Type of the EWASM interpreter
	EWASMInterpreter string
	// Type of the EVM interpreter
	EVMInterpreter string

	ExtraEips []int // Additional EIPS that are to be enabled

}

// Interpreter is used to run Berith based contracts and will utilise the
// passed environment to query external sources for state information.
// The Interpreter will run the byte code VM based on the passed
// configuration.
type Interpreter interface {
	// Run loops and evaluates the contract's code with the given input data and returns
	// the return byte-slice and an error if one occurred.
	Run(contract *Contract, input []byte, static bool) ([]byte, error)
	// CanRun tells if the contract, passed as an argument, can be
	// run by the current interpreter. This is meant so that the
	// caller can do something like:
	//
	// ```golang
	// for _, interpreter := range interpreters {
	//   if interpreter.CanRun(contract.code) {
	//     interpreter.Run(contract.code, input)
	//   }
	// }
	// ```
	CanRun([]byte) bool
}

// keccakState wraps sha3.state. In addition to the usual hash methods, it also supports
// Read to get a variable amount of data from the hash state. Read is faster than Sum
// because it doesn't copy the internals state, but also modifies the internals state.
type keccakState interface {
	hash.Hash
	Read([]byte) (int, error)
}

// EVMInterpreter represents an EVM interpreter
type EVMInterpreter struct {
	evm      *EVM
	cfg      Config
	gasTable params.GasTable

	hasher    keccakState // Keccak256 hasher instance shared across opcodes
	hasherBuf common.Hash // Keccak256 hasher result array shared aross opcodes

	readOnly   bool   // Whether to throw on stateful modifications
	returnData []byte // Last CALL's return data for subsequent reuse
}

// NewEVMInterpreter returns a new instance of the Interpreter.
func NewEVMInterpreter(evm *EVM, cfg Config) *EVMInterpreter {
	// We use the STOP instruction whether to see
	// the jump table was initialised. If it was not
	// we'll set the default jump table.
	log.Warn("NewEVMInterpreter", "JumpTable[STOP]", cfg.JumpTable[STOP])
	if cfg.JumpTable[STOP].execute == nil {
		switch {
		case evm.ChainConfig().IsConstantinople(evm.BlockNumber):
			cfg.JumpTable = constantinopleInstructionSet
		case evm.ChainConfig().IsByzantium(evm.BlockNumber):
			cfg.JumpTable = byzantiumInstructionSet
		case evm.ChainConfig().IsHomestead(evm.BlockNumber):
			cfg.JumpTable = homesteadInstructionSet
		default:
			cfg.JumpTable = frontierInstructionSet
		}
	}
	cfg.ExtraEips = append(cfg.ExtraEips, []int{2929, 2200, 1884, 1344}...)
	log.Warn("NewEVMInterpreter", "ExtraEips", cfg.ExtraEips)
	for i, eip := range cfg.ExtraEips {
		if err := EnableEIP(eip, &cfg.JumpTable); err != nil {
			// Disable it, so caller can check if it's activated or not
			cfg.ExtraEips = append(cfg.ExtraEips[:i], cfg.ExtraEips[i+1:]...)
			log.Error("EIP activation failed", "eip", eip, "error", err)
		}
	}

	return &EVMInterpreter{
		evm:      evm,
		cfg:      cfg,
		gasTable: evm.ChainConfig().GasTable(evm.BlockNumber),
	}
}

func (in *EVMInterpreter) enforceRestrictions(op OpCode, operation operation, stack *Stack) error {
	if in.evm.chainRules.IsByzantium {
		if in.readOnly {
			// If the interpreter is operating in readonly mode, make sure no
			// state-modifying operation is performed. The 3rd stack item
			// for a call operation is the value. Transferring value from one
			// account to the others means the state is modified and should also
			// return with an error.
			if operation.writes || (op == CALL && stack.Back(2).BitLen() > 0) {
				return errWriteProtection
			}
		}
	}
	return nil
}

// Run loops and evaluates the contract's code with the given input data and returns
// the return byte-slice and an error if one occurred.
//
// It's important to note that any errors returned by the interpreter should be
// considered a revert-and-consume-all-gas operation except for
// errExecutionReverted which means revert-and-keep-gas-left.
func (in *EVMInterpreter) Run(contract *Contract, input []byte, readOnly bool) (ret []byte, err error) {
	log.Warn("EVMInterpreter.Run ??????")

	// Increment the call depth which is restricted to 1024
	in.evm.depth++
	defer func() { in.evm.depth-- }()

	// Make sure the readOnly is only set if we aren't in readOnly yet.
	// This makes also sure that the readOnly flag isn't removed for child calls.
	if readOnly && !in.readOnly {
		in.readOnly = true
		defer func() { in.readOnly = false }()
	}

	// Reset the previous call's return data. It's unimportant to preserve the old buffer
	// as every returning call will return new data anyway.
	in.returnData = nil

	// Don't bother with the execution if there's no code.
	if len(contract.Code) == 0 {
		log.Warn("EVMInterpreter.Run / length of contract code is 0 ")
		return nil, nil
	}

	var (
		op    OpCode        // current opcode
		mem   = NewMemory() // bound memory
		stack = newstack()  // local stack
		// For optimisation reason we're using uint64 as the program counter.
		// It's theoretically possible to go above 2^64. The YP defines the PC
		// to be uint256. Practically much less so feasible.
		pc   = uint64(0) // program counter
		cost uint64
		// copies used by tracer
		pcCopy  uint64 // needed for the deferred Tracer
		gasCopy uint64 // for Tracer to log gas remaining before execution
		logged  bool   // deferred Tracer should ignore already logged steps
	)
	// Don't move this deferrred function, it's placed before the capturestate-deferred method,
	// so that it get's executed _after_: the capturestate needs the stacks before
	// they are returned to the pools
	defer func() {
		returnStack(stack)
	}()
	contract.Input = input

	if in.cfg.Debug {
		defer func() {
			if err != nil {
				if !logged {
					in.cfg.Tracer.CaptureState(in.evm, pcCopy, op, gasCopy, cost, mem, stack, contract, in.evm.depth, err)
				} else {
					in.cfg.Tracer.CaptureFault(in.evm, pcCopy, op, gasCopy, cost, mem, stack, contract, in.evm.depth, err)
				}
			}
		}()
	}
	// The Interpreter main run loop (contextual). This loop runs until either an
	// explicit STOP, RETURN or SELFDESTRUCT is executed, an error occurred during
	// the execution of one of the operations or until the done flag is set by the
	// parent context.
	for atomic.LoadInt32(&in.evm.abort) == 0 {
		if in.cfg.Debug {
			// Capture pre-execution values for tracing.
			logged, pcCopy, gasCopy = false, pc, contract.Gas
		}

		// Get the operation from the jump table and validate the stack to ensure there are
		// enough stack items available to perform the operation.
		op = contract.GetOp(pc)
		// fmt.Println("Compiling..\t", "op=", op, "[", int(op), "]")

		operation := in.cfg.JumpTable[op]
		if &operation == nil {
			log.Error("EVMInterpreter.Run / Invalid op error ", "error", err, "CodeByte", contract.Code)
			return nil, fmt.Errorf("invalid opcode 0x%x", int(op))
		}

		if stack.len() > operation.maxStack {
			log.Error("EVMInterpreter.Run / Stack Overflow", "error", err, "Stack.len()", stack.len(), "Operation", operation)
			return nil, err
		} else if stack.len() < operation.minStack {
			log.Error("EVMInterpreter.Run / Stack Underflow", "error", err)
			return nil, err
		}
		// If the operation is valid, enforce and write restrictions
		if err := in.enforceRestrictions(op, operation, stack); err != nil {
			log.Error("EVMInterpreter.Run / Enforce Restriction error", "error", err)
			return nil, err
		}

		var memorySize uint64
		// calculate the new memory size and expand the memory to fit
		// the operation
		if operation.memorySize != nil {
			memSize, overflow := operation.memorySize(stack)
			if overflow {
				log.Error("Overflow !")
				return nil, errGasUintOverflow
			}
			// memory is expanded in words of 32 bytes. Gas
			// is also calculated in words.
			if memorySize, overflow = math.SafeMul(toWordSize(memSize), 32); overflow {
				log.Error("Gas Overflow !")
				return nil, errGasUintOverflow
			}
		}
		// consume the gas and return an error if not enough gas is available.
		// cost is explicitly set so that the capture state defer method can get the proper cost
		//
		// ????????? ???????????? ????????? ????????? ?????? ?????? ????????? ????????????.
		//
		// [Berith]
		// cost, err = operation.gasCost(in.gasTable, in.evm, contract, stack, mem, memorySize)
		// if err != nil || !contract.UseGas(cost) {
		// 	return nil, ErrOutOfGas
		// }
		if memorySize > 0 {
			mem.Resize(memorySize)
		}

		// [Berith]
		// cost => operation.constantGas
		if in.cfg.Debug {
			in.cfg.Tracer.CaptureState(in.evm, pc, op, gasCopy, operation.constantGas, mem, stack, contract, in.evm.depth, err)
			logged = true
		}

		// execute the operation
		res, err := operation.execute(&pc, in, contract, mem, stack)

		// if the operation clears the return data (e.g. it has returning data)
		// set the last return to the result of the operation.
		if operation.returns {
			in.returnData = res
		}

		switch {
		case err != nil:
			log.Warn("EVMInterpreter.Run / Error occured after excute code", err)
			return nil, err
		case operation.reverts:
			log.Warn("EVMInterpreter.Run / revert", "operation", operation)
			return res, errExecutionReverted
		case operation.halts:
			log.Warn("EVMInterpreter.Run / halts", "operation", operation)
			return res, nil
		case !operation.jumps:
			pc++
		}
	}
	return nil, nil
}

// CanRun tells if the contract, passed as an argument, can be
// run by the current interpreter.
func (in *EVMInterpreter) CanRun(code []byte) bool {
	return true
}
