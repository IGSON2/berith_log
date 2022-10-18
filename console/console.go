// Copyright 2016 The go-ethereum Authors
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

package console

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"berith-chain/internals/jsre"
	"berith-chain/internals/web3ext"

	"github.com/BerithFoundation/berith-chain/log"
	"github.com/BerithFoundation/berith-chain/rpc"
	"github.com/mattn/go-colorable"
	"github.com/peterh/liner"
	"github.com/robertkrimen/otto"
)

var (
	passwordRegexp = regexp.MustCompile(`personal.[nus]`)
	onlyWhitespace = regexp.MustCompile(`^\s*$`)
	exit           = regexp.MustCompile(`^\s*exit\s*;*\s*$`)
)

// HistoryFile is the file within the data directory to store input scrollback.
const HistoryFile = "history"

// DefaultPrompt is the default prompt line prefix to use for user input querying.
const DefaultPrompt = "> "

// Config is the collection of configurations to fine tune the behavior of the
// JavaScript console.
type Config struct {
	DataDir  string       // Data directory to store the console history at
	DocRoot  string       // Filesystem path from where to load JavaScript files from
	Client   *rpc.Client  // RPC client to execute Ethereum requests through
	Prompt   string       // Input prompt prefix string (defaults to DefaultPrompt)
	Prompter UserPrompter // Input prompter to allow interactive user feedback (defaults to TerminalPrompter)
	Printer  io.Writer    // Output writer to serialize any display strings to (defaults to os.Stdout)
	Preload  []string     // Absolute paths to JavaScript files to preload
}

// Console is a JavaScript interpreted runtime environment. It is a fully fledged
// JavaScript console attached to a running node via an external or in-process RPC
// client.
type Console struct {
	client   *rpc.Client  // RPC client to execute Ethereum requests through
	jsre     *jsre.JSRE   // JavaScript runtime environment running the interpreter
	prompt   string       // Input prompt prefix string
	prompter UserPrompter // Input prompter to allow interactive user feedback
	histPath string       // Absolute path to the console scrollback history
	history  []string     // Scroll history maintained by the console
	printer  io.Writer    // Output writer to serialize any display strings to
}

// New initializes a JavaScript interpreted runtime environment and sets defaults
// with the config struct.
func New(config Config) (*Console, error) {
	// Handle unset config values gracefully
	if config.Prompter == nil {
		config.Prompter = Stdin
	}
	if config.Prompt == "" {
		config.Prompt = DefaultPrompt
	}
	if config.Printer == nil {
		config.Printer = colorable.NewColorableStdout()
	}
	// Initialize the console and return
	console := &Console{
		client:   config.Client,
		jsre:     jsre.New(config.DocRoot, config.Printer),
		prompt:   config.Prompt,
		prompter: config.Prompter,
		printer:  config.Printer,
		histPath: filepath.Join(config.DataDir, HistoryFile),
	}
	if err := os.MkdirAll(config.DataDir, 0700); err != nil {
		return nil, err
	}
	if err := console.init(config.Preload); err != nil {
		return nil, err
	}
	return console, nil
}

// init retrieves the available APIs from the remote RPC provider and initializes
// the console's JavaScript namespaces based on the exposed modules.
func (c *Console) init(preload []string) error {
	fmt.Println("Console.init() 호출")
	// Initialize the JavaScript <-> Go RPC bridge
	bridge := newBridge(c.client, c.prompter, c.printer)
	c.jsre.Set("jeth", struct{}{})

	jethObj, _ := c.jsre.Get("jeth")
	jethObj.Object().Set("send", bridge.Send)
	jethObj.Object().Set("sendAsync", bridge.Send)

	consoleObj, _ := c.jsre.Get("console")
	consoleObj.Object().Set("log", c.consoleOutput)
	consoleObj.Object().Set("error", c.consoleOutput)

	// Load all the internals utility JavaScript libraries
	if err := c.jsre.Compile("bignumber.js", jsre.BigNumber_JS); err != nil {
		return fmt.Errorf("bignumber.js: %v", err)
	}
	if err := c.jsre.Compile("web3.js", jsre.Web3_JS); err != nil {
		return fmt.Errorf("web3.js: %v", err)
	}
	if _, err := c.jsre.Run("var Web3 = require('web3');"); err != nil {
		return fmt.Errorf("web3 require: %v", err)
	}
	if _, err := c.jsre.Run("var web3 = new Web3(jeth);"); err != nil {
		return fmt.Errorf("web3 provider: %v", err)
	}
	// Load the supported APIs into the JavaScript runtime environment
	apis, err := c.client.SupportedModules()
	if err != nil {
		return fmt.Errorf("api modules: %v", err)
	}
	flatten := "var berith = web3.berith; var personal = web3.personal; "
	for api := range apis {
		if api == "web3" {
			continue // manually mapped or ignore
		}
		if file, ok := web3ext.Modules[api]; ok {
			// Load our extension for the module.
			if err = c.jsre.Compile(fmt.Sprintf("%s.js", api), file); err != nil {
				return fmt.Errorf("%s.js: %v", api, err)
			}
			flatten += fmt.Sprintf("var %s = web3.%s; ", api, api)
		} else if obj, err := c.jsre.Run("web3." + api); err == nil && obj.IsObject() {
			// Enable web3.js built-in extension if available.
			flatten += fmt.Sprintf("var %s = web3.%s; ", api, api)
		}
	}
	if _, err = c.jsre.Run(flatten); err != nil {
		return fmt.Errorf("namespace flattening: %v", err)
	}
	// 빠른 테스트 초기 설정을 위한 임시 명령어
	_, err = c.jsre.Run(`
			var u1 = "";
			var u2 = "";
			if(berith.accounts[0] !== ""){u1 = berith.accounts[0];}
			if(berith.accounts[1] !== ""){u2 = berith.accounts[1];}
			var b = berith.getBalance;
			var ua = personal.unlockAccount;
			var t = berith.sendTransaction;
			if(u1 !== ""){ua(u1,"777");}
			if(u2 !== ""){ua(u2,"777");}
			`)

	if err != nil {
		log.Error("임시 변수 초기화 불가능", "err", err)
	}

	// 컨트랙트 실험
	_, err = c.jsre.Run(`var abi = [
		{
			"inputs": [],
			"stateMutability": "nonpayable",
			"type": "constructor"
		},
		{
			"anonymous": false,
			"inputs": [
				{
					"indexed": true,
					"internalType": "address",
					"name": "owner",
					"type": "address"
				},
				{
					"indexed": true,
					"internalType": "address",
					"name": "approved",
					"type": "address"
				},
				{
					"indexed": true,
					"internalType": "uint256",
					"name": "tokenId",
					"type": "uint256"
				}
			],
			"name": "Approval",
			"type": "event"
		},
		{
			"anonymous": false,
			"inputs": [
				{
					"indexed": true,
					"internalType": "address",
					"name": "owner",
					"type": "address"
				},
				{
					"indexed": true,
					"internalType": "address",
					"name": "operator",
					"type": "address"
				},
				{
					"indexed": false,
					"internalType": "bool",
					"name": "approved",
					"type": "bool"
				}
			],
			"name": "ApprovalForAll",
			"type": "event"
		},
		{
			"anonymous": false,
			"inputs": [
				{
					"indexed": true,
					"internalType": "address",
					"name": "from",
					"type": "address"
				},
				{
					"indexed": true,
					"internalType": "address",
					"name": "to",
					"type": "address"
				},
				{
					"indexed": true,
					"internalType": "uint256",
					"name": "tokenId",
					"type": "uint256"
				}
			],
			"name": "Transfer",
			"type": "event"
		},
		{
			"inputs": [
				{
					"internalType": "address",
					"name": "_receiver",
					"type": "address"
				}
			],
			"name": "addUserWhiteList",
			"outputs": [],
			"stateMutability": "nonpayable",
			"type": "function"
		},
		{
			"inputs": [],
			"name": "admin",
			"outputs": [
				{
					"internalType": "address",
					"name": "",
					"type": "address"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "address",
					"name": "_receiver",
					"type": "address"
				},
				{
					"internalType": "uint256",
					"name": "_requestedCount",
					"type": "uint256"
				}
			],
			"name": "airDropMint",
			"outputs": [],
			"stateMutability": "nonpayable",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "address",
					"name": "to",
					"type": "address"
				},
				{
					"internalType": "uint256",
					"name": "tokenId",
					"type": "uint256"
				}
			],
			"name": "approve",
			"outputs": [],
			"stateMutability": "nonpayable",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "address",
					"name": "owner",
					"type": "address"
				}
			],
			"name": "balanceOf",
			"outputs": [
				{
					"internalType": "uint256",
					"name": "",
					"type": "uint256"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "uint256",
					"name": "tokenId",
					"type": "uint256"
				}
			],
			"name": "getApproved",
			"outputs": [
				{
					"internalType": "address",
					"name": "",
					"type": "address"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [],
			"name": "getSaleInfo",
			"outputs": [
				{
					"components": [
						{
							"internalType": "uint256",
							"name": "price",
							"type": "uint256"
						},
						{
							"internalType": "uint256",
							"name": "maxSupply",
							"type": "uint256"
						},
						{
							"internalType": "uint256",
							"name": "maxMintAmount",
							"type": "uint256"
						},
						{
							"internalType": "uint256",
							"name": "maxMintPerSale",
							"type": "uint256"
						}
					],
					"internalType": "struct GalleryNft.SaleInfo",
					"name": "",
					"type": "tuple"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "address",
					"name": "owner",
					"type": "address"
				},
				{
					"internalType": "address",
					"name": "operator",
					"type": "address"
				}
			],
			"name": "isApprovedForAll",
			"outputs": [
				{
					"internalType": "bool",
					"name": "",
					"type": "bool"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "address",
					"name": "_user",
					"type": "address"
				}
			],
			"name": "isUserWhiteList",
			"outputs": [
				{
					"internalType": "bool",
					"name": "",
					"type": "bool"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [],
			"name": "lockNft",
			"outputs": [
				{
					"internalType": "bool",
					"name": "",
					"type": "bool"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [],
			"name": "name",
			"outputs": [
				{
					"internalType": "string",
					"name": "",
					"type": "string"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [],
			"name": "owner",
			"outputs": [
				{
					"internalType": "address",
					"name": "",
					"type": "address"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "uint256",
					"name": "tokenId",
					"type": "uint256"
				}
			],
			"name": "ownerOf",
			"outputs": [
				{
					"internalType": "address",
					"name": "",
					"type": "address"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "uint256",
					"name": "_requestedAmount",
					"type": "uint256"
				}
			],
			"name": "publicMint",
			"outputs": [],
			"stateMutability": "payable",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "address",
					"name": "_receiver",
					"type": "address"
				}
			],
			"name": "removeUserWhitelist",
			"outputs": [],
			"stateMutability": "nonpayable",
			"type": "function"
		},
		{
			"inputs": [],
			"name": "revealed",
			"outputs": [
				{
					"internalType": "bool",
					"name": "",
					"type": "bool"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "address",
					"name": "from",
					"type": "address"
				},
				{
					"internalType": "address",
					"name": "to",
					"type": "address"
				},
				{
					"internalType": "uint256",
					"name": "tokenId",
					"type": "uint256"
				}
			],
			"name": "safeTransferFrom",
			"outputs": [],
			"stateMutability": "nonpayable",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "address",
					"name": "from",
					"type": "address"
				},
				{
					"internalType": "address",
					"name": "to",
					"type": "address"
				},
				{
					"internalType": "uint256",
					"name": "tokenId",
					"type": "uint256"
				},
				{
					"internalType": "bytes",
					"name": "data",
					"type": "bytes"
				}
			],
			"name": "safeTransferFrom",
			"outputs": [],
			"stateMutability": "nonpayable",
			"type": "function"
		},
		{
			"inputs": [],
			"name": "saleInfo",
			"outputs": [
				{
					"internalType": "uint256",
					"name": "price",
					"type": "uint256"
				},
				{
					"internalType": "uint256",
					"name": "maxSupply",
					"type": "uint256"
				},
				{
					"internalType": "uint256",
					"name": "maxMintAmount",
					"type": "uint256"
				},
				{
					"internalType": "uint256",
					"name": "maxMintPerSale",
					"type": "uint256"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "address",
					"name": "_admin",
					"type": "address"
				}
			],
			"name": "setAdmin",
			"outputs": [],
			"stateMutability": "nonpayable",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "address",
					"name": "operator",
					"type": "address"
				},
				{
					"internalType": "bool",
					"name": "approved",
					"type": "bool"
				}
			],
			"name": "setApprovalForAll",
			"outputs": [],
			"stateMutability": "nonpayable",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "bool",
					"name": "_isLock",
					"type": "bool"
				}
			],
			"name": "setLockNft",
			"outputs": [],
			"stateMutability": "nonpayable",
			"type": "function"
		},
		{
			"inputs": [
				{
					"components": [
						{
							"internalType": "uint256",
							"name": "price",
							"type": "uint256"
						},
						{
							"internalType": "uint256",
							"name": "maxSupply",
							"type": "uint256"
						},
						{
							"internalType": "uint256",
							"name": "maxMintAmount",
							"type": "uint256"
						},
						{
							"internalType": "uint256",
							"name": "maxMintPerSale",
							"type": "uint256"
						}
					],
					"internalType": "struct GalleryNft.SaleInfo",
					"name": "_saleInfo",
					"type": "tuple"
				}
			],
			"name": "setSaleInfo",
			"outputs": [],
			"stateMutability": "nonpayable",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "bool",
					"name": "_isMinting",
					"type": "bool"
				}
			],
			"name": "setSubmitMinting",
			"outputs": [],
			"stateMutability": "nonpayable",
			"type": "function"
		},
		{
			"inputs": [],
			"name": "submitMinting",
			"outputs": [
				{
					"internalType": "bool",
					"name": "",
					"type": "bool"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "bytes4",
					"name": "interfaceId",
					"type": "bytes4"
				}
			],
			"name": "supportsInterface",
			"outputs": [
				{
					"internalType": "bool",
					"name": "",
					"type": "bool"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [],
			"name": "symbol",
			"outputs": [
				{
					"internalType": "string",
					"name": "",
					"type": "string"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "uint256",
					"name": "index",
					"type": "uint256"
				}
			],
			"name": "tokenByIndex",
			"outputs": [
				{
					"internalType": "uint256",
					"name": "",
					"type": "uint256"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "address",
					"name": "owner",
					"type": "address"
				},
				{
					"internalType": "uint256",
					"name": "index",
					"type": "uint256"
				}
			],
			"name": "tokenOfOwnerByIndex",
			"outputs": [
				{
					"internalType": "uint256",
					"name": "",
					"type": "uint256"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "uint256",
					"name": "_tokenId",
					"type": "uint256"
				}
			],
			"name": "tokenURI",
			"outputs": [
				{
					"internalType": "string",
					"name": "",
					"type": "string"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [],
			"name": "totalSupply",
			"outputs": [
				{
					"internalType": "uint256",
					"name": "",
					"type": "uint256"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "address",
					"name": "from",
					"type": "address"
				},
				{
					"internalType": "address",
					"name": "to",
					"type": "address"
				},
				{
					"internalType": "uint256",
					"name": "tokenId",
					"type": "uint256"
				}
			],
			"name": "transferFrom",
			"outputs": [],
			"stateMutability": "nonpayable",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "address",
					"name": "_owner",
					"type": "address"
				}
			],
			"name": "walletOfOwner",
			"outputs": [
				{
					"internalType": "uint256[]",
					"name": "",
					"type": "uint256[]"
				}
			],
			"stateMutability": "view",
			"type": "function"
		},
		{
			"inputs": [
				{
					"internalType": "uint256",
					"name": "_requestedAmount",
					"type": "uint256"
				}
			],
			"name": "whiteListSale",
			"outputs": [],
			"stateMutability": "payable",
			"type": "function"
		}
	];
	var object = "0x6080604052604051806080016040528067016345785d8a00008152602001620186a081526020016103e881526020016064815250600b6000820151816000015560208201518160010155604082015181600201556060820151816003015550506000601060146101000a81548160ff0219169083151502179055506000601060156101000a81548160ff0219169083151502179055506000601060166101000a81548160ff0219169083151502179055506040518060400160405280600781526020017f6261736555524c0000000000000000000000000000000000000000000000000081525060119080519060200190620000fd92919062000282565b506040518060400160405280600b81526020017f756e52657665616c55524c000000000000000000000000000000000000000000815250601290805190602001906200014b92919062000282565b503480156200015957600080fd5b506040518060400160405280600681526020017f42657269746800000000000000000000000000000000000000000000000000008152506040518060400160405280600381526020017f42525400000000000000000000000000000000000000000000000000000000008152508160009080519060200190620001de92919062000282565b508060019080519060200190620001f792919062000282565b50505033601060006101000a81548173ffffffffffffffffffffffffffffffffffffffff021916908373ffffffffffffffffffffffffffffffffffffffff16021790555033600f60006101000a81548173ffffffffffffffffffffffffffffffffffffffff021916908373ffffffffffffffffffffffffffffffffffffffff16021790555062000397565b828054620002909062000332565b90600052602060002090601f016020900481019282620002b4576000855562000300565b82601f10620002cf57805160ff191683800117855562000300565b8280016001018555821562000300579182015b82811115620002ff578251825591602001919060010190620002e2565b5b5090506200030f919062000313565b5090565b5b808211156200032e57600081600090555060010162000314565b5090565b600060028204905060018216806200034b57607f821691505b6020821081141562000362576200036162000368565b5b50919050565b7f4e487b7100000000000000000000000000000000000000000000000000000000600052602260045260246000fd5b61496b80620003a76000396000f3fe6080604052600436106101f95760003560e01c8063518302271161010d5780639c5bfeae116100a0578063c87b56dd1161006f578063c87b56dd1461073c578063db83694c14610779578063e985e9c5146107a4578063f851a440146107e1578063ff34939d1461080c576101f9565b80639c5bfeae146106a3578063a22cb465146106ce578063b85eb674146106f7578063b88d4fde14610713576101f9565b806375a1ed08116100dc57806375a1ed08146105f65780638da5cb5b1461061f5780638e3695b81461064a57806395d89b4114610678576101f9565b806351830227146105285780636352211e14610553578063704b6c021461059057806370a08231146105b9576101f9565b806323b872dd116101905780632f745c591161015f5780632f745c591461041f57806342842e0e1461045c578063438b6300146104855780634e42324c146104c25780634f6ccce7146104eb576101f9565b806323b872dd14610388578063252d30cc146103b15780632a8a9938146103da5780632db1154414610403576101f9565b8063095ea7b3116101cc578063095ea7b3146102e05780630ba03b4914610309578063171c5d9c1461033457806318160ddd1461035d576101f9565b806301ffc9a7146101fe5780630623e43b1461023b57806306fdde0314610278578063081812fc146102a3575b600080fd5b34801561020a57600080fd5b50610225600480360381019061022091906133c0565b610835565b6040516102329190613ab0565b60405180910390f35b34801561024757600080fd5b50610262600480360381019061025d91906131f0565b6108af565b60405161026f9190613ab0565b60405180910390f35b34801561028457600080fd5b5061028d610905565b60405161029a9190613acb565b60405180910390f35b3480156102af57600080fd5b506102ca60048036038101906102c5919061343b565b610997565b6040516102d79190613a27565b60405180910390f35b3480156102ec57600080fd5b506103076004803603810190610302919061335b565b6109dd565b005b34801561031557600080fd5b5061031e610af5565b60405161032b9190613ab0565b60405180910390f35b34801561034057600080fd5b5061035b600480360381019061035691906131f0565b610b08565b005b34801561036957600080fd5b50610372610c42565b60405161037f9190613e08565b60405180910390f35b34801561039457600080fd5b506103af60048036038101906103aa9190613255565b610c4f565b005b3480156103bd57600080fd5b506103d860048036038101906103d39190613412565b610ccf565b005b3480156103e657600080fd5b5061040160048036038101906103fc91906131f0565b610de8565b005b61041d6004803603810190610418919061343b565b610f2b565b005b34801561042b57600080fd5b506104466004803603810190610441919061335b565b61112d565b6040516104539190613e08565b60405180910390f35b34801561046857600080fd5b50610483600480360381019061047e9190613255565b6111d2565b005b34801561049157600080fd5b506104ac60048036038101906104a791906131f0565b6111f2565b6040516104b99190613a8e565b60405180910390f35b3480156104ce57600080fd5b506104e960048036038101906104e49190613397565b6112ec565b005b3480156104f757600080fd5b50610512600480360381019061050d919061343b565b611399565b60405161051f9190613e08565b60405180910390f35b34801561053457600080fd5b5061053d611430565b60405161054a9190613ab0565b60405180910390f35b34801561055f57600080fd5b5061057a6004803603810190610575919061343b565b611443565b6040516105879190613a27565b60405180910390f35b34801561059c57600080fd5b506105b760048036038101906105b291906131f0565b6114f5565b005b3480156105c557600080fd5b506105e060048036038101906105db91906131f0565b6115c9565b6040516105ed9190613e08565b60405180910390f35b34801561060257600080fd5b5061061d6004803603810190610618919061335b565b611681565b005b34801561062b57600080fd5b5061063461185f565b6040516106419190613a27565b60405180910390f35b34801561065657600080fd5b5061065f611885565b60405161066f9493929190613e23565b60405180910390f35b34801561068457600080fd5b5061068d6118a3565b60405161069a9190613acb565b60405180910390f35b3480156106af57600080fd5b506106b8611935565b6040516106c59190613ab0565b60405180910390f35b3480156106da57600080fd5b506106f560048036038101906106f0919061331f565b611948565b005b610711600480360381019061070c919061343b565b61195e565b005b34801561071f57600080fd5b5061073a600480360381019061073591906132a4565b611ba8565b005b34801561074857600080fd5b50610763600480360381019061075e919061343b565b611c0a565b6040516107709190613acb565b60405180910390f35b34801561078557600080fd5b5061078e611cdd565b60405161079b9190613ded565b60405180910390f35b3480156107b057600080fd5b506107cb60048036038101906107c69190613219565b611d1f565b6040516107d89190613ab0565b60405180910390f35b3480156107ed57600080fd5b506107f6611db3565b6040516108039190613a27565b60405180910390f35b34801561081857600080fd5b50610833600480360381019061082e9190613397565b611dd9565b005b60007f780e9d63000000000000000000000000000000000000000000000000000000007bffffffffffffffffffffffffffffffffffffffffffffffffffffffff1916827bffffffffffffffffffffffffffffffffffffffffffffffffffffffff191614806108a857506108a782611e86565b5b9050919050565b6000601360008373ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16815260200190815260200160002060009054906101000a900460ff169050919050565b6060600080546109149061411a565b80601f01602080910402602001604051908101604052809291908181526020018280546109409061411a565b801561098d5780601f106109625761010080835404028352916020019161098d565b820191906000526020600020905b81548152906001019060200180831161097057829003601f168201915b5050505050905090565b60006109a282611f68565b6004600083815260200190815260200160002060009054906101000a900473ffffffffffffffffffffffffffffffffffffffff169050919050565b60006109e882611443565b90508073ffffffffffffffffffffffffffffffffffffffff168373ffffffffffffffffffffffffffffffffffffffff161415610a59576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401610a5090613ccd565b60405180910390fd5b8073ffffffffffffffffffffffffffffffffffffffff16610a78611fb3565b73ffffffffffffffffffffffffffffffffffffffff161480610aa75750610aa681610aa1611fb3565b611d1f565b5b610ae6576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401610add90613c6d565b60405180910390fd5b610af08383611fbb565b505050565b601060149054906101000a900460ff1681565b3373ffffffffffffffffffffffffffffffffffffffff16601060009054906101000a900473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff161480610bb157503373ffffffffffffffffffffffffffffffffffffffff16600f60009054906101000a900473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16145b610bf0576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401610be790613bad565b60405180910390fd5b601360008273ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16815260200190815260200160002060006101000a81549060ff021916905550565b6000600880549050905090565b60001515601060159054906101000a900460ff16151514610c6f57600080fd5b610c80610c7a611fb3565b82612074565b610cbf576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401610cb690613ced565b60405180910390fd5b610cca838383612109565b505050565b3373ffffffffffffffffffffffffffffffffffffffff16601060009054906101000a900473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff161480610d7857503373ffffffffffffffffffffffffffffffffffffffff16600f60009054906101000a900473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16145b610db7576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401610dae90613bad565b60405180910390fd5b80600b6000820151816000015560208201518160010155604082015181600201556060820151816003015590505050565b3373ffffffffffffffffffffffffffffffffffffffff16601060009054906101000a900473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff161480610e9157503373ffffffffffffffffffffffffffffffffffffffff16600f60009054906101000a900473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16145b610ed0576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401610ec790613bad565b60405180910390fd5b6001601360008373ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16815260200190815260200160002060006101000a81548160ff02191690831515021790555050565b6000610f37600a612370565b9050601060149054906101000a900460ff16610f88576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401610f7f90613dcd565b60405180910390fd5b6001600b60010154610f9a9190613f4f565b8282610fa69190613f4f565b1115610fe7576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401610fde90613d2d565b60405180910390fd5b600082118015610ffc5750600b600201548211155b61103b576040517f08c379a000000000000000000000000000000000000000000000000000000000815260040161103290613b8d565b60405180910390fd5b81600b6000015461104c9190613fd6565b341461108d576040517f08c379a000000000000000000000000000000000000000000000000000000000815260040161108490613c0d565b60405180910390fd5b600b600301548261109d336115c9565b6110a79190613f4f565b11156110e8576040517f08c379a00000000000000000000000000000000000000000000000000000000081526004016110df90613b4d565b60405180910390fd5b6000600190505b8281116111285761110b3382846111069190613f4f565b61237e565b611115600a61239c565b80806111209061417d565b9150506110ef565b505050565b6000611138836115c9565b8210611179576040517f08c379a000000000000000000000000000000000000000000000000000000000815260040161117090613aed565b60405180910390fd5b600660008473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff168152602001908152602001600020600083815260200190815260200160002054905092915050565b6111ed83838360405180602001604052806000815250611ba8565b505050565b606060006111ff836115c9565b905060008167ffffffffffffffff811115611243577f4e487b7100000000000000000000000000000000000000000000000000000000600052604160045260246000fd5b6040519080825280602002602001820160405280156112715781602001602082028036833780820191505090505b50905060005b828110156112e157611289858261112d565b8282815181106112c2577f4e487b7100000000000000000000000000000000000000000000000000000000600052603260045260246000fd5b60200260200101818152505080806112d99061417d565b915050611277565b508092505050919050565b3373ffffffffffffffffffffffffffffffffffffffff16600f60009054906101000a900473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff161461137c576040517f08c379a000000000000000000000000000000000000000000000000000000000815260040161137390613dad565b60405180910390fd5b80601060156101000a81548160ff02191690831515021790555050565b60006113a3610c42565b82106113e4576040517f08c379a00000000000000000000000000000000000000000000000000000000081526004016113db90613d4d565b60405180910390fd5b6008828154811061141e577f4e487b7100000000000000000000000000000000000000000000000000000000600052603260045260246000fd5b90600052602060002001549050919050565b601060169054906101000a900460ff1681565b6000806002600084815260200190815260200160002060009054906101000a900473ffffffffffffffffffffffffffffffffffffffff169050600073ffffffffffffffffffffffffffffffffffffffff168173ffffffffffffffffffffffffffffffffffffffff1614156114ec576040517f08c379a00000000000000000000000000000000000000000000000000000000081526004016114e390613cad565b60405180910390fd5b80915050919050565b3373ffffffffffffffffffffffffffffffffffffffff16600f60009054906101000a900473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff1614611585576040517f08c379a000000000000000000000000000000000000000000000000000000000815260040161157c90613dad565b60405180910390fd5b80601060006101000a81548173ffffffffffffffffffffffffffffffffffffffff021916908373ffffffffffffffffffffffffffffffffffffffff16021790555050565b60008073ffffffffffffffffffffffffffffffffffffffff168273ffffffffffffffffffffffffffffffffffffffff16141561163a576040517f08c379a000000000000000000000000000000000000000000000000000000000815260040161163190613c2d565b60405180910390fd5b600360008373ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff168152602001908152602001600020549050919050565b3373ffffffffffffffffffffffffffffffffffffffff16601060009054906101000a900473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16148061172a57503373ffffffffffffffffffffffffffffffffffffffff16600f60009054906101000a900473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16145b611769576040517f08c379a000000000000000000000000000000000000000000000000000000000815260040161176090613bad565b60405180910390fd5b600081116117ac576040517f08c379a00000000000000000000000000000000000000000000000000000000081526004016117a390613c4d565b60405180910390fd5b60006117b8600a612370565b90506001600b600101546117cc9190613f4f565b82826117d89190613f4f565b1115611819576040517f08c379a000000000000000000000000000000000000000000000000000000000815260040161181090613d2d565b60405180910390fd5b6000600190505b8281116118595761183c8482846118379190613f4f565b6123b2565b611846600a61239c565b80806118519061417d565b915050611820565b50505050565b600f60009054906101000a900473ffffffffffffffffffffffffffffffffffffffff1681565b600b8060000154908060010154908060020154908060030154905084565b6060600180546118b29061411a565b80601f01602080910402602001604051908101604052809291908181526020018280546118de9061411a565b801561192b5780601f106119005761010080835404028352916020019161192b565b820191906000526020600020905b81548152906001019060200180831161190e57829003601f168201915b5050505050905090565b601060159054906101000a900460ff1681565b61195a611953611fb3565b838361258c565b5050565b600061196a600a612370565b9050611975336108af565b6119b4576040517f08c379a00000000000000000000000000000000000000000000000000000000081526004016119ab90613d6d565b60405180910390fd5b601060149054906101000a900460ff16611a03576040517f08c379a00000000000000000000000000000000000000000000000000000000081526004016119fa90613d0d565b60405180910390fd5b6001600b60010154611a159190613f4f565b8282611a219190613f4f565b1115611a62576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401611a5990613d2d565b60405180910390fd5b600082118015611a775750600b600201548211155b611ab6576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401611aad90613b8d565b60405180910390fd5b81600b60000154611ac79190613fd6565b3414611b08576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401611aff90613c0d565b60405180910390fd5b600b6003015482611b18336115c9565b611b229190613f4f565b1115611b63576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401611b5a90613b4d565b60405180910390fd5b6000600190505b828111611ba357611b86338284611b819190613f4f565b61237e565b611b90600a61239c565b8080611b9b9061417d565b915050611b6a565b505050565b611bb9611bb3611fb3565b83612074565b611bf8576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401611bef90613d8d565b60405180910390fd5b611c04848484846126f9565b50505050565b6060601060169054906101000a900460ff16611c7e57600060128054611c2f9061411a565b905011611c4b5760405180602001604052806000815250611c77565b6012611c5683612755565b604051602001611c679291906139f8565b6040516020818303038152906040525b9050611cd8565b600060118054611c8d9061411a565b905011611ca95760405180602001604052806000815250611cd5565b6011611cb483612755565b604051602001611cc59291906139f8565b6040516020818303038152906040525b90505b919050565b611ce5613083565b600b604051806080016040529081600082015481526020016001820154815260200160028201548152602001600382015481525050905090565b6000600560008473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16815260200190815260200160002060008373ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16815260200190815260200160002060009054906101000a900460ff16905092915050565b601060009054906101000a900473ffffffffffffffffffffffffffffffffffffffff1681565b3373ffffffffffffffffffffffffffffffffffffffff16600f60009054906101000a900473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff1614611e69576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401611e6090613dad565b60405180910390fd5b80601060146101000a81548160ff02191690831515021790555050565b60007f80ac58cd000000000000000000000000000000000000000000000000000000007bffffffffffffffffffffffffffffffffffffffffffffffffffffffff1916827bffffffffffffffffffffffffffffffffffffffffffffffffffffffff19161480611f5157507f5b5e139f000000000000000000000000000000000000000000000000000000007bffffffffffffffffffffffffffffffffffffffffffffffffffffffff1916827bffffffffffffffffffffffffffffffffffffffffffffffffffffffff1916145b80611f615750611f6082612902565b5b9050919050565b611f718161296c565b611fb0576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401611fa790613cad565b60405180910390fd5b50565b600033905090565b816004600083815260200190815260200160002060006101000a81548173ffffffffffffffffffffffffffffffffffffffff021916908373ffffffffffffffffffffffffffffffffffffffff160217905550808273ffffffffffffffffffffffffffffffffffffffff1661202e83611443565b73ffffffffffffffffffffffffffffffffffffffff167f8c5be1e5ebec7d5bd14f71427d1e84f3dd0314c0f7b2291e5b200ac8c7c3b92560405160405180910390a45050565b60008061208083611443565b90508073ffffffffffffffffffffffffffffffffffffffff168473ffffffffffffffffffffffffffffffffffffffff1614806120c257506120c18185611d1f565b5b8061210057508373ffffffffffffffffffffffffffffffffffffffff166120e884610997565b73ffffffffffffffffffffffffffffffffffffffff16145b91505092915050565b8273ffffffffffffffffffffffffffffffffffffffff1661212982611443565b73ffffffffffffffffffffffffffffffffffffffff161461217f576040517f08c379a000000000000000000000000000000000000000000000000000000000815260040161217690613b2d565b60405180910390fd5b600073ffffffffffffffffffffffffffffffffffffffff168273ffffffffffffffffffffffffffffffffffffffff1614156121ef576040517f08c379a00000000000000000000000000000000000000000000000000000000081526004016121e690613bcd565b60405180910390fd5b6121fa8383836129d8565b612205600082611fbb565b6001600360008573ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16815260200190815260200160002060008282546122559190614030565b925050819055506001600360008473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16815260200190815260200160002060008282546122ac9190613f4f565b92505081905550816002600083815260200190815260200160002060006101000a81548173ffffffffffffffffffffffffffffffffffffffff021916908373ffffffffffffffffffffffffffffffffffffffff160217905550808273ffffffffffffffffffffffffffffffffffffffff168473ffffffffffffffffffffffffffffffffffffffff167fddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef60405160405180910390a461236b838383612aec565b505050565b600081600001549050919050565b612398828260405180602001604052806000815250612af1565b5050565b6001816000016000828254019250508190555050565b600073ffffffffffffffffffffffffffffffffffffffff168273ffffffffffffffffffffffffffffffffffffffff161415612422576040517f08c379a000000000000000000000000000000000000000000000000000000000815260040161241990613c8d565b60405180910390fd5b61242b8161296c565b1561246b576040517f08c379a000000000000000000000000000000000000000000000000000000000815260040161246290613b6d565b60405180910390fd5b612477600083836129d8565b6001600360008473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16815260200190815260200160002060008282546124c79190613f4f565b92505081905550816002600083815260200190815260200160002060006101000a81548173ffffffffffffffffffffffffffffffffffffffff021916908373ffffffffffffffffffffffffffffffffffffffff160217905550808273ffffffffffffffffffffffffffffffffffffffff16600073ffffffffffffffffffffffffffffffffffffffff167fddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef60405160405180910390a461258860008383612aec565b5050565b8173ffffffffffffffffffffffffffffffffffffffff168373ffffffffffffffffffffffffffffffffffffffff1614156125fb576040517f08c379a00000000000000000000000000000000000000000000000000000000081526004016125f290613bed565b60405180910390fd5b80600560008573ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16815260200190815260200160002060008473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16815260200190815260200160002060006101000a81548160ff0219169083151502179055508173ffffffffffffffffffffffffffffffffffffffff168373ffffffffffffffffffffffffffffffffffffffff167f17307eab39ab6107e8899845ad3d59bd9653f200f220920489ca2b5937696c31836040516126ec9190613ab0565b60405180910390a3505050565b612704848484612109565b61271084848484612b4c565b61274f576040517f08c379a000000000000000000000000000000000000000000000000000000000815260040161274690613b0d565b60405180910390fd5b50505050565b6060600082141561279d576040518060400160405280600181526020017f300000000000000000000000000000000000000000000000000000000000000081525090506128fd565b600082905060005b600082146127cf5780806127b89061417d565b915050600a826127c89190613fa5565b91506127a5565b60008167ffffffffffffffff811115612811577f4e487b7100000000000000000000000000000000000000000000000000000000600052604160045260246000fd5b6040519080825280601f01601f1916602001820160405280156128435781602001600182028036833780820191505090505b5090505b600085146128f65760018261285c9190614030565b9150600a8561286b91906141c6565b60306128779190613f4f565b60f81b8183815181106128b3577f4e487b7100000000000000000000000000000000000000000000000000000000600052603260045260246000fd5b60200101907effffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff1916908160001a905350600a856128ef9190613fa5565b9450612847565b8093505050505b919050565b60007f01ffc9a7000000000000000000000000000000000000000000000000000000007bffffffffffffffffffffffffffffffffffffffffffffffffffffffff1916827bffffffffffffffffffffffffffffffffffffffffffffffffffffffff1916149050919050565b60008073ffffffffffffffffffffffffffffffffffffffff166002600084815260200190815260200160002060009054906101000a900473ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff1614159050919050565b6129e3838383612ce3565b600073ffffffffffffffffffffffffffffffffffffffff168373ffffffffffffffffffffffffffffffffffffffff161415612a2657612a2181612ce8565b612a65565b8173ffffffffffffffffffffffffffffffffffffffff168373ffffffffffffffffffffffffffffffffffffffff1614612a6457612a638382612d31565b5b5b600073ffffffffffffffffffffffffffffffffffffffff168273ffffffffffffffffffffffffffffffffffffffff161415612aa857612aa381612e9e565b612ae7565b8273ffffffffffffffffffffffffffffffffffffffff168273ffffffffffffffffffffffffffffffffffffffff1614612ae657612ae58282612fe1565b5b5b505050565b505050565b612afb83836123b2565b612b086000848484612b4c565b612b47576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401612b3e90613b0d565b60405180910390fd5b505050565b6000612b6d8473ffffffffffffffffffffffffffffffffffffffff16613060565b15612cd6578373ffffffffffffffffffffffffffffffffffffffff1663150b7a02612b96611fb3565b8786866040518563ffffffff1660e01b8152600401612bb89493929190613a42565b602060405180830381600087803b158015612bd257600080fd5b505af1925050508015612c0357506040513d601f19601f82011682018060405250810190612c0091906133e9565b60015b612c86573d8060008114612c33576040519150601f19603f3d011682016040523d82523d6000602084013e612c38565b606091505b50600081511415612c7e576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401612c7590613b0d565b60405180910390fd5b805181602001fd5b63150b7a0260e01b7bffffffffffffffffffffffffffffffffffffffffffffffffffffffff1916817bffffffffffffffffffffffffffffffffffffffffffffffffffffffff191614915050612cdb565b600190505b949350505050565b505050565b6008805490506009600083815260200190815260200160002081905550600881908060018154018082558091505060019003906000526020600020016000909190919091505550565b60006001612d3e846115c9565b612d489190614030565b9050600060076000848152602001908152602001600020549050818114612e2d576000600660008673ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff168152602001908152602001600020600084815260200190815260200160002054905080600660008773ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff168152602001908152602001600020600084815260200190815260200160002081905550816007600083815260200190815260200160002081905550505b6007600084815260200190815260200160002060009055600660008573ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16815260200190815260200160002060008381526020019081526020016000206000905550505050565b60006001600880549050612eb29190614030565b9050600060096000848152602001908152602001600020549050600060088381548110612f08577f4e487b7100000000000000000000000000000000000000000000000000000000600052603260045260246000fd5b906000526020600020015490508060088381548110612f50577f4e487b7100000000000000000000000000000000000000000000000000000000600052603260045260246000fd5b906000526020600020018190555081600960008381526020019081526020016000208190555060096000858152602001908152602001600020600090556008805480612fc5577f4e487b7100000000000000000000000000000000000000000000000000000000600052603160045260246000fd5b6001900381819060005260206000200160009055905550505050565b6000612fec836115c9565b905081600660008573ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff168152602001908152602001600020600083815260200190815260200160002081905550806007600084815260200190815260200160002081905550505050565b6000808273ffffffffffffffffffffffffffffffffffffffff163b119050919050565b6040518060800160405280600081526020016000815260200160008152602001600081525090565b60006130be6130b984613e8d565b613e68565b9050828152602081018484840111156130d657600080fd5b6130e18482856140d8565b509392505050565b6000813590506130f8816148d9565b92915050565b60008135905061310d816148f0565b92915050565b60008135905061312281614907565b92915050565b60008151905061313781614907565b92915050565b600082601f83011261314e57600080fd5b813561315e8482602086016130ab565b91505092915050565b60006080828403121561317957600080fd5b6131836080613e68565b90506000613193848285016131db565b60008301525060206131a7848285016131db565b60208301525060406131bb848285016131db565b60408301525060606131cf848285016131db565b60608301525092915050565b6000813590506131ea8161491e565b92915050565b60006020828403121561320257600080fd5b6000613210848285016130e9565b91505092915050565b6000806040838503121561322c57600080fd5b600061323a858286016130e9565b925050602061324b858286016130e9565b9150509250929050565b60008060006060848603121561326a57600080fd5b6000613278868287016130e9565b9350506020613289868287016130e9565b925050604061329a868287016131db565b9150509250925092565b600080600080608085870312156132ba57600080fd5b60006132c8878288016130e9565b94505060206132d9878288016130e9565b93505060406132ea878288016131db565b925050606085013567ffffffffffffffff81111561330757600080fd5b6133138782880161313d565b91505092959194509250565b6000806040838503121561333257600080fd5b6000613340858286016130e9565b9250506020613351858286016130fe565b9150509250929050565b6000806040838503121561336e57600080fd5b600061337c858286016130e9565b925050602061338d858286016131db565b9150509250929050565b6000602082840312156133a957600080fd5b60006133b7848285016130fe565b91505092915050565b6000602082840312156133d257600080fd5b60006133e084828501613113565b91505092915050565b6000602082840312156133fb57600080fd5b600061340984828501613128565b91505092915050565b60006080828403121561342457600080fd5b600061343284828501613167565b91505092915050565b60006020828403121561344d57600080fd5b600061345b848285016131db565b91505092915050565b600061347083836139da565b60208301905092915050565b61348581614064565b82525050565b600061349682613ee3565b6134a08185613f11565b93506134ab83613ebe565b8060005b838110156134dc5781516134c38882613464565b97506134ce83613f04565b9250506001810190506134af565b5085935050505092915050565b6134f281614076565b82525050565b600061350382613eee565b61350d8185613f22565b935061351d8185602086016140e7565b613526816142b3565b840191505092915050565b600061353c82613ef9565b6135468185613f33565b93506135568185602086016140e7565b61355f816142b3565b840191505092915050565b600061357582613ef9565b61357f8185613f44565b935061358f8185602086016140e7565b80840191505092915050565b600081546135a88161411a565b6135b28186613f44565b945060018216600081146135cd57600181146135de57613611565b60ff19831686528186019350613611565b6135e785613ece565b60005b83811015613609578154818901526001820191506020810190506135ea565b838801955050505b50505092915050565b6000613627602b83613f33565b9150613632826142c4565b604082019050919050565b600061364a603283613f33565b915061365582614313565b604082019050919050565b600061366d602583613f33565b915061367882614362565b604082019050919050565b6000613690601c83613f33565b915061369b826143b1565b602082019050919050565b60006136b3601c83613f33565b91506136be826143da565b602082019050919050565b60006136d6602183613f33565b91506136e182614403565b604082019050919050565b60006136f9602183613f33565b915061370482614452565b604082019050919050565b600061371c602483613f33565b9150613727826144a1565b604082019050919050565b600061373f601983613f33565b915061374a826144f0565b602082019050919050565b6000613762600e83613f33565b915061376d82614519565b602082019050919050565b6000613785602983613f33565b915061379082614542565b604082019050919050565b60006137a8600c83613f33565b91506137b382614591565b602082019050919050565b60006137cb603e83613f33565b91506137d6826145ba565b604082019050919050565b60006137ee602083613f33565b91506137f982614609565b602082019050919050565b6000613811600583613f44565b915061381c82614632565b600582019050919050565b6000613834601883613f33565b915061383f8261465b565b602082019050919050565b6000613857602183613f33565b915061386282614684565b604082019050919050565b600061387a603183613f33565b9150613885826146d3565b604082019050919050565b600061389d602283613f33565b91506138a882614722565b604082019050919050565b60006138c0601183613f33565b91506138cb82614771565b602082019050919050565b60006138e3602c83613f33565b91506138ee8261479a565b604082019050919050565b6000613906602283613f33565b9150613911826147e9565b604082019050919050565b6000613929602e83613f33565b915061393482614838565b604082019050919050565b600061394c601883613f33565b915061395782614887565b602082019050919050565b600061396f601f83613f33565b915061397a826148b0565b602082019050919050565b60808201600082015161399b60008501826139da565b5060208201516139ae60208501826139da565b5060408201516139c160408501826139da565b5060608201516139d460608501826139da565b50505050565b6139e3816140ce565b82525050565b6139f2816140ce565b82525050565b6000613a04828561359b565b9150613a10828461356a565b9150613a1b82613804565b91508190509392505050565b6000602082019050613a3c600083018461347c565b92915050565b6000608082019050613a57600083018761347c565b613a64602083018661347c565b613a7160408301856139e9565b8181036060830152613a8381846134f8565b905095945050505050565b60006020820190508181036000830152613aa8818461348b565b905092915050565b6000602082019050613ac560008301846134e9565b92915050565b60006020820190508181036000830152613ae58184613531565b905092915050565b60006020820190508181036000830152613b068161361a565b9050919050565b60006020820190508181036000830152613b268161363d565b9050919050565b60006020820190508181036000830152613b4681613660565b9050919050565b60006020820190508181036000830152613b6681613683565b9050919050565b60006020820190508181036000830152613b86816136a6565b9050919050565b60006020820190508181036000830152613ba6816136c9565b9050919050565b60006020820190508181036000830152613bc6816136ec565b9050919050565b60006020820190508181036000830152613be68161370f565b9050919050565b60006020820190508181036000830152613c0681613732565b9050919050565b60006020820190508181036000830152613c2681613755565b9050919050565b60006020820190508181036000830152613c4681613778565b9050919050565b60006020820190508181036000830152613c668161379b565b9050919050565b60006020820190508181036000830152613c86816137be565b9050919050565b60006020820190508181036000830152613ca6816137e1565b9050919050565b60006020820190508181036000830152613cc681613827565b9050919050565b60006020820190508181036000830152613ce68161384a565b9050919050565b60006020820190508181036000830152613d068161386d565b9050919050565b60006020820190508181036000830152613d2681613890565b9050919050565b60006020820190508181036000830152613d46816138b3565b9050919050565b60006020820190508181036000830152613d66816138d6565b9050919050565b60006020820190508181036000830152613d86816138f9565b9050919050565b60006020820190508181036000830152613da68161391c565b9050919050565b60006020820190508181036000830152613dc68161393f565b9050919050565b60006020820190508181036000830152613de681613962565b9050919050565b6000608082019050613e026000830184613985565b92915050565b6000602082019050613e1d60008301846139e9565b92915050565b6000608082019050613e3860008301876139e9565b613e4560208301866139e9565b613e5260408301856139e9565b613e5f60608301846139e9565b95945050505050565b6000613e72613e83565b9050613e7e828261414c565b919050565b6000604051905090565b600067ffffffffffffffff821115613ea857613ea7614284565b5b613eb1826142b3565b9050602081019050919050565b6000819050602082019050919050565b60008190508160005260206000209050919050565b600081519050919050565b600081519050919050565b600081519050919050565b6000602082019050919050565b600082825260208201905092915050565b600082825260208201905092915050565b600082825260208201905092915050565b600081905092915050565b6000613f5a826140ce565b9150613f65836140ce565b9250827fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff03821115613f9a57613f996141f7565b5b828201905092915050565b6000613fb0826140ce565b9150613fbb836140ce565b925082613fcb57613fca614226565b5b828204905092915050565b6000613fe1826140ce565b9150613fec836140ce565b9250817fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff0483118215151615614025576140246141f7565b5b828202905092915050565b600061403b826140ce565b9150614046836140ce565b925082821015614059576140586141f7565b5b828203905092915050565b600061406f826140ae565b9050919050565b60008115159050919050565b60007fffffffff0000000000000000000000000000000000000000000000000000000082169050919050565b600073ffffffffffffffffffffffffffffffffffffffff82169050919050565b6000819050919050565b82818337600083830152505050565b60005b838110156141055780820151818401526020810190506140ea565b83811115614114576000848401525b50505050565b6000600282049050600182168061413257607f821691505b6020821081141561414657614145614255565b5b50919050565b614155826142b3565b810181811067ffffffffffffffff8211171561417457614173614284565b5b80604052505050565b6000614188826140ce565b91507fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff8214156141bb576141ba6141f7565b5b600182019050919050565b60006141d1826140ce565b91506141dc836140ce565b9250826141ec576141eb614226565b5b828206905092915050565b7f4e487b7100000000000000000000000000000000000000000000000000000000600052601160045260246000fd5b7f4e487b7100000000000000000000000000000000000000000000000000000000600052601260045260246000fd5b7f4e487b7100000000000000000000000000000000000000000000000000000000600052602260045260246000fd5b7f4e487b7100000000000000000000000000000000000000000000000000000000600052604160045260246000fd5b6000601f19601f8301169050919050565b7f455243373231456e756d657261626c653a206f776e657220696e646578206f7560008201527f74206f6620626f756e6473000000000000000000000000000000000000000000602082015250565b7f4552433732313a207472616e7366657220746f206e6f6e20455243373231526560008201527f63656976657220696d706c656d656e7465720000000000000000000000000000602082015250565b7f4552433732313a207472616e736665722066726f6d20696e636f72726563742060008201527f6f776e6572000000000000000000000000000000000000000000000000000000602082015250565b7f457863656564206d617820616d6f756e742070657220706572736f6e00000000600082015250565b7f4552433732313a20746f6b656e20616c7265616479206d696e74656400000000600082015250565b7f546f6f206d616e79207265717565737473206f72207a65726f2072657175657360008201527f7400000000000000000000000000000000000000000000000000000000000000602082015250565b7f4f6e6c792061646d696e2026206f776e65722063616e2063616c6c207468697360008201527f2e00000000000000000000000000000000000000000000000000000000000000602082015250565b7f4552433732313a207472616e7366657220746f20746865207a65726f2061646460008201527f7265737300000000000000000000000000000000000000000000000000000000602082015250565b7f4552433732313a20617070726f766520746f2063616c6c657200000000000000600082015250565b7f4e6f7420656e6f75676820657468000000000000000000000000000000000000600082015250565b7f4552433732313a2061646472657373207a65726f206973206e6f74206120766160008201527f6c6964206f776e65720000000000000000000000000000000000000000000000602082015250565b7f7a65726f20726571756573740000000000000000000000000000000000000000600082015250565b7f4552433732313a20617070726f76652063616c6c6572206973206e6f7420746f60008201527f6b656e206f776e6572206e6f7220617070726f76656420666f7220616c6c0000602082015250565b7f4552433732313a206d696e7420746f20746865207a65726f2061646472657373600082015250565b7f2e6a736f6e000000000000000000000000000000000000000000000000000000600082015250565b7f4552433732313a20696e76616c696420746f6b656e2049440000000000000000600082015250565b7f4552433732313a20617070726f76616c20746f2063757272656e74206f776e6560008201527f7200000000000000000000000000000000000000000000000000000000000000602082015250565b7f4552433732313a207472616e736665722063616c6c6572206973206e6f74206f60008201527f776e6572206e6f7220617070726f766564000000000000000000000000000000602082015250565b7f5468652077686974656c6973742073616c65206973206e6f7420656e61626c6560008201527f6421000000000000000000000000000000000000000000000000000000000000602082015250565b7f457863656564206d617820616d6f756e74000000000000000000000000000000600082015250565b7f455243373231456e756d657261626c653a20676c6f62616c20696e646578206f60008201527f7574206f6620626f756e64730000000000000000000000000000000000000000602082015250565b7f746869732061646472657373206973206e6f742077686974656c69737420757360008201527f6572000000000000000000000000000000000000000000000000000000000000602082015250565b7f4552433732313a2063616c6c6572206973206e6f7420746f6b656e206f776e6560008201527f72206e6f7220617070726f766564000000000000000000000000000000000000602082015250565b7f4f6e6c79206f776e65722063616e2063616c6c20746869730000000000000000600082015250565b7f546865207075626c69632073616c65206973206e6f7420656e61626c65642100600082015250565b6148e281614064565b81146148ed57600080fd5b50565b6148f981614076565b811461490457600080fd5b50565b61491081614082565b811461491b57600080fd5b50565b614927816140ce565b811461493257600080fd5b5056fea2646970667358221220b6fc04e218d3be147ab133dae908213028ab7c22b9bd6ac8f57160d1a3590c6364736f6c63430008010033";
	  
	var contractAbi = berith.contract(abi);
	  
	var newc = contractAbi.new(abi,{from:u1,data:object,arguments:["james",20],gas:2000000});
	  
	  
	  
	`)
	if err != nil {
		log.Error("컨트랙트 초기화 오류", "err", err)
	}
	// Initialize the global name register (disabled for now)
	//c.jsre.Run(`var GlobalRegistrar = berith.contract(` + registrar.GlobalRegistrarAbi + `);   registrar = GlobalRegistrar.at("` + registrar.GlobalRegistrarAddr + `");`)

	// If the console is in interactive mode, instrument password related methods to query the user
	if c.prompter != nil {
		// Retrieve the account management object to instrument
		personal, err := c.jsre.Get("personal")
		if err != nil {
			return err
		}
		// Override the openWallet, unlockAccount, newAccount and sign methods since
		// these require user interaction. Assign these method in the Console the
		// original web3 callbacks. These will be called by the jeth.* methods after
		// they got the password from the user and send the original web3 request to
		// the backend.
		if obj := personal.Object(); obj != nil { // make sure the personal api is enabled over the interface
			if _, err = c.jsre.Run(`jeth.openWallet = personal.openWallet;`); err != nil {
				return fmt.Errorf("personal.openWallet: %v", err)
			}
			if _, err = c.jsre.Run(`jeth.unlockAccount = personal.unlockAccount;`); err != nil {
				return fmt.Errorf("personal.unlockAccount: %v", err)
			}
			if _, err = c.jsre.Run(`jeth.newAccount = personal.newAccount;`); err != nil {
				return fmt.Errorf("personal.newAccount: %v", err)
			}
			if _, err = c.jsre.Run(`jeth.sign = personal.sign;`); err != nil {
				return fmt.Errorf("personal.sign: %v", err)
			}
			obj.Set("openWallet", bridge.OpenWallet)
			obj.Set("unlockAccount", bridge.UnlockAccount)
			obj.Set("newAccount", bridge.NewAccount)
			obj.Set("sign", bridge.Sign)
		}
	}
	// The admin.sleep and admin.sleepBlocks are offered by the console and not by the RPC layer.
	admin, err := c.jsre.Get("admin")
	if err != nil {
		return err
	}
	if obj := admin.Object(); obj != nil { // make sure the admin api is enabled over the interface
		obj.Set("sleepBlocks", bridge.SleepBlocks)
		obj.Set("sleep", bridge.Sleep)
		obj.Set("clearHistory", c.clearHistory)
	}
	// Preload any JavaScript files before starting the console
	for _, path := range preload {
		if err := c.jsre.Exec(path); err != nil {
			failure := err.Error()
			if ottoErr, ok := err.(*otto.Error); ok {
				failure = ottoErr.String()
			}
			return fmt.Errorf("%s: %v", path, failure)
		}
	}
	// Configure the console's input prompter for scrollback and tab completion
	if c.prompter != nil {
		if content, err := ioutil.ReadFile(c.histPath); err != nil {
			c.prompter.SetHistory(nil)
		} else {
			c.history = strings.Split(string(content), "\n")
			c.prompter.SetHistory(c.history)
		}
		c.prompter.SetWordCompleter(c.AutoCompleteInput)
	}
	return nil
}

func (c *Console) clearHistory() {
	c.history = nil
	c.prompter.ClearHistory()
	if err := os.Remove(c.histPath); err != nil {
		fmt.Fprintln(c.printer, "can't delete history file:", err)
	} else {
		fmt.Fprintln(c.printer, "history file deleted")
	}
}

// consoleOutput is an override for the console.log and console.error methods to
// stream the output into the configured output stream instead of stdout.
func (c *Console) consoleOutput(call otto.FunctionCall) otto.Value {
	output := []string{}
	for _, argument := range call.ArgumentList {
		output = append(output, fmt.Sprintf("%v", argument))
	}
	fmt.Fprintln(c.printer, strings.Join(output, " "))
	return otto.Value{}
}

// AutoCompleteInput is a pre-assembled word completer to be used by the user
// input prompter to provide hints to the user about the methods available.
func (c *Console) AutoCompleteInput(line string, pos int) (string, []string, string) {
	// No completions can be provided for empty inputs
	if len(line) == 0 || pos == 0 {
		return "", nil, ""
	}
	// Chunck data to relevant part for autocompletion
	// E.g. in case of nested lines berith.getBalance(berith.coinb<tab><tab>
	start := pos - 1
	for ; start > 0; start-- {
		// Skip all methods and namespaces (i.e. including the dot)
		if line[start] == '.' || (line[start] >= 'a' && line[start] <= 'z') || (line[start] >= 'A' && line[start] <= 'Z') {
			continue
		}
		// Handle web3 in a special way (i.e. other numbers aren't auto completed)
		if start >= 3 && line[start-3:start] == "web3" {
			start -= 3
			continue
		}
		// We've hit an unexpected character, autocomplete form here
		start++
		break
	}
	return line[:start], c.jsre.CompleteKeywords(line[start:pos]), line[pos:]
}

// Welcome show summary of current Geth instance and some metadata about the
// console's available modules.
func (c *Console) Welcome() {
	// Print some generic Geth metadata
	fmt.Fprintf(c.printer, "Welcome to the Berith JavaScript console!\n\n")
	c.jsre.Run(`
		console.log("instance: " + web3.version.node);
		console.log("coinbase: " + berith.coinbase);
		console.log("at block: " + berith.blockNumber + " (" + new Date(1000 * berith.getBlock(berith.blockNumber).timestamp) + ")");
		console.log(" datadir: " + admin.datadir);
	`)
	// List all the supported modules for the user to call
	if apis, err := c.client.SupportedModules(); err == nil {
		modules := make([]string, 0, len(apis))
		for api, version := range apis {
			modules = append(modules, fmt.Sprintf("%s:%s", api, version))
		}
		sort.Strings(modules)
		fmt.Fprintln(c.printer, " modules:", strings.Join(modules, " "))
	}
	fmt.Fprintln(c.printer)
}

// Evaluate executes code and pretty prints the result to the specified output
// stream.
func (c *Console) Evaluate(statement string) error {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(c.printer, "[native] error: %v\n", r)
		}
	}()
	return c.jsre.Evaluate(statement, c.printer)
}

// Interactive starts an interactive user session, where input is propted from
// the configured user prompter.
func (c *Console) Interactive() {
	var (
		prompt    = c.prompt          // Current prompt line (used for multi-line inputs)
		indents   = 0                 // Current number of input indents (used for multi-line inputs)
		input     = ""                // Current user input
		scheduler = make(chan string) // Channel to send the next prompt on and receive the input
	)
	// Start a goroutine to listen for prompt requests and send back inputs
	go func() {
		for {
			// Read the next user input
			line, err := c.prompter.PromptInput(<-scheduler)
			if err != nil {
				log.Error("Console Error ! ", "Err", err)
				// In case of an error, either clear the prompt or fail
				if err == liner.ErrPromptAborted { // ctrl-C
					prompt, indents, input = c.prompt, 0, ""
					scheduler <- ""
					continue
				}
				close(scheduler)
				return
			}
			// User input retrieved, send for interpretation and loop
			scheduler <- line
		}
	}()
	// Monitor Ctrl-C too in case the input is empty and we need to bail
	abort := make(chan os.Signal, 1)
	signal.Notify(abort, syscall.SIGINT, syscall.SIGTERM)

	// Start sending prompts to the user and reading back inputs
	for {
		// Send the next prompt, triggering an input read and process the result
		scheduler <- prompt
		select {
		case <-abort:
			// User forcefully quite the console
			fmt.Fprintln(c.printer, "caught interrupt, exiting")
			return

		case line, ok := <-scheduler:
			// User input was returned by the prompter, handle special cases
			if !ok || (indents <= 0 && exit.MatchString(line)) {
				return
			}
			if onlyWhitespace.MatchString(line) {
				continue
			}
			// Append the line to the input and check for multi-line interpretation
			input += line + "\n"

			indents = countIndents(input)
			if indents <= 0 {
				prompt = c.prompt
			} else {
				prompt = strings.Repeat(".", indents*3) + " "
			}
			// If all the needed lines are present, save the command and run
			if indents <= 0 {
				if len(input) > 0 && input[0] != ' ' && !passwordRegexp.MatchString(input) {
					if command := strings.TrimSpace(input); len(c.history) == 0 || command != c.history[len(c.history)-1] {
						c.history = append(c.history, command)
						if c.prompter != nil {
							c.prompter.AppendHistory(command)
						}
					}
				}
				c.Evaluate(input)
				input = ""
			}
		}
	}
}

// countIndents returns the number of identations for the given input.
// In case of invalid input such as var a = } the result can be negative.
func countIndents(input string) int {
	var (
		indents     = 0
		inString    = false
		strOpenChar = ' '   // keep track of the string open char to allow var str = "I'm ....";
		charEscaped = false // keep track if the previous char was the '\' char, allow var str = "abc\"def";
	)

	for _, c := range input {
		switch c {
		case '\\':
			// indicate next char as escaped when in string and previous char isn't escaping this backslash
			if !charEscaped && inString {
				charEscaped = true
			}
		case '\'', '"':
			if inString && !charEscaped && strOpenChar == c { // end string
				inString = false
			} else if !inString && !charEscaped { // begin string
				inString = true
				strOpenChar = c
			}
			charEscaped = false
		case '{', '(':
			if !inString { // ignore brackets when in string, allow var str = "a{"; without indenting
				indents++
			}
			charEscaped = false
		case '}', ')':
			if !inString {
				indents--
			}
			charEscaped = false
		default:
			charEscaped = false
		}
	}

	return indents
}

// Execute runs the JavaScript file specified as the argument.
func (c *Console) Execute(path string) error {
	return c.jsre.Exec(path)
}

// Stop cleans up the console and terminates the runtime environment.
func (c *Console) Stop(graceful bool) error {
	if err := ioutil.WriteFile(c.histPath, []byte(strings.Join(c.history, "\n")), 0600); err != nil {
		return err
	}
	if err := os.Chmod(c.histPath, 0600); err != nil { // Force 0600, even if it was different previously
		return err
	}
	c.jsre.Stop(graceful)
	return nil
}
