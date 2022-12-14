// Copyright 2018 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.
package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"berith-chain/internals/berithapi"

	"github.com/BerithFoundation/berith-chain/accounts"
	"github.com/BerithFoundation/berith-chain/accounts/keystore"
	"github.com/BerithFoundation/berith-chain/common"
	"github.com/BerithFoundation/berith-chain/common/hexutil"
	"github.com/BerithFoundation/berith-chain/core/types"
	"github.com/BerithFoundation/berith-chain/rlp"
	"github.com/BerithFoundation/berith-chain/signer/storage"
)

// Used for testing
type headlessUi struct {
	approveCh chan string // to send approve/deny
	inputCh   chan string // to send password
}

func (ui *headlessUi) OnInputRequired(info UserInputRequest) (UserInputResponse, error) {
	return UserInputResponse{}, errors.New("not implemented")
}

func (ui *headlessUi) OnSignerStartup(info StartupInfo) {
}

func (ui *headlessUi) OnApprovedTx(tx berithapi.SignTransactionResult) {
	fmt.Printf("OnApproved()\n")
}

func (ui *headlessUi) ApproveTx(request *SignTxRequest) (SignTxResponse, error) {

	switch <-ui.approveCh {
	case "Y":
		return SignTxResponse{request.Transaction, true}, nil
	case "M": // modify
		// The headless UI always modifies the transaction
		old := big.Int(request.Transaction.Value)
		newVal := big.NewInt(0).Add(&old, big.NewInt(1))
		request.Transaction.Value = hexutil.Big(*newVal)
		return SignTxResponse{request.Transaction, true}, nil
	default:
		return SignTxResponse{request.Transaction, false}, nil
	}
}

func (ui *headlessUi) ApproveSignData(request *SignDataRequest) (SignDataResponse, error) {
	if "Y" == <-ui.approveCh {
		return SignDataResponse{true, <-ui.approveCh}, nil
	}
	return SignDataResponse{false, ""}, nil
}

func (ui *headlessUi) ApproveExport(request *ExportRequest) (ExportResponse, error) {
	return ExportResponse{<-ui.approveCh == "Y"}, nil

}

func (ui *headlessUi) ApproveImport(request *ImportRequest) (ImportResponse, error) {
	if "Y" == <-ui.approveCh {
		return ImportResponse{true, <-ui.approveCh, <-ui.approveCh}, nil
	}
	return ImportResponse{false, "", ""}, nil
}

func (ui *headlessUi) ApproveListing(request *ListRequest) (ListResponse, error) {
	switch <-ui.approveCh {
	case "A":
		return ListResponse{request.Accounts}, nil
	case "1":
		l := make([]Account, 1)
		l[0] = request.Accounts[1]
		return ListResponse{l}, nil
	default:
		return ListResponse{nil}, nil
	}
}

func (ui *headlessUi) ApproveNewAccount(request *NewAccountRequest) (NewAccountResponse, error) {
	if "Y" == <-ui.approveCh {
		return NewAccountResponse{true, <-ui.approveCh}, nil
	}
	return NewAccountResponse{false, ""}, nil
}

func (ui *headlessUi) ShowError(message string) {
	//stdout is used by communication
	fmt.Fprintln(os.Stderr, message)
}

func (ui *headlessUi) ShowInfo(message string) {
	//stdout is used by communication
	fmt.Fprintln(os.Stderr, message)
}

func tmpDirName(t *testing.T) string {
	d, err := ioutil.TempDir("", "berith-keystore-test")
	if err != nil {
		t.Fatal(err)
	}
	d, err = filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func setup(t *testing.T) (*SignerAPI, *headlessUi) {
	db, err := NewFourbytes()
	if err != nil {
		t.Fatal(err.Error())
	}
	ui := &headlessUi{make(chan string, 20), make(chan string, 20)}
	am := StartClefAccountManager(tmpDirName(t), true, true, "")
	api := NewSignerAPI(am, 1337, true, ui, db, true, &storage.NoStorage{})
	return api, ui

}
func createAccount(ui *headlessUi, api *SignerAPI, t *testing.T) {
	ui.approveCh <- "Y"
	ui.inputCh <- "a_long_password"
	_, err := api.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Some time to allow changes to propagate
	time.Sleep(250 * time.Millisecond)
}

func failCreateAccountWithPassword(ui *headlessUi, api *SignerAPI, password string, t *testing.T) {

	ui.approveCh <- "Y"
	// We will be asked three times to provide a suitable password
	ui.inputCh <- password
	ui.inputCh <- password
	ui.inputCh <- password

	addr, err := api.New(context.Background())
	if err == nil {
		t.Fatal("Should have returned an error")
	}
	if addr != (accounts.Account{}) {
		t.Fatal("Empty address should be returned")
	}
}

func failCreateAccount(ui *headlessUi, api *SignerAPI, t *testing.T) {
	ui.approveCh <- "N"
	addr, err := api.New(context.Background())
	if err != ErrRequestDenied {
		t.Fatal(err)
	}
	if addr != (accounts.Account{}) {
		t.Fatal("Empty address should be returned")
	}
}

func list(ui *headlessUi, api *SignerAPI, t *testing.T) ([]common.Address, error) {
	ui.approveCh <- "A"
	return api.List(context.Background())

}

func TestNewAcc(t *testing.T) {
	api, control := setup(t)
	verifyNum := func(num int) {
		list, err := list(control, api, t)
		if err != nil {
			t.Errorf("Unexpected error %v", err)
		}
		if len(list) != num {
			t.Errorf("Expected %d accounts, got %d", num, len(list))
		}
	}
	// Testing create and create-deny
	createAccount(control, api, t)
	createAccount(control, api, t)
	failCreateAccount(control, api, t)
	failCreateAccount(control, api, t)
	createAccount(control, api, t)
	failCreateAccount(control, api, t)
	createAccount(control, api, t)
	failCreateAccount(control, api, t)

	verifyNum(4)

	// Fail to create this, due to bad password
	failCreateAccountWithPassword(control, api, "short", t)
	failCreateAccountWithPassword(control, api, "longerbutbad\rfoo", t)

	verifyNum(4)

	// Testing listing:
	// Listing one Account
	control.approveCh <- "1"
	list, err := api.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("List should only show one Account")
	}
	// Listing denied
	control.approveCh <- "Nope"
	list, err = api.List(context.Background())
	if len(list) != 0 {
		t.Fatalf("List should be empty")
	}
	if err != ErrRequestDenied {
		t.Fatal("Expected deny")
	}
}

func TestSignData(t *testing.T) {
	api, control := setup(t)
	//Create two accounts
	createAccount(control, api, t)
	createAccount(control, api, t)
	control.approveCh <- "1"
	list, err := api.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := common.NewMixedcaseAddress(list[0])

	control.approveCh <- "Y"
	control.approveCh <- "wrongpassword"
	h, err := api.Sign(context.Background(), a, []byte("EHLO world"))
	if h != nil {
		t.Errorf("Expected nil-data, got %x", h)
	}
	if err != keystore.ErrDecrypt {
		t.Errorf("Expected ErrLocked! %v", err)
	}
	control.approveCh <- "No way"
	h, err = api.Sign(context.Background(), a, []byte("EHLO world"))
	if h != nil {
		t.Errorf("Expected nil-data, got %x", h)
	}
	if err != ErrRequestDenied {
		t.Errorf("Expected ErrRequestDenied! %v", err)
	}
	control.approveCh <- "Y"
	control.approveCh <- "a_long_password"
	h, err = api.Sign(context.Background(), a, []byte("EHLO world"))
	if err != nil {
		t.Fatal(err)
	}
	if h == nil || len(h) != 65 {
		t.Errorf("Expected 65 byte signature (got %d bytes)", len(h))
	}
}
func mkTestTx(from common.MixedcaseAddress) SendTxArgs {
	to := common.NewMixedcaseAddress(common.HexToAddress("0x1337"))
	gas := hexutil.Uint64(21000)
	gasPrice := (hexutil.Big)(*big.NewInt(2000000000))
	value := (hexutil.Big)(*big.NewInt(1e18))
	nonce := (hexutil.Uint64)(0)
	data := hexutil.Bytes(common.Hex2Bytes("01020304050607080a"))
	tx := SendTxArgs{
		From:     from,
		To:       &to,
		Gas:      gas,
		GasPrice: gasPrice,
		Value:    value,
		Data:     &data,
		Nonce:    nonce}
	return tx
}

func TestSignTx(t *testing.T) {
	var (
		list      []common.Address
		res, res2 *berithapi.SignTransactionResult
		err       error
	)

	api, control := setup(t)
	createAccount(control, api, t)
	control.approveCh <- "A"
	list, err = api.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := common.NewMixedcaseAddress(list[0])

	methodSig := "test(uint)"
	tx := mkTestTx(a)

	control.approveCh <- "Y"
	control.approveCh <- "wrongpassword"
	res, err = api.SignTransaction(context.Background(), tx, &methodSig)
	if res != nil {
		t.Errorf("Expected nil-response, got %v", res)
	}
	if err != keystore.ErrDecrypt {
		t.Errorf("Expected ErrLocked! %v", err)
	}
	control.approveCh <- "No way"
	res, err = api.SignTransaction(context.Background(), tx, &methodSig)
	if res != nil {
		t.Errorf("Expected nil-response, got %v", res)
	}
	if err != ErrRequestDenied {
		t.Errorf("Expected ErrRequestDenied! %v", err)
	}
	control.approveCh <- "Y"
	control.approveCh <- "a_long_password"
	res, err = api.SignTransaction(context.Background(), tx, &methodSig)

	if err != nil {
		t.Fatal(err)
	}
	parsedTx := &types.Transaction{}
	rlp.Decode(bytes.NewReader(res.Raw), parsedTx)

	//The tx should NOT be modified by the UI
	if parsedTx.Value().Cmp(tx.Value.ToInt()) != 0 {
		t.Errorf("Expected value to be unchanged, expected %v got %v", tx.Value, parsedTx.Value())
	}
	control.approveCh <- "Y"
	control.approveCh <- "a_long_password"

	res2, err = api.SignTransaction(context.Background(), tx, &methodSig)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Raw, res2.Raw) {
		t.Error("Expected tx to be unmodified by UI")
	}

	//The tx is modified by the UI
	control.approveCh <- "M"
	control.approveCh <- "a_long_password"

	res2, err = api.SignTransaction(context.Background(), tx, &methodSig)
	if err != nil {
		t.Fatal(err)
	}
	parsedTx2 := &types.Transaction{}
	rlp.Decode(bytes.NewReader(res.Raw), parsedTx2)

	//The tx should be modified by the UI
	if parsedTx2.Value().Cmp(tx.Value.ToInt()) != 0 {
		t.Errorf("Expected value to be unchanged, got %v", parsedTx.Value())
	}
	if bytes.Equal(res.Raw, res2.Raw) {
		t.Error("Expected tx to be modified by UI")
	}

}

/*
func TestAsyncronousResponses(t *testing.T){

	//Set up one account
	api, control := setup(t)
	createAccount(control, api, t)

	// Two transactions, the second one with larger value than the first
	tx1 := mkTestTx()
	newVal := big.NewInt(0).Add((*big.Int) (tx1.Value), big.NewInt(1))
	tx2 := mkTestTx()
	tx2.Value = (*hexutil.Big)(newVal)

	control <- "W" //wait
	control <- "Y" //
	control <- "a_long_password"
	control <- "Y" //
	control <- "a_long_password"

	var err error

	h1, err := api.SignTransaction(context.Background(), common.HexToAddress("1111"), tx1, nil)
	h2, err := api.SignTransaction(context.Background(), common.HexToAddress("2222"), tx2, nil)


	}
*/
