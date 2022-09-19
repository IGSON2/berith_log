package types

import (
	"math/big"

	"github.com/BerithFoundation/berith-chain/common"
)

type TxdataInterface interface {
	MarshalJSON() ([]byte, error)
	UnmarshalJSON(input []byte) error
	getV() *big.Int
	getR() *big.Int
	getS() *big.Int
	getPayload() []byte
	getGasLimit() uint64
	getPrice() *big.Int
	getAmount() *big.Int
	getAccountNonce() uint64
	getBase() JobWallet
	getTarget() JobWallet
	getRecipient() *common.Address
}

type originTxdata struct {
	// From의 Nonce
	AccountNonce uint64          `json:"nonce"    gencodec:"required"`
	Price        *big.Int        `json:"gasPrice" gencodec:"required"`
	GasLimit     uint64          `json:"gas"      gencodec:"required"`
	Recipient    *common.Address `json:"to"       rlp:"nil"` // nil means contract creation
	Amount       *big.Int        `json:"value"    gencodec:"required"`
	Payload      []byte          `json:"input"    gencodec:"required"`

	// Signature values
	V *big.Int `json:"v" gencodec:"required"`
	R *big.Int `json:"r" gencodec:"required"`
	S *big.Int `json:"s" gencodec:"required"`

	// This is only used when marshaling to JSON.
	Hash *common.Hash `json:"hash" rlp:"-"`
}

func (o *originTxdata) MarshalJSON() ([]byte, error)     { return nil, nil }
func (o *originTxdata) UnmarshalJSON(input []byte) error { return nil }
func (o *originTxdata) getV() *big.Int                   { return o.V }
func (o *originTxdata) getR() *big.Int                   { return o.R }
func (o *originTxdata) getS() *big.Int                   { return o.S }
func (o *originTxdata) getPayload() []byte               { return o.Payload }
func (o *originTxdata) getGasLimit() uint64              { return o.GasLimit }
func (o *originTxdata) getPrice() *big.Int               { return o.Price }
func (o *originTxdata) getAmount() *big.Int              { return o.Amount }
func (o *originTxdata) getAccountNonce() uint64          { return o.AccountNonce }
func (o *originTxdata) getBase() JobWallet               { return JobWallet(1) }
func (o *originTxdata) getTarget() JobWallet             { return JobWallet(1) }
func (o *originTxdata) getRecipient() *common.Address    { return o.Recipient }
